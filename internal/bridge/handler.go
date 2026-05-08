package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/agent"
	"github.com/p4u/claude-proxy/internal/mcpbridge"
	"github.com/p4u/claude-proxy/internal/proxy"
	"github.com/p4u/claude-proxy/internal/router"
	"github.com/p4u/claude-proxy/internal/store"
)

const maxBodyBytes = 16 << 20

// Handler is the HTTP handler for /api/v1/* endpoints.
type Handler struct {
	mgr      *agent.Manager
	upstream *proxy.Handler
	db       *store.DB
	log      *slog.Logger
}

// New returns a new bridge Handler.
func New(mgr *agent.Manager, upstream *proxy.Handler, db *store.DB, log *slog.Logger) *Handler {
	return &Handler{mgr: mgr, upstream: upstream, db: db, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api")
	if path == "" {
		path = "/"
	}

	// CORS preflight
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers",
			"anthropic-version, anthropic-beta, x-api-key, authorization, content-type")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")

	switch {
	case path == "/v1/messages" && r.Method == http.MethodPost:
		h.serveMessages(w, r)
	case path == "/v1/messages/count_tokens" && r.Method == http.MethodPost:
		h.forwardUpstream(w, r)
	case path == "/v1/models" && r.Method == http.MethodGet:
		h.forwardUpstream(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found_error",
			"The requested resource does not exist.")
	}
}

func (h *Handler) serveMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}
	if len(body) > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
		return
	}

	var req MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	if req.MaxTokens == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "max_tokens is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages must not be empty")
		return
	}

	if v := r.Header.Get("Anthropic-Version"); v != "" {
		w.Header().Set("Anthropic-Version", v)
	}

	dr := router.Derive(r, body)
	convKey := dr.ConvID

	h.log.Info("api request",
		"conv", convKey, "src", string(dr.Source),
		"model", req.Model, "stream", req.Stream,
		"tools", len(req.Tools), "msgs", len(req.Messages))

	// Build MCP tool catalog.
	var mcpTools []mcpbridge.Tool
	clientToolSet := make(map[string]bool)
	for _, t := range req.Tools {
		mcpTools = append(mcpTools, mcpbridge.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
		clientToolSet[t.Name] = true
	}

	sess, err := h.mgr.GetOrCreate(r.Context(), convKey, mcpTools)
	if err != nil {
		h.log.Error("get/create session", "err", err, "conv", convKey)
		writeError(w, http.StatusServiceUnavailable, "api_error",
			"could not start agent session: "+err.Error())
		return
	}

	// Serialise turns within a session.
	sess.TurnMu.Lock()
	defer sess.TurnMu.Unlock()

	// Deliver tool results from the client via MCP.
	toolResults := extractToolResults(req.Messages)
	for _, tr := range toolResults {
		if mcp := sess.MCP(); mcp != nil {
			// tool_result blocks carry tool_use_id, not the tool name.
			// Look up the name from the preceding assistant message.
			name := ResolveToolName(req.Messages, tr.ToolUseID)
			if name == "" {
				h.log.Warn("could not resolve tool name", "tool_use_id", tr.ToolUseID)
				continue
			}
			content := toolResultContent(tr.Content)
			mcp.ResolveByName(name, mcpbridge.ToolResult{
				Content: content,
				IsError: tr.IsError,
			})
			h.log.Debug("resolved tool result", "name", name, "tool_use_id", tr.ToolUseID)
		}
	}

	onlyToolResults := hasOnlyToolResults(req.Messages)
	if !sess.IsAlive() {
		writeError(w, http.StatusServiceUnavailable, "api_error", "session not alive")
		return
	}

	var eventCh <-chan agent.ClaudeEvent

	if onlyToolResults {
		// MCP results have been delivered; agent is already running.
		// Just read from the ongoing event stream.
		eventCh, err = sess.Events()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "api_error", "session error: "+err.Error())
			return
		}
	} else {
		newContent := extractNewUserContent(req.Messages)
		if newContent == nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				"could not extract user message content")
			return
		}
		msgLine, err2 := agent.BuildUserMessage(newContent)
		if err2 != nil {
			writeError(w, http.StatusInternalServerError, "api_error", "build message: "+err2.Error())
			return
		}
		eventCh, err = sess.Send(msgLine)
		if err != nil {
			h.log.Warn("send failed; respawning", "err", err, "conv", convKey)
			if respErr := sess.Respawn(r.Context()); respErr != nil {
				writeError(w, http.StatusServiceUnavailable, "api_error", "session error: "+err.Error())
				return
			}
			eventCh, err = sess.Send(msgLine)
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "api_error", "session error: "+err.Error())
				return
			}
		}
	}

	model := req.Model
	if model == "" {
		model = "claude-3-5-sonnet-20241022"
	}
	msgID := newMsgID()

	var costUSD float64
	if req.Stream {
		costUSD = h.streamResponse(w, r.Context(), eventCh, msgID, model, clientToolSet)
	} else {
		costUSD = h.collectResponse(w, r.Context(), eventCh, msgID, model, clientToolSet)
	}

	_ = store.BumpAgentSession(r.Context(), h.db, convKey, costUSD)
}

func (h *Handler) streamResponse(
	w http.ResponseWriter,
	ctx context.Context,
	events <-chan agent.ClaudeEvent,
	msgID, model string,
	clientTools map[string]bool,
) float64 {
	initSSE(w, model)

	writeSSEEvent(w, "message_start", buildMessageStart(msgID, model, 0))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	tracker := newIndexTracker()
	pingTicker := time.NewTicker(10 * time.Second)
	defer pingTicker.Stop()
	var costUSD float64

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				sseError(w, "api_error", "agent subprocess exited unexpectedly")
				return costUSD
			}
			done, cost := h.handleEvent(w, ev, tracker, clientTools, true)
			costUSD += cost
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if done {
				return costUSD
			}
		case <-pingTicker.C:
			writePing(w)
		case <-ctx.Done():
			return costUSD
		}
	}
}

func (h *Handler) collectResponse(
	w http.ResponseWriter,
	ctx context.Context,
	events <-chan agent.ClaudeEvent,
	msgID, model string,
	clientTools map[string]bool,
) float64 {
	var textBuf strings.Builder
	var toolUseBlocks []ContentBlock
	var toolUseOutIdxs []int // parallel to toolUseBlocks: output index for each tool_use block
	var inputTokens, outputTokens int
	stopReason := "end_turn"
	tracker := newIndexTracker()
	var costUSD float64

	// Track per-block accumulated tool inputs for non-streaming.
	toolInputs := make(map[int]*strings.Builder) // outIdx → input json fragments

loop:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				break loop
			}
			switch ev.Type {
			case "stream_event":
				var inner map[string]json.RawMessage
				if json.Unmarshal(ev.Event, &inner) != nil {
					continue
				}
				var typStr string
				_ = json.Unmarshal(inner["type"], &typStr)

				switch typStr {
				case "content_block_start":
					var idx int
					_ = json.Unmarshal(inner["index"], &idx)
					var cb map[string]json.RawMessage
					_ = json.Unmarshal(inner["content_block"], &cb)
					var cbType, cbName, cbID string
					_ = json.Unmarshal(cb["type"], &cbType)
					_ = json.Unmarshal(cb["name"], &cbName)
					_ = json.Unmarshal(cb["id"], &cbID)
					swallow := cbType == "tool_use" && !clientTools[cbName]
					outIdx, sw := tracker.register(idx, swallow)
					if !sw && cbType == "tool_use" {
						toolUseBlocks = append(toolUseBlocks, ContentBlock{
							Type: "tool_use",
							ID:   cbID,
							Name: cbName,
						})
						toolUseOutIdxs = append(toolUseOutIdxs, outIdx)
						toolInputs[outIdx] = &strings.Builder{}
					}

				case "content_block_delta":
					var idx int
					_ = json.Unmarshal(inner["index"], &idx)
					outIdx, ok2 := tracker.claudeToOut[idx]
					if !ok2 || outIdx == -1 {
						continue
					}
					var delta struct {
						Type        string `json:"type"`
						Text        string `json:"text"`
						PartialJSON string `json:"partial_json"`
					}
					_ = json.Unmarshal(inner["delta"], &delta)
					switch delta.Type {
					case "text_delta":
						textBuf.WriteString(delta.Text)
					case "input_json_delta":
						if b, ok3 := toolInputs[outIdx]; ok3 {
							b.WriteString(delta.PartialJSON)
						}
					}

				case "message_delta":
					var md struct {
						Delta struct {
							StopReason string `json:"stop_reason"`
						} `json:"delta"`
						Usage struct {
							OutputTokens int `json:"output_tokens"`
						} `json:"usage"`
					}
					_ = json.Unmarshal(ev.Event, &md)
					if md.Delta.StopReason != "" {
						stopReason = md.Delta.StopReason
					}
					outputTokens = md.Usage.OutputTokens

				case "message_start":
					var ms struct {
						Message struct {
							Usage struct {
								InputTokens int `json:"input_tokens"`
							} `json:"usage"`
						} `json:"message"`
					}
					_ = json.Unmarshal(ev.Event, &ms)
					inputTokens = ms.Message.Usage.InputTokens
				}

			case "result":
				costUSD = ev.TotalCostUSD
				break loop
			}

		case <-ctx.Done():
			break loop
		}
	}

	// Finalise tool input JSON. toolUseOutIdxs[i] is the output block index
	// for toolUseBlocks[i]; toolInputs is keyed by output index, not position.
	for i := range toolUseBlocks {
		outIdx := toolUseOutIdxs[i]
		if b, ok := toolInputs[outIdx]; ok && b.Len() > 0 {
			toolUseBlocks[i].Input = json.RawMessage(b.String())
		}
	}

	content := []ContentBlock{}
	if textBuf.Len() > 0 {
		content = append(content, ContentBlock{Type: "text", Text: textBuf.String()})
	}
	content = append(content, toolUseBlocks...)

	resp := MessagesResponse{
		ID:         msgID,
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		Model:      model,
		StopReason: stopReason,
		Usage:      Usage{InputTokens: inputTokens, OutputTokens: outputTokens},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
	return costUSD
}

// handleEvent processes one ClaudeEvent. Returns (done=true) when the turn is
// complete. For streaming mode it also writes SSE events.
func (h *Handler) handleEvent(
	w http.ResponseWriter,
	ev agent.ClaudeEvent,
	tracker *indexTracker,
	clientTools map[string]bool,
	streaming bool,
) (done bool, cost float64) {
	switch ev.Type {
	case "system":
		if ev.Subtype == "api_retry" && streaming {
			writePing(w)
		}

	case "stream_event":
		evtName, data, stop, _ := translateStreamEvent(ev.Event, tracker, clientTools)
		if evtName != "" && data != nil && streaming {
			writeSSEEvent(w, evtName, data)
		}
		if stop {
			return true, 0
		}

	case "result":
		if ev.IsError && streaming {
			sseError(w, "api_error", "agent error: "+ev.Result)
		}
		return true, ev.TotalCostUSD
	}
	return false, 0
}

// forwardUpstream rewrites the path from /api/v1/... to /v1/... and proxies
// to api.anthropic.com via the existing proxy handler.
func (h *Handler) forwardUpstream(w http.ResponseWriter, r *http.Request) {
	r2 := r.Clone(r.Context())
	r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
	if r2.URL.Path == "" {
		r2.URL.Path = "/"
	}
	r2.RequestURI = r2.URL.RequestURI()
	h.upstream.ServeHTTP(w, r2)
}

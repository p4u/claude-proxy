// Package mcpbridge implements a per-session MCP HTTP server (Streamable HTTP
// transport) that exposes client-defined tools to the claude CLI subprocess.
// When the agent calls a client tool, the call is parked here until the
// bridge handler feeds the tool_result from the next HTTP request.
package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Tool mirrors the Anthropic API tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolResult is the outcome of a client-executed tool.
type ToolResult struct {
	Content string
	IsError bool
}

// PendingCall represents a tool call the agent made that is waiting for the
// client to execute and return a result.
type PendingCall struct {
	ToolUseID string
	Name      string
	Input     json.RawMessage
	result    chan ToolResult
}

// Server is an MCP HTTP server bound to a local port for one agent session.
type Server struct {
	mu      sync.RWMutex
	tools   []Tool
	pending map[string]*PendingCall // tool_use_id → pending call
	addr    string
	srv     *http.Server
	ln      net.Listener
}

// New starts a new MCP server on a random local port and returns it.
func New() (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{
		pending: make(map[string]*PendingCall),
		addr:    ln.Addr().String(),
		ln:      ln,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleMCP)
	s.srv = &http.Server{Handler: mux}
	go s.srv.Serve(ln) //nolint:errcheck
	return s, nil
}

// URL returns the base URL clients should point at.
func (s *Server) URL() string { return "http://" + s.addr }

// Addr returns the TCP address the server is listening on.
func (s *Server) Addr() string { return s.addr }

// UpdateTools replaces the tool catalog for this session.
func (s *Server) UpdateTools(tools []Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = tools
}

// HasTool reports whether a named tool is in the current catalog.
func (s *Server) HasTool(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// Resolve delivers a tool result to a parked call, unblocking the agent.
// Returns false if no matching call is found.
func (s *Server) Resolve(toolUseID string, result ToolResult) bool {
	s.mu.Lock()
	pc, ok := s.pending[toolUseID]
	if ok {
		delete(s.pending, toolUseID)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	pc.result <- result
	return true
}

// Close shuts down the MCP server and cancels all pending calls with an error.
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
	s.mu.Lock()
	for id, pc := range s.pending {
		pc.result <- ToolResult{Content: "server closed", IsError: true}
		delete(s.pending, id)
	}
	s.mu.Unlock()
}

// --- JSON-RPC types ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func errResp(id any, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, errResp(nil, -32700, "parse error"))
		return
	}

	var resp rpcResponse
	switch req.Method {
	case "initialize":
		resp = rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "claude-proxy-mcp", "version": "1.0"},
		}}
	case "notifications/initialized":
		w.WriteHeader(http.StatusNoContent)
		return
	case "ping":
		resp = rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	case "tools/list":
		s.mu.RLock()
		tools := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			tools = append(tools, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		s.mu.RUnlock()
		resp = rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": tools}}
	case "tools/call":
		resp = s.handleToolCall(r.Context(), req)
	default:
		resp = errResp(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}

	writeJSON(w, resp)
}

func (s *Server) handleToolCall(ctx context.Context, req rpcRequest) rpcResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errResp(req.ID, -32602, "invalid params")
	}

	// Mint a tool_use_id. In practice the agent supplies one via the stream;
	// here we use the tool name + timestamp as a stable key if not available.
	// The real tool_use_id comes from the stream_event before this call, so
	// the bridge has already told us what to expect.
	toolUseID := fmt.Sprintf("mcp_%s_%d", params.Name, time.Now().UnixNano())

	pc := &PendingCall{
		ToolUseID: toolUseID,
		Name:      params.Name,
		Input:     params.Arguments,
		result:    make(chan ToolResult, 1),
	}

	s.mu.Lock()
	// Register under both the generated ID and the name so the bridge can match.
	s.pending[toolUseID] = pc
	// Also register under name for lookup by the bridge when it sees tool_use blocks.
	s.pending["name:"+params.Name] = pc
	s.mu.Unlock()

	// Wait for the bridge to deliver the result (or context cancellation).
	select {
	case result := <-pc.result:
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": result.Content}},
			"isError": result.IsError,
		}}
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, toolUseID)
		delete(s.pending, "name:"+params.Name)
		s.mu.Unlock()
		return errResp(req.ID, -32603, "context cancelled")
	}
}

// PendingByName returns the pending call registered under a tool name (if any).
// Used by the bridge to match stream_event tool_use_id to the MCP call.
func (s *Server) PendingByName(name string) *PendingCall {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pending["name:"+name]
}

// ResolveByName delivers a result to the pending call for a given tool name.
func (s *Server) ResolveByName(name string, result ToolResult) bool {
	s.mu.Lock()
	pc, ok := s.pending["name:"+name]
	if ok {
		delete(s.pending, "name:"+name)
		if pc.ToolUseID != "" {
			delete(s.pending, pc.ToolUseID)
		}
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	pc.result <- result
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

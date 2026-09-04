package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"
)

// translateAnthropicToOpenAI converts the subset of the Anthropic Messages
// request used by Claude Code into the widely implemented OpenAI Chat
// Completions shape. Unknown Anthropic-only controls are intentionally omitted
// instead of leaking fields that strict compatibility servers reject.
func translateAnthropicToOpenAI(raw []byte) ([]byte, bool, error) {
	var in map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&in); err != nil {
		return nil, false, fmt.Errorf("decode Anthropic request: %w", err)
	}
	out := map[string]any{}
	for _, key := range []string{"model", "temperature", "top_p", "stream", "user"} {
		if v, ok := in[key]; ok {
			out[key] = v
		}
	}
	if v, ok := in["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := in["stop_sequences"]; ok {
		out["stop"] = v
	}

	messages := make([]any, 0)
	if system := textFromContent(in["system"]); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	if rawMessages, ok := in["messages"].([]any); ok {
		for _, item := range rawMessages {
			m, _ := item.(map[string]any)
			messages = append(messages, openAIMessages(m)...)
		}
	}
	out["messages"] = messages

	if tools, ok := in["tools"].([]any); ok && len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, item := range tools {
			t, _ := item.(map[string]any)
			name, _ := t["name"].(string)
			if name == "" {
				continue
			}
			fn := map[string]any{"name": name, "parameters": t["input_schema"]}
			if desc, _ := t["description"].(string); desc != "" {
				fn["description"] = desc
			}
			converted = append(converted, map[string]any{"type": "function", "function": fn})
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}
	if choice, ok := in["tool_choice"].(map[string]any); ok {
		switch choice["type"] {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			if name, _ := choice["name"].(string); name != "" {
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
	}
	stream, _ := out["stream"].(bool)
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, false, fmt.Errorf("encode OpenAI request: %w", err)
	}
	return encoded, stream, nil
}

func openAIMessages(in map[string]any) []any {
	role, _ := in["role"].(string)
	content := in["content"]
	if role == "assistant" {
		msg := map[string]any{"role": "assistant"}
		var texts []string
		var calls []any
		for _, b := range contentBlocks(content) {
			switch b["type"] {
			case "text":
				if s, _ := b["text"].(string); s != "" {
					texts = append(texts, s)
				}
			case "tool_use":
				args, _ := json.Marshal(b["input"])
				calls = append(calls, map[string]any{
					"id": b["id"], "type": "function",
					"function": map[string]any{"name": b["name"], "arguments": string(args)},
				})
			}
		}
		if len(texts) > 0 {
			msg["content"] = strings.Join(texts, "")
		} else {
			msg["content"] = nil
		}
		if len(calls) > 0 {
			msg["tool_calls"] = calls
		}
		return []any{msg}
	}

	// Anthropic tool_result blocks live inside a user message; Chat Completions
	// represents each as a standalone role=tool message. Preserve surrounding
	// user content by flushing it on either side of those blocks.
	var out []any
	var parts []any
	flush := func() {
		if len(parts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": parts})
			parts = nil
		}
	}
	for _, b := range contentBlocks(content) {
		switch b["type"] {
		case "tool_result":
			flush()
			out = append(out, map[string]any{
				"role": "tool", "tool_call_id": b["tool_use_id"],
				"content": textFromContent(b["content"]),
			})
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": b["text"]})
		case "image":
			if part := openAIImage(b); part != nil {
				parts = append(parts, part)
			}
		}
	}
	flush()
	if len(out) == 0 {
		out = append(out, map[string]any{"role": "user", "content": textFromContent(content)})
	}
	return out
}

func contentBlocks(content any) []map[string]any {
	if s, ok := content.(string); ok {
		return []map[string]any{{"type": "text", "text": s}}
	}
	items, _ := content.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if block, ok := item.(map[string]any); ok {
			out = append(out, block)
		}
	}
	return out
}

func textFromContent(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	var texts []string
	for _, b := range contentBlocks(content) {
		if b["type"] == "text" {
			if s, _ := b["text"].(string); s != "" {
				texts = append(texts, s)
			}
		}
	}
	if len(texts) > 0 {
		return strings.Join(texts, "")
	}
	if content != nil {
		if raw, err := json.Marshal(content); err == nil {
			return string(raw)
		}
	}
	return ""
}

func openAIImage(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	typ, _ := source["type"].(string)
	var imageURL string
	switch typ {
	case "base64":
		media, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if media != "" && data != "" {
			imageURL = "data:" + media + ";base64," + data
		}
	case "url":
		imageURL, _ = source["url"].(string)
	}
	if imageURL == "" {
		return nil
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}}
}

func translateOpenAIJSON(raw []byte) ([]byte, error) {
	var in struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   any    `json:"content"`
				Refusal   string `json:"refusal"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("decode OpenAI response: %w", err)
	}
	if len(in.Choices) == 0 {
		return nil, fmt.Errorf("OpenAI response contains no choices")
	}
	choice := in.Choices[0]
	content := make([]any, 0, 1+len(choice.Message.ToolCalls))
	text := openAIResponseText(choice.Message.Content)
	if text == "" {
		text = choice.Message.Refusal
	}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, call := range choice.Message.ToolCalls {
		var input any = map[string]any{}
		if strings.TrimSpace(call.Function.Arguments) != "" {
			if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
				input = map[string]any{"_raw": call.Function.Arguments}
			}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input,
		})
	}
	out := map[string]any{
		"id": in.ID, "type": "message", "role": "assistant", "content": content,
		"model": in.Model, "stop_reason": anthropicStopReason(choice.FinishReason),
		"stop_sequence": nil, "usage": anthropicUsage(in.Usage),
	}
	return json.Marshal(out)
}

func openAIResponseText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	items, _ := content.([]any)
	var out []string
	for _, item := range items {
		part, _ := item.(map[string]any)
		if typ, _ := part["type"].(string); typ == "text" || typ == "output_text" {
			if s, _ := part["text"].(string); s != "" {
				out = append(out, s)
			}
		}
	}
	return strings.Join(out, "")
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	PromptDetails    struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func anthropicUsage(u openAIUsage) map[string]any {
	return map[string]any{
		"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens,
		"cache_read_input_tokens":     u.PromptDetails.CachedTokens,
		"cache_creation_input_tokens": 0,
	}
}

func anthropicStopReason(reason string) any {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "stop", "content_filter":
		return "end_turn"
	default:
		return nil
	}
}

func translateOpenAIError(raw []byte, status int) []byte {
	var in struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &in)
	message := strings.TrimSpace(in.Error.Message)
	if message == "" {
		message = strings.TrimSpace(string(raw))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	typ := "api_error"
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		typ = "authentication_error"
	case http.StatusTooManyRequests:
		typ = "rate_limit_error"
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		typ = "invalid_request_error"
	}
	out, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": message}})
	return out
}

// translateOpenAIStream exposes a live Anthropic SSE stream while consuming
// OpenAI chat.completion.chunk events. The pipe preserves backpressure and
// cancellation; it does not buffer a completion before returning it.
func translateOpenAIStream(ctx context.Context, src io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer src.Close()
		err := writeAnthropicStream(ctx, pw, src)
		_ = pw.CloseWithError(err)
	}()
	return pr
}

type streamTool struct {
	ID   string
	Name string
	Args strings.Builder
}

func writeAnthropicStream(ctx context.Context, dst io.Writer, src io.Reader) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	started := false
	textOpen := false
	textBlock := -1
	nextBlock := 0
	tools := map[int]*streamTool{}
	finish := ""
	usage := openAIUsage{}
	id, model := "", ""
	emit := func(event string, payload any) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(dst, "event: %s\ndata: %s\n\n", event, raw)
		return err
	}
	start := func() error {
		if started {
			return nil
		}
		started = true
		return emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "content": []any{},
			"model": model, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		}})
	}
	closeText := func() error {
		if !textOpen {
			return nil
		}
		textOpen = false
		return emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": textBlock})
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Refusal   string `json:"refusal"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage openAIUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("decode OpenAI stream chunk: %w", err)
		}
		if id == "" {
			id = chunk.ID
		}
		if model == "" {
			model = chunk.Model
		}
		if err := start(); err != nil {
			return err
		}
		if chunk.Usage.PromptTokens != 0 || chunk.Usage.CompletionTokens != 0 {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		deltaText := choice.Delta.Content
		if deltaText == "" {
			deltaText = choice.Delta.Refusal
		}
		if deltaText != "" {
			if !textOpen {
				textOpen = true
				textBlock = nextBlock
				nextBlock++
				if err := emit("content_block_start", map[string]any{"type": "content_block_start", "index": textBlock, "content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
					return err
				}
			}
			if err := emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": textBlock, "delta": map[string]any{"type": "text_delta", "text": deltaText}}); err != nil {
				return err
			}
		}
		for _, call := range choice.Delta.ToolCalls {
			t := tools[call.Index]
			if t == nil {
				t = &streamTool{ID: call.ID, Name: call.Function.Name}
				tools[call.Index] = t
			}
			if call.ID != "" {
				t.ID = call.ID
			}
			if call.Function.Name != "" {
				t.Name = call.Function.Name
			}
			t.Args.WriteString(call.Function.Arguments)
		}
		if choice.FinishReason != "" {
			finish = choice.FinishReason
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := start(); err != nil {
		return err
	}
	if err := closeText(); err != nil {
		return err
	}
	toolIndexes := make([]int, 0, len(tools))
	for index := range tools {
		toolIndexes = append(toolIndexes, index)
	}
	sort.Ints(toolIndexes)
	for _, toolIndex := range toolIndexes {
		t := tools[toolIndex]
		block := nextBlock
		nextBlock++
		if err := emit("content_block_start", map[string]any{"type": "content_block_start", "index": block, "content_block": map[string]any{"type": "tool_use", "id": t.ID, "name": t.Name, "input": map[string]any{}}}); err != nil {
			return err
		}
		if t.Args.Len() > 0 {
			if err := emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": block, "delta": map[string]any{"type": "input_json_delta", "partial_json": t.Args.String()}}); err != nil {
				return err
			}
		}
		if err := emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": block}); err != nil {
			return err
		}
	}
	if err := emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": anthropicStopReason(finish), "stop_sequence": nil}, "usage": anthropicUsage(usage)}); err != nil {
		return err
	}
	return emit("message_stop", map[string]any{"type": "message_stop"})
}

// approximateAnthropicTokens is a conservative local fallback for
// /v1/messages/count_tokens because Chat Completions defines no equivalent.
// It is intentionally labelled approximate in code; its purpose is Claude
// Code context preflight, not billing or accounting.
func approximateAnthropicTokens(raw []byte) int64 {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return max(1, int64(len(raw)+3)/4)
	}
	chars := countStringRunes(value)
	return max(1, int64(chars+3)/4+8)
}

func countStringRunes(v any) int {
	switch x := v.(type) {
	case string:
		return utf8.RuneCountInString(x)
	case []any:
		n := 0
		for _, item := range x {
			n += countStringRunes(item)
		}
		return n
	case map[string]any:
		n := 0
		for _, item := range x {
			n += countStringRunes(item)
		}
		return n
	default:
		return 0
	}
}

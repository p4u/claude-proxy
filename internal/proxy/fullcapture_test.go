package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// postAs sends a /v1/messages request carrying the given identity.
func postAs(t *testing.T, h *Handler, id *usertoken.Identity, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(body)))
	if id != nil {
		req = req.WithContext(usertoken.WithIdentity(req.Context(), id))
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("status=%d body=%s", rw.Code, rw.Body.String())
	}
	return rw
}

type storedMsg struct {
	seq     int
	role    string
	content string
	model   string
}

// storedMessages returns every captured message in the test DB (each test uses
// a fresh DB holding a single conversation). The stored conv_id is the
// router-derived key, not the raw metadata.user_id.
func storedMessages(t *testing.T, db *store.DB) []storedMsg {
	t.Helper()
	rows, err := db.Query(
		`SELECT seq, role, content, model FROM conversation_message ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []storedMsg
	for rows.Next() {
		var m storedMsg
		if err := rows.Scan(&m.seq, &m.role, &m.content, &m.model); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// fullCaptureUser creates a user token row so conversation_message's FK holds,
// and returns the matching identity.
func fullCaptureUser(t *testing.T, db *store.DB, name string) *usertoken.Identity {
	t.Helper()
	ut, err := usertoken.Create(context.Background(), db, name)
	if err != nil {
		t.Fatal(err)
	}
	return &usertoken.Identity{UserTokenID: ut.ID, UserName: ut.Name, FullCapture: true}
}

// jsonUpstream replies with a non-streaming Messages response.
func jsonUpstream(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintf(w, `{"id":"msg_1","type":"message","model":"claude-test",
			"content":[{"type":"text","text":%q}],
			"usage":{"input_tokens":5,"output_tokens":3}}`, text)
	}
}

func TestFullCaptureSeqAndIdempotency(t *testing.T) {
	h, _, db, _ := setupProxy(t, jsonUpstream("reply one"))
	h.PromptRetentionDays = 7
	id := fullCaptureUser(t, db, "alice")

	// Turn 1.
	postAs(t, h, id, `{"model":"claude-test","metadata":{"user_id":"conv-A"},
		"messages":[{"role":"user","content":"q1"}]}`)

	got := storedMessages(t, db)
	if len(got) != 2 {
		t.Fatalf("after turn 1: %d rows, want 2 (user + assistant): %+v", len(got), got)
	}
	if got[0].seq != 0 || got[0].role != "user" || got[0].content != "q1" {
		t.Fatalf("row 0 = %+v", got[0])
	}
	if got[1].seq != 1 || got[1].role != "assistant" || got[1].content != "reply one" {
		t.Fatalf("row 1 = %+v", got[1])
	}
	if got[1].model != "claude-test" {
		t.Fatalf("assistant model = %q", got[1].model)
	}

	// Turn 2 re-sends the whole history (stateless API) plus a new user turn.
	postAs(t, h, id, `{"model":"claude-test","metadata":{"user_id":"conv-A"},
		"messages":[{"role":"user","content":"q1"},
		            {"role":"assistant","content":"reply one"},
		            {"role":"user","content":"q2"}]}`)

	got = storedMessages(t, db)
	if len(got) != 4 {
		t.Fatalf("after turn 2: %d rows, want 4: %+v", len(got), got)
	}
	if got[2].seq != 2 || got[2].content != "q2" {
		t.Fatalf("row 2 = %+v", got[2])
	}
	if got[3].seq != 3 || got[3].role != "assistant" {
		t.Fatalf("row 3 = %+v", got[3])
	}

	// Replaying the exact same request must not duplicate or shift anything.
	postAs(t, h, id, `{"model":"claude-test","metadata":{"user_id":"conv-A"},
		"messages":[{"role":"user","content":"q1"},
		            {"role":"assistant","content":"reply one"},
		            {"role":"user","content":"q2"}]}`)
	again := storedMessages(t, db)
	if len(again) != 4 {
		t.Fatalf("replay changed row count: %d, want 4: %+v", len(again), again)
	}
	for i := range got {
		if got[i] != again[i] {
			t.Fatalf("replay mutated row %d: %+v -> %+v", i, got[i], again[i])
		}
	}
}

func TestFullCaptureConcatenatesTextBlocksSkipsToolBlocks(t *testing.T) {
	h, _, db, _ := setupProxy(t, jsonUpstream("ok"))
	h.PromptRetentionDays = 7
	id := fullCaptureUser(t, db, "bob")

	postAs(t, h, id, `{"model":"claude-test","metadata":{"user_id":"conv-B"},"messages":[
		{"role":"user","content":[
			{"type":"text","text":"first"},
			{"type":"image","source":{}},
			{"type":"tool_result","tool_use_id":"t1","content":"secret"},
			{"type":"text","text":"second"}]}]}`)

	got := storedMessages(t, db)
	if len(got) == 0 {
		t.Fatal("nothing captured")
	}
	if got[0].content != "first\n\nsecond" {
		t.Fatalf("content = %q, want both text blocks concatenated", got[0].content)
	}
	if strings.Contains(got[0].content, "secret") {
		t.Fatal("tool_result payload leaked into stored content")
	}
}

func TestFullCaptureTruncatesLongMessages(t *testing.T) {
	h, _, db, _ := setupProxy(t, jsonUpstream(strings.Repeat("b", maxMessageRunes+500)))
	h.PromptRetentionDays = 7
	id := fullCaptureUser(t, db, "carol")

	big := strings.Repeat("a", maxMessageRunes+1000)
	postAs(t, h, id, `{"model":"claude-test","metadata":{"user_id":"conv-C"},
		"messages":[{"role":"user","content":"`+big+`"}]}`)

	got := storedMessages(t, db)
	if len(got) != 2 {
		t.Fatalf("rows=%d, want 2: %+v", len(got), got)
	}
	for _, m := range got {
		if !strings.HasSuffix(m.content, truncMarker) {
			t.Fatalf("%s message not marked truncated (len %d)", m.role, len([]rune(m.content)))
		}
		if n := len([]rune(m.content)) - len([]rune(truncMarker)); n != maxMessageRunes {
			t.Fatalf("%s message body = %d runes, want %d", m.role, n, maxMessageRunes)
		}
	}
}

const sseReply = `event: message_start
data: {"type":"message_start","message":{"id":"m","model":"claude-sse","usage":{"input_tokens":9,"output_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm private"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello "}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"world"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}

event: message_stop
data: {"type":"message_stop"}
`

func TestFullCaptureAssistantReplyFromSSE(t *testing.T) {
	h, _, db, _ := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, sseReply)
	})
	h.PromptRetentionDays = 7
	id := fullCaptureUser(t, db, "dave")

	rw := postAs(t, h, id, `{"model":"claude-test","stream":true,"metadata":{"user_id":"conv-D"},
		"messages":[{"role":"user","content":"hi"}]}`)
	if rw.Body.String() != sseReply {
		t.Fatal("client stream was altered by capture")
	}

	got := storedMessages(t, db)
	if len(got) != 2 {
		t.Fatalf("rows=%d, want 2: %+v", len(got), got)
	}
	reply := got[1]
	if reply.role != "assistant" || reply.seq != 1 {
		t.Fatalf("reply row = %+v", reply)
	}
	if reply.content != "Hello world" {
		t.Fatalf("reply content = %q, want concatenated text deltas", reply.content)
	}
	if strings.Contains(reply.content, "private") {
		t.Fatal("thinking delta leaked into stored reply")
	}
	if reply.model != "claude-sse" {
		t.Fatalf("reply model = %q, want the response model", reply.model)
	}
}

func TestFullCaptureSkippedWhenOff(t *testing.T) {
	body := `{"model":"claude-test","metadata":{"user_id":"conv-E"},
		"messages":[{"role":"user","content":"hi"}]}`

	// Flag off (default identity) → prompt_log only, no conversation_message.
	h, _, db, _ := setupProxy(t, jsonUpstream("x"))
	h.PromptRetentionDays = 7
	ut, err := usertoken.Create(context.Background(), db, "erin")
	if err != nil {
		t.Fatal(err)
	}
	postAs(t, h, &usertoken.Identity{UserTokenID: ut.ID, UserName: ut.Name}, body)
	if n := countRows(t, db, "conversation_message"); n != 0 {
		t.Fatalf("conversation_message rows=%d with full_capture off, want 0", n)
	}
	if n := countRows(t, db, "prompt_log"); n != 1 {
		t.Fatalf("prompt_log rows=%d, want 1 (default capture unchanged)", n)
	}

	// Retention 0 → the per-user flag is ignored, nothing is captured at all.
	h2, _, db2, _ := setupProxy(t, jsonUpstream("x"))
	h2.PromptRetentionDays = 0
	id2 := fullCaptureUser(t, db2, "frank")
	postAs(t, h2, id2, body)
	if n := countRows(t, db2, "conversation_message"); n != 0 {
		t.Fatalf("conversation_message rows=%d with retention 0, want 0", n)
	}
	if n := countRows(t, db2, "prompt_log"); n != 0 {
		t.Fatalf("prompt_log rows=%d with retention 0, want 0", n)
	}

	// No identity at all (admin token / no auth) → no full capture.
	h3, _, db3, _ := setupProxy(t, jsonUpstream("x"))
	h3.PromptRetentionDays = 7
	postAs(t, h3, nil, body)
	if n := countRows(t, db3, "conversation_message"); n != 0 {
		t.Fatalf("conversation_message rows=%d without identity, want 0", n)
	}
}

func countRows(t *testing.T, db *store.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

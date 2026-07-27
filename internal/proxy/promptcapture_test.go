package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCapturePromptInsertAndSkip(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{"ok":true}`)
	}

	// Retention enabled → prompt captured.
	h, _, db, _ := setupProxy(t, upstream)
	h.PromptRetentionDays = 7
	body := []byte(`{"model":"claude-y","metadata":{"user_id":"s1"},"messages":[{"role":"user","content":"hello world"}]}`)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body)))
	if rw.Code != 200 {
		t.Fatalf("status=%d", rw.Code)
	}
	var n int
	var gotModel, gotPrompt string
	if err := db.QueryRow(`SELECT COUNT(*) FROM prompt_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("prompt_log rows=%d, want 1", n)
	}
	_ = db.QueryRow(`SELECT model, prompt FROM prompt_log LIMIT 1`).Scan(&gotModel, &gotPrompt)
	if gotModel != "claude-y" || gotPrompt != "hello world" {
		t.Fatalf("stored model=%q prompt=%q", gotModel, gotPrompt)
	}

	// Retention disabled → nothing captured.
	h2, _, db2, _ := setupProxy(t, upstream)
	h2.PromptRetentionDays = 0
	rw2 := httptest.NewRecorder()
	h2.ServeHTTP(rw2, httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body)))
	var n2 int
	_ = db2.QueryRow(`SELECT COUNT(*) FROM prompt_log`).Scan(&n2)
	if n2 != 0 {
		t.Fatalf("prompt_log rows=%d with retention 0, want 0", n2)
	}
}

func TestExtractPromptStringContent(t *testing.T) {
	body := []byte(`{"model":"claude-x","messages":[
		{"role":"user","content":"first"},
		{"role":"assistant","content":"reply"},
		{"role":"user","content":"the last user turn"}]}`)
	model, prompt, ok := extractPrompt(body)
	if !ok {
		t.Fatal("expected ok")
	}
	if model != "claude-x" {
		t.Fatalf("model=%q", model)
	}
	if prompt != "the last user turn" {
		t.Fatalf("prompt=%q (want last user message)", prompt)
	}
}

func TestExtractPromptBlockContent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"image","source":{}},
		{"type":"text","text":"block text here"},
		{"type":"text","text":"second"}]}]}`)
	_, prompt, ok := extractPrompt(body)
	if !ok {
		t.Fatal("expected ok")
	}
	if prompt != "block text here" {
		t.Fatalf("prompt=%q (want first text block)", prompt)
	}
}

func TestExtractPromptOversizeTrim(t *testing.T) {
	big := strings.Repeat("a", maxPromptLen+500)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + big + `"}]}`)
	_, prompt, ok := extractPrompt(body)
	if !ok {
		t.Fatal("expected ok")
	}
	if len([]rune(prompt)) != maxPromptLen {
		t.Fatalf("prompt len=%d, want %d", len([]rune(prompt)), maxPromptLen)
	}
}

func TestExtractPromptNoUser(t *testing.T) {
	if _, _, ok := extractPrompt([]byte(`{"messages":[{"role":"assistant","content":"x"}]}`)); ok {
		t.Fatal("expected ok=false with no user message")
	}
	if _, _, ok := extractPrompt([]byte(`not json`)); ok {
		t.Fatal("expected ok=false on malformed body")
	}
	if _, _, ok := extractPrompt([]byte(`{"messages":[{"role":"user","content":[{"type":"image"}]}]}`)); ok {
		t.Fatal("expected ok=false when no text block present")
	}
}

// The retention purge deletes in bounded batches so it never holds the write
// lock long enough to starve in-flight requests. Batching is only correct if it
// still removes everything expired, including the rows past the first batch,
// and still leaves everything inside the window alone.
func TestPurgeExpiredDeletesAcrossBatches(t *testing.T) {
	_, _, db, _ := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()

	const (
		expired = purgeBatch*2 + 37 // spans three batches, the last one short
		fresh   = 10
	)
	cutoff := int64(1_000_000)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := range expired {
		if _, err := tx.Exec(
			`INSERT INTO prompt_log (conv_id, ts, model, prompt) VALUES (?, ?, 'm', 'p')`,
			fmt.Sprintf("old-%d", i), cutoff-1); err != nil {
			t.Fatalf("seed expired: %v", err)
		}
	}
	for i := range fresh {
		if _, err := tx.Exec(
			`INSERT INTO prompt_log (conv_id, ts, model, prompt) VALUES (?, ?, 'm', 'p')`,
			fmt.Sprintf("new-%d", i), cutoff); err != nil {
			t.Fatalf("seed fresh: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := purgeExpired(ctx, db, "prompt_log", cutoff); err != nil {
		t.Fatalf("purge: %v", err)
	}

	var remaining, stale int
	if err := db.QueryRow(`SELECT COUNT(*) FROM prompt_log`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM prompt_log WHERE ts < ?`, cutoff).Scan(&stale); err != nil {
		t.Fatalf("count stale: %v", err)
	}
	if stale != 0 {
		t.Fatalf("%d expired rows survived the purge", stale)
	}
	if remaining != fresh {
		t.Fatalf("remaining = %d, want %d (rows inside the window must be kept)", remaining, fresh)
	}
}

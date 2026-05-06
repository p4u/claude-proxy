package router

import (
	"net/http"
	"strings"
	"testing"
)

func req(body string) *http.Request {
	r, _ := http.NewRequest("POST", "http://x/v1/messages", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	return r
}

func TestHeaderWins(t *testing.T) {
	r := req(`{"metadata":{"user_id":"abc"}}`)
	r.Header.Set("X-Router-Conversation-ID", "explicit-123")
	res := Derive(r, []byte(`{"metadata":{"user_id":"abc"}}`))
	if res.Source != SourceHeader || res.ConvID != "explicit-123" {
		t.Fatalf("got %+v", res)
	}
}

func TestMetadataUserID(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"sess-uuid-9f0"}}`)
	res := Derive(req(""), body)
	if res.Source != SourceMetadataUID {
		t.Fatalf("source: %v", res.Source)
	}
	if res.ConvID != "u_sess-uuid-9f0" {
		t.Fatalf("convID: %s", res.ConvID)
	}
}

func TestContentHashStability(t *testing.T) {
	body1 := []byte(`{"system":[{"type":"text","text":"You are Claude Code"}],"messages":[{"role":"user","content":"hello"}]}`)
	body2 := []byte(`{"system":[{"type":"text","text":"You are Claude Code"}],"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"again"}]}`)
	a := Derive(req(""), body1)
	b := Derive(req(""), body2)
	if a.Source != SourceContentHash || b.Source != SourceContentHash {
		t.Fatalf("expected content_hash, got %v / %v", a.Source, b.Source)
	}
	if a.ConvID != b.ConvID {
		t.Fatalf("expected stable hash across turns: %s vs %s", a.ConvID, b.ConvID)
	}
}

func TestContentHashDistinct(t *testing.T) {
	a := Derive(req(""), []byte(`{"system":"sys","messages":[{"role":"user","content":"alpha"}]}`))
	b := Derive(req(""), []byte(`{"system":"sys","messages":[{"role":"user","content":"beta"}]}`))
	if a.ConvID == b.ConvID {
		t.Fatalf("different first messages must hash differently")
	}
}

func TestFallback(t *testing.T) {
	r := Derive(req(""), nil)
	if r.Source != SourceFallback {
		t.Fatalf("source: %v", r.Source)
	}
}

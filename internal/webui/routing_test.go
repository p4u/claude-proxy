package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

func TestRoutingHistoryAuthAndPagination(t *testing.T) {
	db, h := newTestServer(t)
	if w := do(t, h, http.MethodGet, "/api/routing/events", "", nil); w.Code != 401 {
		t.Fatalf("anonymous: %d", w.Code)
	}
	cookie := loginCookie(t, h)
	for range 3 {
		err := store.AppendRoutingEvent(context.Background(), db, store.RoutingEvent{TS: time.Now().Unix(), Policy: "rebalance", Mode: "live", Kind: "switched", Reason: "usage-advantage", Conversation: store.ConversationReference("raw-private-id"), SourceID: "cred_a", TargetID: "cred_b"})
		if err != nil {
			t.Fatal(err)
		}
	}
	w := do(t, h, http.MethodGet, "/api/routing/events?limit=2", "", cookie)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "raw-private-id") {
		t.Fatal("raw conversation leaked")
	}
	var page struct {
		Items []store.RoutingEvent `json:"items"`
		Next  *int64               `json:"next_before"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Next == nil {
		t.Fatalf("page: %+v", page)
	}
	w = do(t, h, http.MethodGet, fmt.Sprintf("/api/routing/events?limit=2&before=%d", *page.Next), "", cookie)
	page.Next = nil
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Next != nil {
		t.Fatalf("older page: %+v", page)
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=no", "before=-1", "before=no", "before=999999999999999999999"} {
		if w := do(t, h, http.MethodGet, "/api/routing/events?"+query, "", cookie); w.Code != 400 {
			t.Fatalf("%s: %d", query, w.Code)
		}
	}
}

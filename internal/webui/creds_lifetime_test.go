package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
)

// AC9: the credential list carries when the login ends and until when it can
// refresh, only for logins whose deadline is known.
func TestListCredsLoginDeadline(t *testing.T) {
	ctx := context.Background()
	db, h := newTestServer(t)
	cookie := loginCookie(t, h)
	expires := time.Now().AddDate(1, 0, 0).Truncate(time.Second)
	deadline := time.Now().Add(5 * 24 * time.Hour).Truncate(time.Second)
	known, err := creds.InsertWithRateLimitTier(ctx, db, "known", "team", "sk-ant-oat-k", "ref-k", expires, 5, "", deadline)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := creds.Insert(ctx, db, "legacy", "max", "sk-ant-oat-l", "ref-l", time.Now().Add(time.Hour), 5)
	if err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, http.MethodGet, "/api/credentials", "", cookie)
	var list []map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	byID := map[string]map[string]any{}
	for _, v := range list {
		byID[v["id"].(string)] = v
	}
	k := byID[known.ID]
	if k["login_ends_at"] != expires.UTC().Format(time.RFC3339) || k["refresh_deadline_at"] != deadline.UTC().Format(time.RFC3339) {
		t.Fatalf("known login: %v", k)
	}
	for _, field := range []string{"login_ends_at", "refresh_deadline_at"} {
		if _, ok := byID[legacy.ID][field]; ok {
			t.Fatalf("legacy login carries %s: %v", field, byID[legacy.ID])
		}
	}
	if strings.Contains(rec.Body.String(), "sk-ant") {
		t.Fatal("the credential list leaked a token")
	}
}

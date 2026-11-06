package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/p4u/claude-proxy/internal/usertoken"
)

func TestUsersListIncludesEmail(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	if _, err := usertoken.CreateWithEmail(ctx, db, "alice", "alice@vocdoni.org"); err != nil {
		t.Fatal(err)
	}
	if _, err := usertoken.Create(ctx, db, "ci"); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, http.MethodGet, "/api/users", "", loginCookie(t, h))
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var users []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &users); err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("users = %v", users)
	}
	for _, u := range users {
		email, ok := u["email"]
		if !ok {
			t.Fatalf("user %v has no email field", u["name"])
		}
		want := ""
		if u["name"] == "alice" {
			want = "alice@vocdoni.org"
		}
		if email != want {
			t.Fatalf("%v: email = %q, want %q", u["name"], email, want)
		}
	}
}

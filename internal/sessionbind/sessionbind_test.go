package sessionbind

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/usertoken"
)

const (
	sidA = "11111111-1111-4111-8111-111111111111"
	sidB = "22222222-2222-4222-8222-222222222222"
	sidC = "33333333-3333-4333-8333-333333333333"
)

func TestValidID(t *testing.T) {
	good := []string{sidA, "abcdef12", strings.Repeat("a", 64), "A_b-C-1234"}
	bad := []string{"", "short", strings.Repeat("a", 65), "has space 1234", "semi;colon1234",
		"new\nline-1234", "ünicode-1234", "../../etc/passwd"}
	for _, s := range good {
		if !ValidID(s) {
			t.Errorf("ValidID(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidID(s) {
			t.Errorf("ValidID(%q) = true, want false", s)
		}
	}
}

func TestRecordAndLookup(t *testing.T) {
	r := New(8)
	clock := time.Unix(1000, 0)
	r.now = func() time.Time { return clock }

	r.Record("user:u1", sidA, "cred-1")
	b, ok := r.Lookup("user:u1", sidA)
	if !ok || b.CredentialID != "cred-1" {
		t.Fatalf("lookup = %+v, %v", b, ok)
	}
	if !b.BoundAt.Equal(clock) || !b.LastSeen.Equal(clock) || !b.SwitchedAt.IsZero() {
		t.Fatalf("unexpected times: %+v", b)
	}

	// Same credential later: last_seen moves, bound_at and switched_at do not.
	clock = clock.Add(time.Minute)
	r.Record("user:u1", sidA, "cred-1")
	b, _ = r.Lookup("user:u1", sidA)
	if !b.BoundAt.Equal(time.Unix(1000, 0)) || !b.LastSeen.Equal(clock) || !b.SwitchedAt.IsZero() {
		t.Fatalf("after repeat: %+v", b)
	}

	// Credential changes: switched_at is stamped, bound_at is preserved.
	clock = clock.Add(time.Minute)
	r.Record("user:u1", sidA, "cred-2")
	b, _ = r.Lookup("user:u1", sidA)
	if b.CredentialID != "cred-2" || !b.SwitchedAt.Equal(clock) || !b.BoundAt.Equal(time.Unix(1000, 0)) {
		t.Fatalf("after switch: %+v", b)
	}
}

func TestOwnerIsolation(t *testing.T) {
	r := New(8)
	r.Record("user:u1", sidA, "cred-1")
	if _, ok := r.Lookup("user:u2", sidA); ok {
		t.Fatal("another owner must not see the binding")
	}
	if _, ok := r.Lookup("admin", sidA); ok {
		t.Fatal("admin scope is separate from user scope")
	}
	// The same session id under two owners is two independent entries.
	r.Record("user:u2", sidA, "cred-9")
	b1, _ := r.Lookup("user:u1", sidA)
	b2, _ := r.Lookup("user:u2", sidA)
	if b1.CredentialID != "cred-1" || b2.CredentialID != "cred-9" {
		t.Fatalf("entries bled together: %+v %+v", b1, b2)
	}
}

func TestInvalidIgnored(t *testing.T) {
	r := New(8)
	r.Record("user:u1", "bad id!", "cred-1")
	r.Record("user:u1", sidA, "")
	if r.Len() != 0 {
		t.Fatalf("len = %d, want 0", r.Len())
	}
	if _, ok := r.Lookup("user:u1", "bad id!"); ok {
		t.Fatal("invalid id looked up")
	}
}

func TestLRUEviction(t *testing.T) {
	r := New(2)
	r.Record("o", sidA, "c1")
	r.Record("o", sidB, "c2")
	// Touch A so B becomes the eviction candidate.
	r.Record("o", sidA, "c1")
	r.Record("o", sidC, "c3")

	if r.Len() != 2 {
		t.Fatalf("len = %d, want 2", r.Len())
	}
	if _, ok := r.Lookup("o", sidB); ok {
		t.Error("B should have been evicted")
	}
	for _, sid := range []string{sidA, sidC} {
		if _, ok := r.Lookup("o", sid); !ok {
			t.Errorf("%s should be present", sid)
		}
	}
}

func TestLookupDoesNotRefreshRecency(t *testing.T) {
	r := New(2)
	r.Record("o", sidA, "c1")
	r.Record("o", sidB, "c2")
	r.Lookup("o", sidA) // polling must not protect A from eviction
	r.Record("o", sidC, "c3")
	if _, ok := r.Lookup("o", sidA); ok {
		t.Error("A should have been evicted despite being looked up")
	}
}

func TestDefaultCapacityAndNilSafety(t *testing.T) {
	if r := New(0); r.cap != DefaultCapacity {
		t.Fatalf("cap = %d", r.cap)
	}
	var r *Registry
	r.Record("o", sidA, "c1") // must not panic
	r.RecordRequest(httptest.NewRequest("GET", "/", nil), "c1", Route{})
	if _, ok := r.Lookup("o", sidA); ok || r.Len() != 0 {
		t.Fatal("nil registry should be empty")
	}
}

func TestConcurrentUse(t *testing.T) {
	r := New(64)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 500 {
				sid := "session-" + strings.Repeat("x", g) + string(rune('a'+i%26)) + "-0123"
				r.Record("o", sid, "c"+string(rune('0'+i%3)))
				r.Lookup("o", sid)
			}
		})
	}
	wg.Wait()
	if r.Len() > 64 {
		t.Fatalf("len %d exceeds capacity", r.Len())
	}
}

func TestOwnerKey(t *testing.T) {
	cases := []struct {
		id   *usertoken.Identity
		want string
	}{
		{nil, "anon"},
		{&usertoken.Identity{}, "anon"},
		{&usertoken.Identity{IsAdmin: true}, "admin"},
		{&usertoken.Identity{UserTokenID: "abc"}, "user:abc"},
	}
	for _, c := range cases {
		if got := OwnerKey(c.id); got != c.want {
			t.Errorf("OwnerKey(%+v) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestRecordRequest(t *testing.T) {
	r := New(8)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(usertoken.WithIdentity(context.Background(), &usertoken.Identity{UserTokenID: "u1"}))

	r.RecordRequest(req, "cred-1", Route{}) // no header: ignored
	if r.Len() != 0 {
		t.Fatal("recorded without header")
	}
	req.Header.Set(Header, "junk!")
	r.RecordRequest(req, "cred-1", Route{})
	if r.Len() != 0 {
		t.Fatal("recorded invalid header")
	}
	req.Header.Set(Header, sidA)
	r.RecordRequest(req, "cred-1", Route{})
	if b, ok := r.Lookup("user:u1", sidA); !ok || b.CredentialID != "cred-1" {
		t.Fatalf("lookup = %+v, %v", b, ok)
	}
}

func TestRecordRouteTracksLatest(t *testing.T) {
	r := New(8)
	first := Route{ConvID: "conv-1", Provider: "anthropic"}
	r.RecordRoute("o", sidA, "c1", first)
	if b, ok := r.Lookup("o", sidA); !ok || b.Route.ConvID != "conv-1" || b.Route.Provider != "anthropic" {
		t.Fatalf("lookup = %+v, %v", b, ok)
	}
	second := Route{ConvID: "conv-1", Provider: "custom", Scope: "my-model", Allowed: []string{"c2"}}
	r.RecordRoute("o", sidA, "c2", second)
	b, _ := r.Lookup("o", sidA)
	if b.Route.Provider != "custom" || b.Route.Scope != "my-model" || len(b.Route.Allowed) != 1 || b.CredentialID != "c2" {
		t.Fatalf("route not replaced: %+v", b)
	}
	if _, ok := r.Lookup("other", sidA); ok {
		t.Fatal("route visible to another owner")
	}
}

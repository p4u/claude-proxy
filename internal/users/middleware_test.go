package users

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/p4u/claude-proxy/internal/store"
)

func TestMiddlewareAuth(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s := NewStore(db)
	ctx := context.Background()
	_, raw, err := s.Create(ctx, "testuser")
	if err != nil {
		t.Fatal(err)
	}

	hit := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(s, inner)

	cases := []struct {
		name       string
		authHeader string
		wantStatus int
		wantHit    bool
	}{
		{"missing header", "", 401, false},
		{"malformed — Basic scheme", "Basic dXNlcjpwYXNz", 401, false},
		{"malformed — no Bearer prefix", "cp_notabearer", 401, false},
		{"unknown token", "Bearer cp_unknowntokenvalue", 401, false},
		{"valid token", "Bearer " + raw, 200, true},
		{"valid token case-insensitive Bearer", "bearer " + raw, 200, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit = false
			req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, req)
			if rw.Code != tc.wantStatus {
				t.Fatalf("status: got %d want %d body=%s", rw.Code, tc.wantStatus, rw.Body.String())
			}
			if hit != tc.wantHit {
				t.Fatalf("inner hit: got %v want %v", hit, tc.wantHit)
			}
		})
	}
}

func TestMiddlewareContextUser(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s := NewStore(db)
	_, raw, err := s.Create(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}

	var gotUser User
	var gotOK bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotOK = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	h := Middleware(s, inner)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != 200 {
		t.Fatalf("status: got %d", rw.Code)
	}
	if !gotOK {
		t.Fatal("no user in context")
	}
	if gotUser.Name != "alice" {
		t.Fatalf("user name: got %q want alice", gotUser.Name)
	}
}

func TestMiddlewareUnauthorizedBody(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s := NewStore(db)
	h := Middleware(s, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/admin/credentials", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != 401 {
		t.Fatalf("status: got %d want 401", rw.Code)
	}
	body := rw.Body.String()
	if body != `{"error":"unauthorized"}` {
		t.Fatalf("body: got %q", body)
	}
	if ct := rw.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type: got %q", ct)
	}
}

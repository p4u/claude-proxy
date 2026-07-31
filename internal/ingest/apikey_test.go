package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

func keyTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// stubVerify points key verification at a local server for the duration of a test.
func stubVerify(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	orig := verifyClient
	verifyClient = ts.Client()
	verifyClient.Transport = &redirectTo{ts.URL, ts.Client().Transport}
	t.Cleanup(func() { verifyClient = orig })
}

type redirectTo struct {
	base string
	rt   http.RoundTripper
}

func (r *redirectTo) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := req.URL.Parse(r.base)
	req.URL.Scheme, req.URL.Host, req.Host = u.Scheme, u.Host, u.Host
	rt := r.rt
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(req)
}

func TestImportKeySuccess(t *testing.T) {
	db := keyTestDB(t)
	var sawAuth, sawPath string
	stubVerify(t, func(w http.ResponseWriter, r *http.Request) {
		sawAuth, sawPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	})

	c, err := ImportKey(context.Background(), db, provider.GLM, "zai", "pro", "  secret-key  ", "", 0)
	if err != nil {
		t.Fatalf("ImportKey: %v", err)
	}
	if c.Provider != provider.GLM {
		t.Errorf("provider = %q, want %q", c.Provider, provider.GLM)
	}
	// Surrounding whitespace is a copy/paste artefact, not part of the key.
	if c.AccessToken != "secret-key" {
		t.Errorf("access token = %q, want it trimmed", c.AccessToken)
	}
	if c.RefreshToken != "" {
		t.Errorf("API keys have no refresh token, got %q", c.RefreshToken)
	}
	if sawAuth != "Bearer secret-key" {
		t.Errorf("verification Authorization = %q", sawAuth)
	}
	// Deliberately /v1/messages, not /v1/models: api.z.ai returns 200 on
	// /v1/models for a garbage token, so verifying there would admit any key.
	if !strings.HasSuffix(sawPath, "/v1/messages") {
		t.Errorf("verification hit %q, want /v1/messages (the only probe that actually authenticates)", sawPath)
	}
}

// A key that cannot authenticate must never enter the pool: once stored, the
// selector would hand live conversations to it and every one would fail.
func TestImportKeyRejectsBadKey(t *testing.T) {
	db := keyTestDB(t)
	stubVerify(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) })

	if _, err := ImportKey(context.Background(), db, provider.GLM, "zai", "", "bad", "", 0); err == nil {
		t.Fatal("expected an error for a rejected key")
	}
	list, err := creds.List(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("rejected key was stored anyway: %d rows", len(list))
	}
}

func TestImportKeyRejectsDuplicateAndEmpty(t *testing.T) {
	db := keyTestDB(t)
	stubVerify(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	})
	ctx := context.Background()

	if _, err := ImportKey(ctx, db, provider.GLM, "a", "", "dup", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportKey(ctx, db, provider.GLM, "b", "", "dup", "", 0); err == nil {
		t.Error("expected duplicate rejection")
	}
	if _, err := ImportKey(ctx, db, provider.GLM, "c", "", "   ", "", 0); err == nil {
		t.Error("expected empty-key rejection")
	}
	// Anthropic is OAuth-based; an API key is the wrong credential shape for it.
	if _, err := ImportKey(ctx, db, provider.Anthropic, "d", "", "k", "", 0); err == nil {
		t.Error("expected rejection for an OAuth provider")
	}
	if _, err := ImportKey(ctx, db, "nonesuch", "e", "", "k", "", 0); err == nil {
		t.Error("expected rejection for an unknown provider")
	}
}

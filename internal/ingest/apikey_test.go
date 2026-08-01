package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// Moving a credential to another endpoint must re-verify the key there. The
// usual reason to move one is that it was added against the wrong cluster and
// got marked revoked, so a successful move also has to heal that status.
func TestUpdateKeyEndpoint(t *testing.T) {
	db := keyTestDB(t)
	ctx := context.Background()
	stubVerify(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	})

	c, err := ImportKey(ctx, db, provider.MiMo, "m", "", "key", "sgp", 0)
	if err != nil {
		t.Fatal(err)
	}
	// sgp is MiMo's default, so nothing is stored — the credential follows the
	// registry rather than pinning a URL that may later move.
	if c.BaseURL != "" {
		t.Errorf("default endpoint should not be stored, got %q", c.BaseURL)
	}
	if err := creds.SetStatus(ctx, db, c.ID, creds.StatusRevoked); err != nil {
		t.Fatal(err)
	}

	moved, err := UpdateKeyEndpoint(ctx, db, c.ID, "ams")
	if err != nil {
		t.Fatalf("UpdateKeyEndpoint: %v", err)
	}
	if moved.BaseURL != "https://token-plan-ams.xiaomimimo.com/anthropic" {
		t.Errorf("endpoint not stored, got %q", moved.BaseURL)
	}
	if moved.Status != creds.StatusActive {
		t.Errorf("status = %q, want active — a key proven to work must not stay revoked", moved.Status)
	}

	// A custom URL the registry has never heard of must be accepted.
	custom := "https://token-plan-xyz.example.com/anthropic"
	if got, err := UpdateKeyEndpoint(ctx, db, c.ID, custom); err != nil || got.BaseURL != custom {
		t.Errorf("custom URL: got %q err %v", got.BaseURL, err)
	}
}

// A rejected key must leave the stored endpoint untouched: swapping one broken
// endpoint for another would be worse than refusing the change.
func TestUpdateKeyEndpointRejectedLeavesCredentialAlone(t *testing.T) {
	db := keyTestDB(t)
	ctx := context.Background()
	ok := true
	stubVerify(t, func(w http.ResponseWriter, _ *http.Request) {
		if !ok {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	})

	c, err := ImportKey(ctx, db, provider.MiMo, "m", "", "key", "ams", 0)
	if err != nil {
		t.Fatal(err)
	}
	before := c.BaseURL

	ok = false
	if _, err := UpdateKeyEndpoint(ctx, db, c.ID, "cn"); err == nil {
		t.Fatal("expected the move to be rejected")
	}
	after, err := creds.Get(ctx, db, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.BaseURL != before {
		t.Errorf("endpoint changed despite verification failing: %q → %q", before, after.BaseURL)
	}
}

// OAuth credentials have a fixed endpoint; offering to move one would be a lie.
func TestUpdateKeyEndpointRejectsOAuth(t *testing.T) {
	db := keyTestDB(t)
	ctx := context.Background()
	c, err := creds.Insert(ctx, db, "a", "max", "sk-ant-oat-x", "rt", time.Now().Add(time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateKeyEndpoint(ctx, db, c.ID, "cn"); err == nil {
		t.Error("expected OAuth credentials to be rejected")
	}
}

// The probe exists so the operator does not have to know what a host supports.
// A translation shim commonly ignores the requested model and answers as
// whatever it fronts, so the reported name — not the requested one — is what
// the catalogue must carry.
func TestProbeCustomHostDiscovers(t *testing.T) {
	var sawModels, sawCount, sawNoAuth bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			sawModels = true
			w.WriteHeader(404) // this host publishes no catalogue
		case "/v1/messages/count_tokens":
			sawCount = true
			_, _ = w.Write([]byte(`{"input_tokens":1}`))
		case "/v1/messages":
			if r.Header.Get("Authorization") == "" {
				sawNoAuth = true
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"type":"message","model":"Qwen3.6-fable","content":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(ts.Close)
	orig := verifyClient
	verifyClient = ts.Client()
	t.Cleanup(func() { verifyClient = orig })

	p := ProbeCustomHost(context.Background(), ts.URL, "sk-test", "")
	if !p.OK {
		t.Fatalf("probe failed: %s", p.Error)
	}
	if !sawModels || !sawCount || !sawNoAuth {
		t.Errorf("probe did not exercise every capability check (models=%v count=%v auth=%v)",
			sawModels, sawCount, sawNoAuth)
	}
	if p.HasModelsAPI {
		t.Error("a 404 on /v1/models must not be reported as a catalogue")
	}
	if !p.HasCountTokens {
		t.Error("count_tokens answered 200 and should be reported")
	}
	if !p.AuthRequired {
		t.Error("the host rejected an unauthenticated call, so auth is enforced")
	}
	if p.ReportedModel != "Qwen3.6-fable" {
		t.Errorf("reported model = %q, want the name the host answered with", p.ReportedModel)
	}
	if len(p.Models) != 1 || p.Models[0].ID != "Qwen3.6-fable" {
		t.Errorf("catalogue should default to the reported model, got %+v", p.Models)
	}
	if p.Models[0].ContextWindow != 0 {
		t.Error("context window was never published and must not be invented")
	}
}

// When the host does publish a catalogue, take it — including the context
// windows, which are otherwise undiscoverable.
func TestProbeCustomHostUsesModelsAPI(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[
				{"id":"m-one","display_name":"M One","max_input_tokens":200000,"max_tokens":8192},
				{"id":"m-two","display_name":"M Two"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"message","model":"m-one","content":[]}`))
	}))
	t.Cleanup(ts.Close)
	orig := verifyClient
	verifyClient = ts.Client()
	t.Cleanup(func() { verifyClient = orig })

	p := ProbeCustomHost(context.Background(), ts.URL, "k", "")
	if !p.HasModelsAPI || len(p.Models) != 2 {
		t.Fatalf("catalogue not taken from /v1/models: %+v", p)
	}
	if p.Models[0].ContextWindow != 200000 || p.Models[0].MaxOutput != 8192 {
		t.Errorf("published context/output must be carried through: %+v", p.Models[0])
	}
}

func TestProbeCustomHostRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	if p := ProbeCustomHost(ctx, "", "k", ""); p.OK || p.Error == "" {
		t.Error("empty URL must fail with a message")
	}
	if p := ProbeCustomHost(ctx, "10.0.0.1:3456", "k", ""); p.OK || p.Error == "" {
		t.Error("a URL without a scheme must fail rather than be guessed at")
	}
}

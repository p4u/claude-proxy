package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
)

// fakeAnthropicCLI serves the token and profile endpoints for CLI tests.
func fakeAnthropicCLI(t *testing.T) (tokenCalls, profileStatus *atomic.Int32) {
	t.Helper()
	tokenCalls, profileStatus = new(atomic.Int32), new(atomic.Int32)
	profileStatus.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenCalls.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-fresh","refresh_token":"ref-fresh","expires_in":28800}`))
		case "/profile":
			w.WriteHeader(int(profileStatus.Load()))
		}
	}))
	prevToken, prevProfile := creds.TokenURL, creds.ProfileURL
	creds.TokenURL, creds.ProfileURL = srv.URL+"/token", srv.URL+"/profile"
	t.Cleanup(func() { creds.TokenURL, creds.ProfileURL = prevToken, prevProfile; srv.Close() })
	return tokenCalls, profileStatus
}

// AC3b: a backup of a year-long login restores without a token-endpoint call
// and keeps its tokens, expiry and deadline; a dead token fails the restore.
func TestCredsExportImportBulkKeepsLongLivedLogin(t *testing.T) {
	ctx := context.Background()
	tokenCalls, profileStatus := fakeAnthropicCLI(t)
	_, src := cliDB(t)
	expires := time.Now().AddDate(1, 0, 0).Truncate(time.Second)
	deadline := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	orig, err := creds.InsertWithRateLimitTier(ctx, src, "team-a", "team", "sk-ant-oat-year", "ref-year", expires, 5, "", deadline)
	if err != nil {
		t.Fatal(err)
	}
	var backup bytes.Buffer
	if err := exportCreds(ctx, src, &backup); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(backup.String(), `"refreshTokenExpiresAt":`) {
		t.Fatalf("export lacks the deadline: %s", backup.String())
	}

	_, dst := cliDB(t)
	if n, err := importBulk(ctx, dst, bytes.NewReader(backup.Bytes()), io.Discard); err != nil || n != 1 {
		t.Fatalf("import-bulk: %d, %v", n, err)
	}
	list, _ := creds.List(ctx, dst)
	if len(list) != 1 {
		t.Fatalf("restored %d credentials", len(list))
	}
	got := list[0]
	if got.AccessToken != orig.AccessToken || got.RefreshToken != orig.RefreshToken ||
		!got.ExpiresAt.Equal(expires) || !got.RefreshExpiresAt.Equal(deadline) ||
		got.Label != "team-a" || got.Weight != 5 {
		t.Fatalf("restored %+v, want the exported login unchanged", got)
	}
	if n := tokenCalls.Load(); n != 0 {
		t.Fatalf("restore made %d token-endpoint calls", n)
	}

	profileStatus.Store(http.StatusUnauthorized)
	_, dead := cliDB(t)
	if _, err := importBulk(ctx, dead, bytes.NewReader(backup.Bytes()), io.Discard); err == nil || !strings.Contains(err.Error(), "credential is not alive") {
		t.Fatalf("dead token restore = %v, want credential is not alive", err)
	}
}

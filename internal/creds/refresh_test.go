package creds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mockTokenServer stands in for the Anthropic OAuth token endpoint.
func mockTokenServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	prev := TokenURL
	SetTokenURL(srv.URL)
	t.Cleanup(func() { SetTokenURL(prev); srv.Close() })
	return srv
}

func TestRefreshTokensSuccess(t *testing.T) {
	mockTokenServer(t, 200, `{"access_token":"sk-ant-oat-new","refresh_token":"ref-new","expires_in":3600}`)

	access, refresh, exp, err := RefreshTokens(context.Background(), "old-ref")
	if err != nil {
		t.Fatalf("refresh-tokens: %v", err)
	}
	if access != "sk-ant-oat-new" || refresh != "ref-new" {
		t.Fatalf("unexpected tokens: %q %q", access, refresh)
	}
	if time.Until(exp) <= 0 {
		t.Fatalf("expiry should be in the future, got %v", exp)
	}
}

func TestRefreshTokensRejected(t *testing.T) {
	mockTokenServer(t, 400, `{"error":"invalid_grant"}`)
	if _, _, _, err := RefreshTokens(context.Background(), "bad"); err == nil {
		t.Fatal("expected rejection error from 400 response")
	}
}

func TestRefresherRefreshNow(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	mockTokenServer(t, 200, `{"access_token":"sk-ant-oat-fresh","refresh_token":"ref-fresh","expires_in":3600}`)

	// Credential far from expiry but force-refresh should still rotate tokens.
	c, err := Insert(ctx, db, "a", "max", "sk-ant-oat-old", "ref-old", time.Now().Add(time.Hour), 5)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	r := NewRefresher(db)
	SetTokenClient(r, &http.Client{Timeout: 5 * time.Second})
	got, err := r.RefreshNow(ctx, c.ID)
	if err != nil {
		t.Fatalf("refresh-now: %v", err)
	}
	if got.AccessToken != "sk-ant-oat-fresh" || got.RefreshToken != "ref-fresh" {
		t.Fatalf("tokens not rotated: %+v", got)
	}
	if got.Status != StatusActive {
		t.Fatalf("status = %q, want active", got.Status)
	}
}

func TestRefresherRejectionMarksRevoked(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	mockTokenServer(t, 401, `{"error":"invalid_grant"}`)

	c, err := Insert(ctx, db, "a", "max", "sk-ant-oat-old", "ref-old", time.Now().Add(time.Hour), 5)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	r := NewRefresher(db)
	SetTokenClient(r, &http.Client{Timeout: 5 * time.Second})
	if _, err := r.RefreshNow(ctx, c.ID); err == nil {
		t.Fatal("expected refresh rejection error")
	}
	got, _ := Get(ctx, db, c.ID)
	if got.Status != StatusRevoked {
		t.Fatalf("status = %q, want revoked after rejection", got.Status)
	}
}

package creds

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// redirector starts a server answering every request with code and a
// Location of target, and returns its URL.
func redirector(t *testing.T, code int, target string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, code)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// redirectToken points TokenURL at a server that 307s to f's token endpoint,
// so a followed redirect would re-send the refresh token there.
func redirectToken(t *testing.T, f *fakeAnthropic) {
	t.Helper()
	f.serve(t)
	prev := TokenURL
	TokenURL = redirector(t, http.StatusTemporaryRedirect, prev)
	t.Cleanup(func() { TokenURL = prev })
}

// A 307 from the token endpoint is a transient failure of the refresher's own
// client: the refresh token is not re-posted to Location, and the credential
// keeps its status and tokens.
func TestRefreshDoesNotFollowRedirect(t *testing.T) {
	ctx := context.Background()
	f := &fakeAnthropic{}
	redirectToken(t, f)
	db := testDB(t)
	r := NewRefresher(db)
	c := insertAt(t, db, "a", near(), time.Time{})

	_, refreshed, err := r.Refresh(ctx, c.ID)
	if n := f.tokenCalls.Load(); n != 0 {
		t.Fatalf("redirect target received %d token requests, want 0", n)
	}
	if err == nil || errors.Is(err, ErrLoginRejected) || refreshed {
		t.Fatalf("err = %v refreshed = %v, want a transient error", err, refreshed)
	}
	got, err := Get(ctx, db, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusActive {
		t.Fatalf("status = %q, want active", got.Status)
	}
	if got.AccessToken != c.AccessToken || got.RefreshToken != c.RefreshToken {
		t.Fatal("tokens changed after a redirected refresh")
	}
}

func TestRefreshTokensDoesNotFollowRedirect(t *testing.T) {
	f := &fakeAnthropic{}
	redirectToken(t, f)
	_, err := RefreshTokens(context.Background(), "ref-old", time.Time{})
	if n := f.tokenCalls.Load(); n != 0 {
		t.Fatalf("redirect target received %d token requests, want 0", n)
	}
	if err == nil {
		t.Fatal("a redirected refresh must fail")
	}
}

// The bearer token must not follow a 302 from the profile endpoint.
func TestCheckTokenDoesNotFollowRedirect(t *testing.T) {
	f := &fakeAnthropic{}
	f.serve(t)
	prev := ProfileURL
	ProfileURL = redirector(t, http.StatusFound, prev)
	t.Cleanup(func() { ProfileURL = prev })

	err := CheckToken(context.Background(), "sk-ant-oat-old")
	if n := f.profileCalls.Load(); n != 0 {
		t.Fatalf("redirect target received %d profile requests, want 0", n)
	}
	if err == nil || errors.Is(err, ErrTokenRejected) {
		t.Fatalf("err = %v, want an inconclusive error", err)
	}
}

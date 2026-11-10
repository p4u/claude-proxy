package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/claudeoauth"
	"github.com/p4u/claude-proxy/internal/creds"
)

// fakeAnthropic serves the token and profile endpoints and counts calls.
type fakeAnthropic struct {
	tokenCalls, profileCalls atomic.Int32
	deadlineIn               int // refresh_token_expires_in the token endpoint reports; 0 omits it
	profileStatus            int // 0 answers 200
}

func (f *fakeAnthropic) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			f.tokenCalls.Add(1)
			extra := ""
			if f.deadlineIn > 0 {
				extra = fmt.Sprintf(`,"refresh_token_expires_in":%d`, f.deadlineIn)
			}
			_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-fresh","refresh_token":"ref-fresh","expires_in":28800` + extra + `}`))
		case "/profile":
			f.profileCalls.Add(1)
			if f.profileStatus != 0 {
				w.WriteHeader(f.profileStatus)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	prevToken, prevProfile := creds.TokenURL, creds.ProfileURL
	creds.TokenURL, creds.ProfileURL = srv.URL+"/token", srv.URL+"/profile"
	t.Cleanup(func() { creds.TokenURL, creds.ProfileURL = prevToken, prevProfile; srv.Close() })
}

// credJSON builds a .credentials.json with the given expiry and deadline
// (zero omits the deadline).
func credJSON(t *testing.T, refresh string, expires, deadline time.Time) []byte {
	t.Helper()
	block := map[string]any{
		"accessToken": "sk-ant-oat-file", "refreshToken": refresh,
		"expiresAt": expires.UnixMilli(), "subscriptionType": "team",
	}
	if !deadline.IsZero() {
		block["refreshTokenExpiresAt"] = deadline.UnixMilli()
	}
	b, err := json.Marshal(map[string]any{"claudeAiOauth": block})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func about(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if want.IsZero() {
		if !got.IsZero() {
			t.Fatalf("%s = %v, want unknown", what, got)
		}
		return
	}
	if d := got.Sub(want); d < -time.Minute || d > time.Minute {
		t.Fatalf("%s = %v, want about %v", what, got, want)
	}
}

// AC3: a pasted 8-hour file stores its deadline, the earlier of the file's
// and the import refresh's, or unknown when the file has none.
func TestImportFileDeadline(t *testing.T) {
	ctx := context.Background()
	day := 24 * time.Hour
	for _, tt := range []struct {
		name       string
		file       time.Duration // 0 = absent
		deadlineIn time.Duration // 0 = absent
		want       time.Duration // 0 = unknown
	}{
		{"file earlier", 10 * day, 20 * day, 10 * day},
		{"refresh earlier", 20 * day, 10 * day, 10 * day},
		{"file only", 10 * day, 0, 10 * day},
		{"absent from the file", 0, 10 * day, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := testDB(t)
			(&fakeAnthropic{deadlineIn: int(tt.deadlineIn.Seconds())}).serve(t)
			var file, want time.Time
			if tt.file != 0 {
				file = time.Now().Add(tt.file)
			}
			if tt.want != 0 {
				want = time.Now().Add(tt.want)
			}
			c, err := ImportFromJSON(ctx, db, credJSON(t, "ref-"+tt.name, time.Now().Add(time.Hour), file), "", 0)
			if err != nil {
				t.Fatal(err)
			}
			about(t, "deadline", c.RefreshExpiresAt, want)
		})
	}
	t.Run("update replaces the stored deadline", func(t *testing.T) {
		db := testDB(t)
		(&fakeAnthropic{}).serve(t)
		old, err := creds.InsertWithRateLimitTier(ctx, db, "old", "team", "sk-ant-oat-old", "ref-old", future(), 5, "", time.Now().Add(2*day))
		if err != nil {
			t.Fatal(err)
		}
		next := time.Now().Add(20 * day)
		c, err := UpdateFromJSON(ctx, db, old.ID, credJSON(t, "ref-new", time.Now().Add(time.Hour), next))
		if err != nil {
			t.Fatal(err)
		}
		about(t, "deadline", c.RefreshExpiresAt, next)
	})
}

// AC3: sign-ins store their deadline and a reconnect replaces the old one.
func TestOAuthDeadline(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	first := time.Now().Add(27 * 24 * time.Hour).Truncate(time.Second)
	c, err := ImportOAuth(ctx, db, &claudeoauth.Tokens{
		AccessToken: "sk-ant-oat-a", RefreshToken: "ref-a", ExpiresAt: time.Now().AddDate(1, 0, 0),
		RefreshTokenExpiresAt: first, SubscriptionType: "team",
	}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := creds.Get(ctx, db, c.ID); !got.RefreshExpiresAt.Equal(first) {
		t.Fatalf("sign-in deadline = %v, want %v", got.RefreshExpiresAt, first)
	}
	second := first.Add(30 * 24 * time.Hour)
	if _, err := UpdateFromOAuth(ctx, db, c.ID, &claudeoauth.Tokens{
		AccessToken: "sk-ant-oat-b", RefreshToken: "ref-b", ExpiresAt: time.Now().AddDate(1, 0, 0),
		RefreshTokenExpiresAt: second, SubscriptionType: "team",
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := creds.Get(ctx, db, c.ID); !got.RefreshExpiresAt.Equal(second) {
		t.Fatalf("reconnect deadline = %v, want the new login's %v", got.RefreshExpiresAt, second)
	}
}

// AC3b (ingest side): a long-lived file is verified against the profile
// endpoint and stored exactly as it is, without a token-endpoint call.
func TestImportLongLivedFileKeepsTokens(t *testing.T) {
	ctx := context.Background()
	expires := time.Now().AddDate(1, 0, 0).Truncate(time.Second)
	deadline := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	t.Run("alive", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{}
		f.serve(t)
		c, err := ImportFromJSON(ctx, db, credJSON(t, "ref-file", expires, deadline), "", 0)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := creds.Get(ctx, db, c.ID)
		if got.AccessToken != "sk-ant-oat-file" || got.RefreshToken != "ref-file" ||
			!got.ExpiresAt.Equal(expires) || !got.RefreshExpiresAt.Equal(deadline) {
			t.Fatalf("stored %+v, want the file's tokens, expiry and deadline", got)
		}
		if _, err := UpdateFromJSON(ctx, db, c.ID, credJSON(t, "ref-file", expires, deadline)); err != nil {
			t.Fatal(err)
		}
		if f.tokenCalls.Load() != 0 || f.profileCalls.Load() != 2 {
			t.Fatalf("token calls %d, profile calls %d; want 0 and 2", f.tokenCalls.Load(), f.profileCalls.Load())
		}
	})
	t.Run("rejected", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{profileStatus: http.StatusUnauthorized}
		f.serve(t)
		_, err := ImportFromJSON(ctx, db, credJSON(t, "ref-file", expires, deadline), "", 0)
		if err == nil || !strings.Contains(err.Error(), "credential is not alive") || f.tokenCalls.Load() != 0 {
			t.Fatalf("err = %v, token calls %d", err, f.tokenCalls.Load())
		}
	})
	t.Run("profile unavailable", func(t *testing.T) {
		db := testDB(t)
		(&fakeAnthropic{profileStatus: http.StatusServiceUnavailable}).serve(t)
		_, err := ImportFromJSON(ctx, db, credJSON(t, "ref-file", expires, deadline), "", 0)
		if err == nil || strings.Contains(err.Error(), "not alive") || !strings.Contains(err.Error(), "503") {
			t.Fatalf("err = %v, want the profile check's own error", err)
		}
		if list, _ := creds.List(ctx, db); len(list) != 0 {
			t.Fatal("a failed check stored the credential")
		}
	})
}

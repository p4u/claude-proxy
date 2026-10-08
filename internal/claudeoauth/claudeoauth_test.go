package claudeoauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
)

// fakeAnthropic serves the token and profile endpoints. It checks the PKCE
// verifier against the challenge from the authorize URL, so a broken
// challenge/verifier pairing fails the exchange exactly as upstream would.
type fakeAnthropic struct {
	challenge string
	orgType   string
	tier      any // nil omits the field; json.RawMessage("null") publishes null
	scope     string
	tokenCode int
	exchanges int
}

func (f *fakeAnthropic) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth/token":
			f.exchanges++
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			sum := sha256.Sum256([]byte(b["code_verifier"]))
			if b["grant_type"] != "authorization_code" || b["code"] != "the-code" ||
				b["client_id"] != creds.ClientID || b["redirect_uri"] != RedirectURL ||
				base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad code","access_token":"sk-ant-oat-leak"}`))
				return
			}
			if f.tokenCode != 0 {
				w.WriteHeader(f.tokenCode)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "sk-ant-oat01-new", "refresh_token": "sk-ant-ort01-new",
				"expires_in": 28800, "scope": f.scope,
				"account": map[string]string{"email_address": "token@example.com"},
			})
		case "/api/oauth/profile":
			if r.Header.Get("Authorization") != "Bearer sk-ant-oat01-new" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			org := map[string]any{"organization_type": f.orgType}
			if f.tier != nil {
				org["rate_limit_tier"] = f.tier
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account":      map[string]string{"email": "owner@example.com"},
				"organization": org,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldToken, oldProfile := creds.TokenURL, ProfileURL
	creds.TokenURL, ProfileURL = srv.URL+"/v1/oauth/token", srv.URL+"/api/oauth/profile"
	t.Cleanup(func() { creds.TokenURL, ProfileURL = oldToken, oldProfile })
	return srv
}

// start opens a session and returns its ID, state and PKCE challenge.
func start(t *testing.T, f *Flow) (id, state, challenge string) {
	t.Helper()
	id, raw, err := f.Start()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if got := u.Scheme + "://" + u.Host + u.Path; got != AuthorizeURL {
		t.Fatalf("authorize endpoint = %s", got)
	}
	for k, want := range map[string]string{
		"code": "true", "client_id": creds.ClientID, "response_type": "code",
		"redirect_uri": RedirectURL, "code_challenge_method": "S256",
	} {
		if q.Get(k) != want {
			t.Fatalf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if !strings.Contains(" "+q.Get("scope")+" ", " user:inference ") {
		t.Fatalf("scope %q lacks user:inference", q.Get("scope"))
	}
	return id, q.Get("state"), q.Get("code_challenge")
}

func TestExchangeHappyPath(t *testing.T) {
	fake := &fakeAnthropic{orgType: "claude_max", scope: strings.Join(Scopes, " ")}
	fake.serve(t)
	f := New()
	id, state, challenge := start(t, f)
	fake.challenge = challenge

	tok, err := f.Exchange(context.Background(), id, "  the-code#"+state+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "sk-ant-oat01-new" || tok.RefreshToken != "sk-ant-ort01-new" ||
		tok.SubscriptionType != "max" || tok.Email != "owner@example.com" {
		t.Fatalf("tokens = %+v", tok)
	}
	if d := time.Until(tok.ExpiresAt); d < 7*time.Hour || d > 8*time.Hour {
		t.Fatalf("expiry %v from now, want 8h minus the 5m margin", d)
	}
	if _, err := f.Exchange(context.Background(), id, "the-code#"+state); err == nil ||
		!strings.Contains(err.Error(), "expired or unknown") {
		t.Fatalf("second exchange = %v, want the session to be single-use", err)
	}
}

func TestExchangeRateLimitTier(t *testing.T) {
	for _, tt := range []struct {
		name    string
		tier    any
		want    string
		invalid bool
	}{
		{name: "absent"},
		{name: "null", tier: json.RawMessage("null")},
		{name: "max 20x", tier: "default_claude_max_20x", want: "default_claude_max_20x"},
		{name: "max 5x", tier: "default_claude_max_5x", want: "default_claude_max_5x"},
		{name: "future", tier: " future:Plan/未定 ", want: "future:Plan/未定"},
		{name: "too long", tier: strings.Repeat("x", 129), invalid: true},
		{name: "control", tier: "bad\ntier", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeAnthropic{orgType: "claude_max", tier: tt.tier, scope: "user:inference"}
			fake.serve(t)
			f := New()
			id, state, challenge := start(t, f)
			fake.challenge = challenge
			tok, err := f.Exchange(context.Background(), id, "the-code#"+state)
			if tt.invalid {
				if !errors.Is(err, creds.ErrInvalidRateLimitTier) {
					t.Fatalf("invalid profile tier error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tok.RateLimitTier != tt.want || tok.SubscriptionType != "max" {
				t.Fatalf("tier=%q plan=%q", tok.RateLimitTier, tok.SubscriptionType)
			}
		})
	}
}

func TestExchangeAcceptsCallbackURLAndBareCode(t *testing.T) {
	fake := &fakeAnthropic{orgType: "claude_pro", scope: "user:profile user:inference"}
	fake.serve(t)
	f := New()

	id, state, challenge := start(t, f)
	fake.challenge = challenge
	if tok, err := f.Exchange(context.Background(), id, RedirectURL+"?code=the-code&state="+state); err != nil || tok.SubscriptionType != "pro" {
		t.Fatalf("callback URL: %+v %v", tok, err)
	}
	id, _, challenge = start(t, f)
	fake.challenge = challenge
	if _, err := f.Exchange(context.Background(), id, "the-code"); err != nil {
		t.Fatalf("bare code: %v", err)
	}
}

func TestExchangeRejections(t *testing.T) {
	ctx := context.Background()
	t.Run("state from another sign-in", func(t *testing.T) {
		fake := &fakeAnthropic{orgType: "claude_max", scope: "user:inference"}
		fake.serve(t)
		f := New()
		id, _, challenge := start(t, f)
		fake.challenge = challenge
		if _, err := f.Exchange(ctx, id, "the-code#someone-elses-state"); err == nil || !strings.Contains(err.Error(), "different sign-in") {
			t.Fatalf("err = %v", err)
		}
		if fake.exchanges != 0 {
			t.Fatal("a mismatched state must be refused before calling Anthropic")
		}
	})
	t.Run("expired session", func(t *testing.T) {
		f := New()
		id, state, _ := start(t, f)
		f.now = func() time.Time { return time.Now().Add(sessionTTL + time.Minute) }
		if _, err := f.Exchange(ctx, id, "the-code#"+state); err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("code rejected upstream does not echo the body", func(t *testing.T) {
		fake := &fakeAnthropic{orgType: "claude_max", scope: "user:inference"}
		fake.serve(t)
		f := New()
		id, state, _ := start(t, f)
		fake.challenge = "not-the-challenge"
		_, err := f.Exchange(ctx, id, "the-code#"+state)
		if err == nil || !strings.Contains(err.Error(), "invalid_grant: bad code") || strings.Contains(err.Error(), "sk-ant") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing inference scope", func(t *testing.T) {
		fake := &fakeAnthropic{orgType: "claude_max", scope: "user:profile"}
		fake.serve(t)
		f := New()
		id, state, challenge := start(t, f)
		fake.challenge = challenge
		if _, err := f.Exchange(ctx, id, "the-code#"+state); err == nil || !strings.Contains(err.Error(), "user:inference") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("account without a subscription", func(t *testing.T) {
		fake := &fakeAnthropic{orgType: "api_individual", scope: "user:inference"}
		fake.serve(t)
		f := New()
		id, state, challenge := start(t, f)
		fake.challenge = challenge
		if _, err := f.Exchange(ctx, id, "the-code#"+state); err == nil || !strings.Contains(err.Error(), "no Claude subscription") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("empty code", func(t *testing.T) {
		f := New()
		id, _, _ := start(t, f)
		if _, err := f.Exchange(ctx, id, "   "); err == nil {
			t.Fatal("an empty code must be refused")
		}
	})
}

func TestSubscriptionTypeMapping(t *testing.T) {
	for in, want := range map[string]string{
		"claude_max": "max", "claude_pro": "pro", "claude_team": "team",
		"claude_enterprise": "enterprise", "claude_ultra": "ultra",
	} {
		if got, err := subscriptionType(in); err != nil || got != want {
			t.Errorf("subscriptionType(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "claude_", "api"} {
		if _, err := subscriptionType(in); err == nil {
			t.Errorf("subscriptionType(%q) accepted a non-subscription", in)
		}
	}
}

func TestSessionsAreBounded(t *testing.T) {
	f := New()
	clock := time.Now()
	f.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	first, _, err := f.Start()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxSessions; i++ {
		if _, _, err := f.Start(); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.sessions) != maxSessions {
		t.Fatalf("%d sessions held, want the cap of %d", len(f.sessions), maxSessions)
	}
	if _, ok := f.sessions[first]; ok {
		t.Fatal("the oldest session should have been evicted")
	}
	f.Cancel(first)
}

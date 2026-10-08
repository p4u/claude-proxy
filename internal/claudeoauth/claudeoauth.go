// Package claudeoauth signs an Anthropic subscription in from the web UI,
// using the same OAuth client and manual-code flow as Claude Code's own
// `/login`: the browser opens claude.com's authorize page, Anthropic shows an
// authentication code on its own callback page, and the operator pastes that
// code back. No loopback listener is involved, so it works for a proxy on a
// remote host exactly as for a local one.
//
// The PKCE verifier never leaves this process: Start keeps it in memory keyed
// by an opaque session ID, and Exchange consumes it. Nothing is persisted
// until the code has been exchanged and the resulting token has proven itself
// against Anthropic's profile endpoint.
package claudeoauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
)

// The values below mirror Claude Code's production OAuth configuration
// (`CLAUDE_AI_AUTHORIZE_URL`, `MANUAL_REDIRECT_URL` and the subscription login
// scope set). The redirect URI must be the manual one: it is the only
// registered redirect that is not a localhost port, and Anthropic's page there
// displays the code for the user to copy.
const (
	AuthorizeURL = "https://claude.com/cai/oauth/authorize"
	RedirectURL  = "https://platform.claude.com/oauth/code/callback"
)

// Scopes is the scope set Claude Code requests for a claude.ai subscription
// login, so a token minted here is interchangeable with one copied out of
// .credentials.json — the refresher keeps the same grant either way.
var Scopes = []string{
	"org:create_api_key", "user:profile", "user:inference",
	"user:sessions:claude_code", "user:mcp_servers", "user:file_upload", "user:plugins",
}

// ProfileURL is a var so tests can point it at a fake server. The token
// endpoint is shared with the refresher (creds.TokenURL).
var ProfileURL = "https://api.anthropic.com/api/oauth/profile"

const (
	sessionTTL  = 10 * time.Minute
	maxSessions = 32
)

// Tokens is a verified login, ready to be stored.
type Tokens struct {
	AccessToken      string
	RefreshToken     string
	ExpiresAt        time.Time
	SubscriptionType string // "max", "pro", "team", "enterprise"
	RateLimitTier    string // profile metadata; empty when not published
	Email            string
}

type pending struct {
	verifier string
	state    string
	created  time.Time
}

// Flow holds in-flight logins. The zero value is not usable; call New.
type Flow struct {
	mu       sync.Mutex
	sessions map[string]pending
	client   *http.Client
	now      func() time.Time
}

// New returns an empty Flow.
func New() *Flow {
	return &Flow{
		sessions: map[string]pending{},
		client:   &http.Client{Timeout: 30 * time.Second},
		now:      time.Now,
	}
}

// SetHTTPClient swaps the client used for the token and profile calls (tests).
func (f *Flow) SetHTTPClient(c *http.Client) { f.client = c }

// Start opens a login and returns its session ID and the authorize URL to
// send the browser to.
func (f *Flow) Start() (id, authorizeURL string, err error) {
	verifier, err := randomToken(32)
	if err != nil {
		return "", "", err
	}
	state, err := randomToken(32)
	if err != nil {
		return "", "", err
	}
	id, err = randomToken(18)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))

	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", creds.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", RedirectURL)
	q.Set("scope", strings.Join(Scopes, " "))
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked()
	if len(f.sessions) >= maxSessions {
		f.evictOldestLocked()
	}
	f.sessions[id] = pending{verifier: verifier, state: state, created: f.now()}
	return id, AuthorizeURL + "?" + q.Encode(), nil
}

// Cancel forgets a pending login. Unknown IDs are ignored.
func (f *Flow) Cancel(id string) {
	f.mu.Lock()
	delete(f.sessions, id)
	f.mu.Unlock()
}

// Exchange trades the code Anthropic displayed for tokens, then reads the
// account profile both to learn the plan and to prove the token works.
//
// The session is consumed before any network call: an authorization code is
// single-use upstream, so a retry with the same session could never succeed
// and the operator must start a fresh sign-in after any failure.
func (f *Flow) Exchange(ctx context.Context, id, pasted string) (*Tokens, error) {
	f.mu.Lock()
	f.pruneLocked()
	p, ok := f.sessions[id]
	delete(f.sessions, id)
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("sign-in session expired or unknown; start the sign-in again")
	}

	code, state := parseCode(pasted)
	if code == "" {
		return nil, errors.New("paste the authentication code shown after approving access")
	}
	if state != "" && state != p.state {
		return nil, errors.New("that code belongs to a different sign-in; start the sign-in again")
	}

	tok, err := f.exchange(ctx, code, p)
	if err != nil {
		return nil, err
	}
	if !creds.HasOATMarker(tok.AccessToken) {
		return nil, errors.New("the token Anthropic returned is not a Claude subscription token")
	}
	if !hasScope(tok.Scope, "user:inference") {
		return nil, fmt.Errorf("the grant lacks the user:inference scope (got %q)", tok.Scope)
	}

	prof, err := f.profile(ctx, tok.AccessToken)
	if err != nil {
		return nil, err
	}
	sub, err := subscriptionType(prof.Organization.OrganizationType)
	if err != nil {
		return nil, err
	}
	tier, err := creds.NormalizeRateLimitTier(prof.Organization.RateLimitTier)
	if err != nil {
		return nil, fmt.Errorf("account profile: %w", err)
	}
	email := prof.Account.Email
	if email == "" {
		email = tok.Account.Email
	}
	return &Tokens{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		// Same safety margin the refresher applies to every expiry it stores.
		ExpiresAt:        f.now().Add(time.Duration(tok.ExpiresIn)*time.Second - 5*time.Minute),
		SubscriptionType: sub,
		RateLimitTier:    tier,
		Email:            email,
	}, nil
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Account      struct {
		Email string `json:"email_address"`
	} `json:"account"`
}

func (f *Flow) exchange(ctx context.Context, code string, p pending) (*tokenResp, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  RedirectURL,
		"client_id":     creds.ClientID,
		"code_verifier": p.verifier,
		"state":         p.state,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, creds.TokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("the code was rejected by Anthropic (%d) — it may have expired or been used already; start the sign-in again: %s",
			resp.StatusCode, upstreamError(raw))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, upstreamError(raw))
	}
	var t tokenResp
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("token exchange decode: %w", err)
	}
	if t.AccessToken == "" || t.RefreshToken == "" || t.ExpiresIn <= 0 {
		return nil, errors.New("token exchange returned no usable token pair")
	}
	return &t, nil
}

type profileResp struct {
	Account struct {
		Email string `json:"email"`
	} `json:"account"`
	Organization struct {
		OrganizationType string `json:"organization_type"`
		// Claude Code 2.1.280 reads this exact field from /api/oauth/profile
		// into claudeAiOauth.rateLimitTier. Omitted/null stays unknown.
		RateLimitTier string `json:"rate_limit_tier"`
	} `json:"organization"`
}

func (f *Flow) profile(ctx context.Context, accessToken string) (*profileResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ProfileURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("account profile: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the new token could not read its account profile (%d): %s", resp.StatusCode, upstreamError(raw))
	}
	var p profileResp
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("account profile decode: %w", err)
	}
	return &p, nil
}

// subscriptionType maps the profile's organization type the way Claude Code
// does. An unrecognised "claude_*" type keeps its suffix so a plan launched
// after this build still imports; anything else is not a subscription.
func subscriptionType(orgType string) (string, error) {
	switch orgType {
	case "claude_max":
		return "max", nil
	case "claude_pro":
		return "pro", nil
	case "claude_team":
		return "team", nil
	case "claude_enterprise":
		return "enterprise", nil
	}
	if s, ok := strings.CutPrefix(orgType, "claude_"); ok && s != "" {
		return s, nil
	}
	if orgType == "" {
		return "", errors.New("this account has no Claude subscription")
	}
	return "", fmt.Errorf("this account has no Claude subscription (organization type %q)", orgType)
}

// parseCode accepts what Anthropic's callback page shows ("code#state"), a
// full callback URL, or a bare code.
func parseCode(s string) (code, state string) {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		q := u.Query()
		return strings.TrimSpace(q.Get("code")), strings.TrimSpace(q.Get("state"))
	}
	code, state, _ = strings.Cut(s, "#")
	return strings.TrimSpace(code), strings.TrimSpace(state)
}

func hasScope(scopes, want string) bool {
	for _, s := range strings.Fields(scopes) {
		if s == want {
			return true
		}
	}
	return false
}

// upstreamError keeps error messages short and free of token material: only
// the OAuth error fields are echoed, never the raw body, and capped in length.
func upstreamError(raw []byte) string {
	msg := oauthErrorText(raw)
	if r := []rune(msg); len(r) > 160 {
		msg = string(r[:160]) + "…"
	}
	return msg
}

func oauthErrorText(raw []byte) string {
	var e struct {
		Error            any    `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return "unexpected response"
	}
	switch v := e.Error.(type) {
	case string:
		if e.ErrorDescription != "" {
			return v + ": " + e.ErrorDescription
		}
		return v
	case map[string]any:
		if msg, ok := v["message"].(string); ok {
			return msg
		}
	}
	return "unexpected response"
}

func (f *Flow) pruneLocked() {
	cutoff := f.now().Add(-sessionTTL)
	for id, p := range f.sessions {
		if p.created.Before(cutoff) {
			delete(f.sessions, id)
		}
	}
}

func (f *Flow) evictOldestLocked() {
	var oldest string
	var at time.Time
	for id, p := range f.sessions {
		if oldest == "" || p.created.Before(at) {
			oldest, at = id, p.created
		}
	}
	delete(f.sessions, oldest)
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

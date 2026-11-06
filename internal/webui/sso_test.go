package webui

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	vocauth "github.com/vocdoni/oauth/go"

	"github.com/p4u/claude-proxy/internal/store"
)

const (
	ssoClientID = "claude-proxy"
	ssoOrigin   = "http://proxy.test"
)

// fakeOP is a minimal OpenID Connect provider: discovery, JWKS and a token
// endpoint that signs an ID token for whoever the test says is signing in.
type fakeOP struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu    sync.Mutex
	nonce string
	email string
}

func newFakeOP(t *testing.T) *fakeOP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	op := &fakeOP{key: key, email: "alice@vocdoni.org"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		base := op.srv.URL
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/auth",
			"token_endpoint":                        base + "/token",
			"jwks_uri":                              base + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		op.mu.Lock()
		claims := map[string]any{
			"iss": op.srv.URL, "aud": ssoClientID, "sub": "sub-1",
			"email": op.email, "email_verified": true, "name": "Alice",
			"nonce": op.nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
		}
		op.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 300,
			"id_token": op.sign(t, claims),
		})
	})
	op.srv = httptest.NewServer(mux)
	t.Cleanup(op.srv.Close)
	return op
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (op *fakeOP) sign(t *testing.T, claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	signing := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, op.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Error(err)
	}
	return signing + "." + b64(sig)
}

func newSSOServer(t *testing.T, issuer string) (*store.DB, http.Handler) {
	t.Helper()
	return newSSOServerWith(t, issuer, nil)
}

// newSSOServerWith is newSSOServer with a hook to adjust the SSO config.
func newSSOServerWith(t *testing.T, issuer string, mut func(*SSOConfig)) (*store.DB, http.Handler) {
	t.Helper()
	db, _ := newTestServer(t)
	keys := base64.StdEncoding.EncodeToString(make([]byte, vocauth.KeySize))
	cfg := SSOConfig{
		Issuer: issuer, ClientID: ssoClientID, ClientSecret: "secret",
		RedirectURL: ssoOrigin + "/auth/callback", CookieKeys: keys, Insecure: true,
	}
	if mut != nil {
		mut(&cfg)
	}
	sso, err := NewSSO(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The password is ignored once SSO is set.
	return db, NewWithOptions(db, nil, Options{Password: testPassword, SSO: sso})
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.Value != "" {
			return c
		}
	}
	return nil
}

// signIn walks the browser side of the code flow and returns the session
// cookie (nil when refused) and the callback's response.
func signIn(t *testing.T, h http.Handler, op *fakeOP) (*http.Cookie, *httptest.ResponseRecorder) {
	t.Helper()
	w := do(t, h, http.MethodGet, "/auth/login?return_to=%2F%23%2Fusers", "", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), op.srv.URL+"/auth?") || q.Get("client_id") != ssoClientID ||
		q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != ssoOrigin+"/auth/callback" {
		t.Fatalf("authorize redirect = %s", loc)
	}
	op.mu.Lock()
	op.nonce = q.Get("nonce")
	op.mu.Unlock()
	state := cookieNamed(w, vocauth.StateCookie)
	if state == nil {
		t.Fatal("no state cookie")
	}
	cb := do(t, h, http.MethodGet, "/auth/callback?code=c1&state="+url.QueryEscape(q.Get("state")), "", state)
	return cookieNamed(cb, vocauth.SessionCookie), cb
}

func TestSSOReplacesPassword(t *testing.T) {
	op := newFakeOP(t)
	db, h := newSSOServer(t, op.srv.URL)

	for _, p := range []string{"/api/login", "/api/logout"} {
		if w := do(t, h, http.MethodPost, p, `{"password":"`+testPassword+`"}`, nil); w.Code != http.StatusNotFound {
			t.Fatalf("POST %s = %d, want 404 (no password in SSO mode)", p, w.Code)
		}
	}
	w := do(t, h, http.MethodGet, "/api/session", "", nil)
	if !strings.Contains(w.Body.String(), `"authenticated":false`) || !strings.Contains(w.Body.String(), `"method":"sso"`) {
		t.Fatalf("session = %s", w.Body.String())
	}
	if w := do(t, h, http.MethodGet, "/api/overview", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("overview without session = %d", w.Code)
	}
	// A password session from the same deployment opens nothing either.
	pw := loginCookie(t, New(db, nil, testPassword, false))
	if w := do(t, h, http.MethodGet, "/api/overview", "", pw); w.Code != http.StatusUnauthorized {
		t.Fatalf("overview with a password cookie = %d, want 401", w.Code)
	}
}

func TestSSOSignInFlow(t *testing.T) {
	op := newFakeOP(t)
	_, h := newSSOServer(t, op.srv.URL)

	sess, cb := signIn(t, h, op)
	if sess == nil {
		t.Fatalf("callback: %d %s", cb.Code, cb.Body.String())
	}
	if got := cb.Header().Get("Location"); got != "/#/users" {
		t.Fatalf("callback returns to %q, want /#/users", got)
	}
	w := do(t, h, http.MethodGet, "/api/session", "", sess)
	if !strings.Contains(w.Body.String(), `"authenticated":true`) || !strings.Contains(w.Body.String(), "alice@vocdoni.org") {
		t.Fatalf("session = %s", w.Body.String())
	}
	if w := do(t, h, http.MethodGet, "/api/overview", "", sess); w.Code != http.StatusOK {
		t.Fatalf("overview = %d %s", w.Code, w.Body.String())
	}

	// State-changing calls must come from the UI's own origin.
	post := func(origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/users", strings.NewReader(`{"name":"bob"}`))
		r.AddCookie(sess)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, o := range []string{"", "https://evil.test"} {
		if w := post(o); w.Code != http.StatusForbidden {
			t.Fatalf("POST from origin %q = %d, want 403", o, w.Code)
		}
	}
	if w := post(ssoOrigin); w.Code != http.StatusOK {
		t.Fatalf("same-origin POST = %d %s", w.Code, w.Body.String())
	}

	// Sign-out is the kit's same-origin POST.
	r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	r.AddCookie(sess)
	r.Header.Set("Origin", ssoOrigin)
	out := httptest.NewRecorder()
	h.ServeHTTP(out, r)
	if out.Code != http.StatusSeeOther || !strings.Contains(out.Header().Get("Set-Cookie"), vocauth.SessionCookie+"=;") {
		t.Fatalf("logout = %d %v", out.Code, out.Header())
	}
}

func TestSSORefusesOtherDomains(t *testing.T) {
	op := newFakeOP(t)
	op.email = "mallory@example.com"
	_, h := newSSOServer(t, op.srv.URL)
	sess, cb := signIn(t, h, op)
	if sess != nil || cb.Code != http.StatusForbidden {
		t.Fatalf("outside account: code %d, session %v", cb.Code, sess)
	}
}

func TestPasswordModeReportsMethod(t *testing.T) {
	_, h := newTestServer(t)
	if w := do(t, h, http.MethodGet, "/api/session", "", nil); !strings.Contains(w.Body.String(), `"method":"password"`) {
		t.Fatalf("session = %s", w.Body.String())
	}
	// /auth/* is not a sign-in route without SSO: it is the SPA like any path.
	if w := do(t, h, http.MethodGet, "/auth/login", "", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("/auth/login in password mode = %d", w.Code)
	}
}

func TestNewSSORejectsBadConfig(t *testing.T) {
	keys := base64.StdEncoding.EncodeToString(make([]byte, vocauth.KeySize))
	good := SSOConfig{ClientID: "c", ClientSecret: "s", RedirectURL: "https://p.test/auth/callback", CookieKeys: keys}
	if _, err := NewSSO(good); err != nil {
		t.Fatalf("good config: %v", err)
	}
	for name, mut := range map[string]func(*SSOConfig){
		"callback path": func(c *SSOConfig) { c.RedirectURL = "https://p.test/callback" },
		"short key":     func(c *SSOConfig) { c.CookieKeys = "c2hvcnQ=" },
		"no secret":     func(c *SSOConfig) { c.ClientSecret = "" },
		"plain http":    func(c *SSOConfig) { c.RedirectURL = "http://p.test/auth/callback" },
		"bad domain":    func(c *SSOConfig) { c.LoginDomains = []string{"a@b"} },
	} {
		c := good
		mut(&c)
		if _, err := NewSSO(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSSOAdminEmailsGateAPI(t *testing.T) {
	op := newFakeOP(t)
	_, h := newSSOServerWith(t, op.srv.URL, func(c *SSOConfig) {
		c.AdminEmails = []string{" Boss@Vocdoni.org "}
	})

	// A member of the login domain signs in but is not an administrator.
	member, cb := signIn(t, h, op)
	if member == nil {
		t.Fatalf("member sign-in refused: %d %s", cb.Code, cb.Body.String())
	}
	w := do(t, h, http.MethodGet, "/api/overview", "", member)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not an administrator") {
		t.Fatalf("member /api/overview = %d %s, want 403 not an administrator", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodGet, "/api/users", "", member); w.Code != http.StatusForbidden {
		t.Fatalf("member /api/users = %d, want 403", w.Code)
	}

	op.mu.Lock()
	op.email = "boss@vocdoni.org"
	op.mu.Unlock()
	admin, cb := signIn(t, h, op)
	if admin == nil {
		t.Fatalf("admin sign-in refused: %d %s", cb.Code, cb.Body.String())
	}
	if w := do(t, h, http.MethodGet, "/api/overview", "", admin); w.Code != http.StatusOK {
		t.Fatalf("admin /api/overview = %d %s", w.Code, w.Body.String())
	}
}

func TestSSOLoginDomains(t *testing.T) {
	op := newFakeOP(t)
	op.email = "mallory@example.com"
	_, h := newSSOServerWith(t, op.srv.URL, func(c *SSOConfig) {
		c.LoginDomains = []string{"example.com"}
	})
	if sess, cb := signIn(t, h, op); sess == nil {
		t.Fatalf("login-domain account refused: %d %s", cb.Code, cb.Body.String())
	}
	op.mu.Lock()
	op.email = "alice@vocdoni.org"
	op.mu.Unlock()
	if sess, cb := signIn(t, h, op); sess != nil || cb.Code != http.StatusForbidden {
		t.Fatalf("account outside the login domains: code %d, session %v", cb.Code, sess)
	}
}

func TestSSOAdminListReported(t *testing.T) {
	keys := base64.StdEncoding.EncodeToString(make([]byte, vocauth.KeySize))
	cfg := SSOConfig{ClientID: "c", ClientSecret: "s", RedirectURL: "https://p.test/auth/callback", CookieKeys: keys}
	open, err := NewSSO(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if open.HasAdminList() {
		t.Fatal("no AdminEmails: HasAdminList = true")
	}
	cfg.AdminEmails = []string{"boss@vocdoni.org"}
	closed, err := NewSSO(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !closed.HasAdminList() {
		t.Fatal("AdminEmails set: HasAdminList = false")
	}
}

// An administrator outside every login domain could never sign in, so the
// proxy refuses to start rather than lock the operator out silently.
func TestNewSSOAdminOutsideLoginDomains(t *testing.T) {
	keys := base64.StdEncoding.EncodeToString(make([]byte, vocauth.KeySize))
	cfg := SSOConfig{
		ClientID: "c", ClientSecret: "s", RedirectURL: "https://p.test/auth/callback", CookieKeys: keys,
		AdminEmails: []string{"ops@example.com"},
	}
	_, err := NewSSO(cfg)
	if err == nil {
		t.Fatal("admin outside the default login domain: accepted")
	}
	for _, want := range []string{"ops@example.com", "vocdoni.org", "CLAUDE_PROXY_LOGIN_DOMAINS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	cfg.LoginDomains = []string{"vocdoni.org", "example.com"}
	if _, err := NewSSO(cfg); err != nil {
		t.Fatalf("admin inside an added login domain: %v", err)
	}
}

// sessionAdmin reads the "admin" field of GET /api/session; present reports
// whether the field was in the reply at all.
func sessionAdmin(t *testing.T, h http.Handler, c *http.Cookie) (admin, present bool) {
	t.Helper()
	w := do(t, h, http.MethodGet, "/api/session", "", c)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("session reply %q: %v", w.Body.String(), err)
	}
	v, present := body["admin"]
	admin, _ = v.(bool)
	return admin, present
}

func TestSSOSessionReportsAdmin(t *testing.T) {
	op := newFakeOP(t)
	_, h := newSSOServerWith(t, op.srv.URL, func(c *SSOConfig) {
		c.AdminEmails = []string{"boss@vocdoni.org"}
	})
	member, cb := signIn(t, h, op)
	if member == nil {
		t.Fatalf("member sign-in refused: %d %s", cb.Code, cb.Body.String())
	}
	if admin, present := sessionAdmin(t, h, member); !present || admin {
		t.Fatalf("member session: admin=%v present=%v, want false/true", admin, present)
	}
	op.mu.Lock()
	op.email = "boss@vocdoni.org"
	op.mu.Unlock()
	boss, cb := signIn(t, h, op)
	if boss == nil {
		t.Fatalf("admin sign-in refused: %d %s", cb.Code, cb.Body.String())
	}
	if admin, present := sessionAdmin(t, h, boss); !present || !admin {
		t.Fatalf("admin session: admin=%v present=%v, want true/true", admin, present)
	}
}

func TestPasswordSessionReportsAdmin(t *testing.T) {
	_, h := newTestServer(t)
	if admin, present := sessionAdmin(t, h, loginCookie(t, h)); !present || !admin {
		t.Fatalf("password session: admin=%v present=%v, want true/true", admin, present)
	}
}

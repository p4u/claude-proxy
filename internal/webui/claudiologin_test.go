package webui

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	vocauth "github.com/vocdoni/oauth/go"

	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// fakeLogins is the claudio API's pending-login store as the page sees it.
type fakeLogins struct {
	mu       sync.Mutex
	pending  map[string]bool
	approved map[string]string
	denied   map[string]bool
}

func newFakeLogins(codes ...string) *fakeLogins {
	f := &fakeLogins{pending: map[string]bool{}, approved: map[string]string{}, denied: map[string]bool{}}
	for _, c := range codes {
		f.pending[c] = true
	}
	return f
}

func fakeNorm(c string) string { return strings.ToUpper(strings.ReplaceAll(c, "-", "")) }

func (f *fakeLogins) Check(c string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending[fakeNorm(c)]
}

func (f *fakeLogins) Approve(c, userID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.pending[fakeNorm(c)] {
		return false
	}
	delete(f.pending, fakeNorm(c))
	f.approved[fakeNorm(c)] = userID
	return true
}

func (f *fakeLogins) Deny(c string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.pending[fakeNorm(c)] {
		return false
	}
	delete(f.pending, fakeNorm(c))
	f.denied[fakeNorm(c)] = true
	return true
}

// newLoginServer is an SSO server with device login on, boss@vocdoni.org as
// the only administrator and a 1M/24h default limit for new members.
func newLoginServer(t *testing.T, issuer string, logins LoginApprover, domains ...string) (*store.DB, http.Handler) {
	t.Helper()
	db, _ := newTestServer(t)
	keys := base64.StdEncoding.EncodeToString(make([]byte, vocauth.KeySize))
	sso, err := NewSSO(SSOConfig{
		Issuer: issuer, ClientID: ssoClientID, ClientSecret: "secret",
		RedirectURL: ssoOrigin + "/auth/callback", CookieKeys: keys, Insecure: true,
		AdminEmails: []string{"boss@vocdoni.org"}, LoginDomains: domains,
	})
	if err != nil {
		t.Fatal(err)
	}
	return db, NewWithOptions(db, nil, Options{
		SSO: sso, Logins: logins,
		DefaultLimit: MemberLimit{OutputTokens: 1_000_000, WindowSeconds: 86400},
	})
}

// postLogin submits the /claudio/login form the way a browser does from the
// page itself: Referrer-Policy no-referrer makes it send "Origin: null".
func postLogin(t *testing.T, h http.Handler, sess *http.Cookie, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/claudio/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "192.0.2.10:5555"
	if hdr == nil {
		hdr = map[string]string{"Origin": "null", "Sec-Fetch-Site": "same-origin"}
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if sess != nil {
		r.AddCookie(sess)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func getLoginPage(h http.Handler, sess *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/claudio/login", nil)
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	if sess != nil {
		r.AddCookie(sess)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func assertLoginHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	want := map[string]string{
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'",
		"X-Frame-Options":         "DENY",
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
	}
	for k, v := range want {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestClaudioLoginRedirectsToSignIn(t *testing.T) {
	op := newFakeOP(t)
	_, h := newLoginServer(t, op.srv.URL, newFakeLogins())
	w := getLoginPage(h, nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/auth/login?return_to=%2Fclaudio%2Flogin" {
		t.Fatalf("unauthenticated GET = %d %q", w.Code, w.Header().Get("Location"))
	}
	assertLoginHeaders(t, w)
	if w := postLogin(t, h, nil, url.Values{"action": {"lookup"}, "code": {"K7PQ3WXM"}}, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST = %d", w.Code)
	}
}

func TestClaudioLoginPage(t *testing.T) {
	op := newFakeOP(t)
	_, h := newLoginServer(t, op.srv.URL, newFakeLogins())
	sess, _ := signIn(t, h, op)
	w := getLoginPage(h, sess)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}
	assertLoginHeaders(t, w)
	body := w.Body.String()
	for _, s := range []string{`<form method="post" action="/claudio/login">`, `name="code"`, "alice@vocdoni.org"} {
		if !strings.Contains(body, s) {
			t.Errorf("page lacks %q", s)
		}
	}
	if strings.Contains(strings.ToLower(body), "<script") {
		t.Error("page has a script")
	}
	// The member is not an administrator of the web UI.
	if w := do(t, h, http.MethodGet, "/api/users", "", sess); w.Code != http.StatusForbidden {
		t.Fatalf("member /api/users = %d", w.Code)
	}
}

func TestClaudioLoginApproveCreatesMember(t *testing.T) {
	op := newFakeOP(t)
	logins := newFakeLogins("K7PQ3WXM", "ABCD2345")
	db, h := newLoginServer(t, op.srv.URL, logins)
	ctx := context.Background()
	// An admin already used the address as a plain name.
	if _, err := usertoken.Create(ctx, db, "alice@vocdoni.org"); err != nil {
		t.Fatal(err)
	}
	sess, _ := signIn(t, h, op)

	w := postLogin(t, h, sess, url.Values{"action": {"lookup"}, "code": {"k7pq-3wxm"}}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("lookup = %d %s", w.Code, w.Body.String())
	}
	assertLoginHeaders(t, w)
	for _, s := range []string{"K7PQ-3WXM", "alice@vocdoni.org", "Approve only if this code is shown in your terminal",
		`value="approve"`, `value="deny"`, `type="hidden" name="code" value="K7PQ3WXM"`} {
		if !strings.Contains(w.Body.String(), s) {
			t.Errorf("approval page lacks %q", s)
		}
	}

	w = postLogin(t, h, sess, url.Values{"action": {"approve"}, "code": {"K7PQ3WXM"}}, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Approved") {
		t.Fatalf("approve = %d %s", w.Code, w.Body.String())
	}
	ut, err := usertoken.LookupByEmail(ctx, db, "alice@vocdoni.org")
	if err != nil {
		t.Fatalf("member not created: %v", err)
	}
	if ut.Name != "alice@vocdoni.org-2" || ut.LimitOutputTokens != 1_000_000 || ut.LimitWindowSeconds != 86400 {
		t.Fatalf("member = %+v", ut)
	}
	if logins.approved["K7PQ3WXM"] != ut.ID {
		t.Fatalf("approved with %q, want %q", logins.approved["K7PQ3WXM"], ut.ID)
	}
	if strings.Contains(w.Body.String(), ut.Token) {
		t.Fatal("the page shows the token")
	}

	// A second login reuses the member's token.
	if w := postLogin(t, h, sess, url.Values{"action": {"approve"}, "code": {"ABCD-2345"}}, nil); w.Code != http.StatusOK {
		t.Fatalf("second approve = %d %s", w.Code, w.Body.String())
	}
	if logins.approved["ABCD2345"] != ut.ID {
		t.Fatal("second approval used another user")
	}
	list, _ := usertoken.List(ctx, db)
	if len(list) != 2 {
		t.Fatalf("users = %d, want 2", len(list))
	}

	// A decided code cannot be decided again.
	w = postLogin(t, h, sess, url.Values{"action": {"approve"}, "code": {"K7PQ3WXM"}}, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknown, expired or already used") {
		t.Fatalf("re-approve = %d %s", w.Code, w.Body.String())
	}
}

func TestClaudioLoginDisabledMemberRefused(t *testing.T) {
	op := newFakeOP(t)
	logins := newFakeLogins("K7PQ3WXM")
	db, h := newLoginServer(t, op.srv.URL, logins)
	ctx := context.Background()
	ut, _ := usertoken.CreateWithEmail(ctx, db, "alice", "alice@vocdoni.org")
	_ = usertoken.SetStatus(ctx, db, ut.ID, usertoken.StatusDisabled)
	sess, _ := signIn(t, h, op)
	w := postLogin(t, h, sess, url.Values{"action": {"approve"}, "code": {"K7PQ3WXM"}}, nil)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "account disabled, contact an admin") {
		t.Fatalf("disabled member approve = %d %s", w.Code, w.Body.String())
	}
	if len(logins.approved) != 0 || !logins.pending["K7PQ3WXM"] {
		t.Fatal("login decided for a disabled member")
	}
}

func TestClaudioLoginDenyAndUnknown(t *testing.T) {
	op := newFakeOP(t)
	logins := newFakeLogins("K7PQ3WXM")
	db, h := newLoginServer(t, op.srv.URL, logins)
	sess, _ := signIn(t, h, op)
	w := postLogin(t, h, sess, url.Values{"action": {"lookup"}, "code": {"ZZZZ2222"}}, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknown, expired or already used") {
		t.Fatalf("unknown lookup = %d %s", w.Code, w.Body.String())
	}
	w = postLogin(t, h, sess, url.Values{"action": {"deny"}, "code": {"K7PQ3WXM"}}, nil)
	if w.Code != http.StatusOK || !logins.denied["K7PQ3WXM"] {
		t.Fatalf("deny = %d %s", w.Code, w.Body.String())
	}
	if _, err := usertoken.LookupByEmail(context.Background(), db, "alice@vocdoni.org"); err != usertoken.ErrNotFound {
		t.Fatalf("deny created a user: %v", err)
	}
	if w := postLogin(t, h, sess, url.Values{"action": {"bogus"}, "code": {"K7PQ3WXM"}}, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bogus action = %d", w.Code)
	}
}

func TestClaudioLoginRefusesCrossOrigin(t *testing.T) {
	op := newFakeOP(t)
	logins := newFakeLogins("K7PQ3WXM")
	_, h := newLoginServer(t, op.srv.URL, logins)
	sess, _ := signIn(t, h, op)
	form := url.Values{"action": {"approve"}, "code": {"K7PQ3WXM"}}
	for name, hdr := range map[string]map[string]string{
		"other origin":            {"Origin": "https://evil.test"},
		"null origin, cross-site": {"Origin": "null", "Sec-Fetch-Site": "cross-site"},
		"null origin, no fetch":   {"Origin": "null"},
		"no origin, cross-site":   {"Sec-Fetch-Site": "cross-site"},
		"nothing":                 {},
	} {
		w := postLogin(t, h, sess, form, hdr)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, w.Code)
		}
		if !strings.Contains(w.Body.String(), "browser is too old for this flow (it sends no Sec-Fetch-Site header)") {
			t.Errorf("%s: refusal page lacks the browser hint: %s", name, w.Body.String())
		}
		assertLoginHeaders(t, w)
	}
	if len(logins.approved) != 0 {
		t.Fatal("a cross-origin post approved a login")
	}
	if w := postLogin(t, h, sess, form, map[string]string{"Origin": ssoOrigin}); w.Code != http.StatusOK {
		t.Fatalf("same-origin post = %d %s", w.Code, w.Body.String())
	}
}

func TestClaudioLoginRechecksDomain(t *testing.T) {
	op := newFakeOP(t)
	op.email = "mallory@example.com"
	// A session minted while example.com was a login domain...
	_, wide := newLoginServer(t, op.srv.URL, newFakeLogins(), "vocdoni.org", "example.com")
	sess, cb := signIn(t, wide, op)
	if sess == nil {
		t.Fatalf("sign-in: %d", cb.Code)
	}
	// ...is refused once it no longer is (same cookie keys).
	logins := newFakeLogins("K7PQ3WXM")
	_, narrow := newLoginServer(t, op.srv.URL, logins)
	if w := getLoginPage(narrow, sess); w.Code != http.StatusForbidden {
		t.Fatalf("GET outside the login domains = %d", w.Code)
	}
	if w := postLogin(t, narrow, sess, url.Values{"action": {"approve"}, "code": {"K7PQ3WXM"}}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("POST outside the login domains = %d", w.Code)
	}
	if len(logins.approved) != 0 {
		t.Fatal("approved outside the login domains")
	}
}

func TestClaudioLogin404WithoutDeviceLogin(t *testing.T) {
	_, pw := newTestServer(t)
	if w := getLoginPage(pw, nil); w.Code != http.StatusNotFound {
		t.Fatalf("password mode = %d", w.Code)
	}
	op := newFakeOP(t)
	_, h := newSSOServer(t, op.srv.URL)
	if w := getLoginPage(h, nil); w.Code != http.StatusNotFound {
		t.Fatalf("SSO without logins = %d", w.Code)
	}
}

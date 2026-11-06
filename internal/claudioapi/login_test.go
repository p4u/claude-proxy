package claudioapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// loginStack is a claudio handler with device login on and a settable clock.
type loginStack struct {
	h      *Handler
	db     *store.DB
	logins *Logins
	now    time.Time
}

func newLoginStack(t *testing.T) *loginStack {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "login.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := &loginStack{db: db, now: time.Unix(1_800_000_000, 0)}
	clock := func() time.Time { return s.now }
	s.logins = NewLogins("https://proxy.test/claudio/login")
	s.logins.now = clock
	s.h = New(http.NewServeMux(), db, nil)
	s.h.SetLogins(s.logins)
	s.h.startLimit.now = clock
	s.h.pollLimit.now = clock
	return s
}

func (s *loginStack) advance(d time.Duration) { s.now = s.now.Add(d) }

func (s *loginStack) post(t *testing.T, path, ip, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("X-Forwarded-For", ip)
	rw := httptest.NewRecorder()
	s.h.ServeHTTP(rw, req)
	return rw
}

type startReply struct {
	DeviceCode string `json:"device_code"`
	UserCode   string `json:"user_code"`
	VerifyURL  string `json:"verify_url"`
	IntervalS  int    `json:"interval_s"`
	ExpiresInS int    `json:"expires_in_s"`
}

func (s *loginStack) start(t *testing.T, ip string) startReply {
	t.Helper()
	rw := s.post(t, "/v1/claudio/login/start", ip, "")
	if rw.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rw.Code, rw.Body.String())
	}
	var r startReply
	if err := json.Unmarshal(rw.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (s *loginStack) poll(t *testing.T, ip, deviceCode string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"device_code": deviceCode})
	return s.post(t, "/v1/claudio/login/poll", ip, string(b))
}

// expectPoll polls after one interval and checks the status and error code.
func (s *loginStack) expectPoll(t *testing.T, ip, deviceCode string, code int, errCode string) *httptest.ResponseRecorder {
	t.Helper()
	s.advance(LoginInterval)
	rw := s.poll(t, ip, deviceCode)
	if rw.Code != code {
		t.Fatalf("poll: %d %s, want %d %s", rw.Code, rw.Body.String(), code, errCode)
	}
	if errCode != "" {
		if got := strings.TrimSpace(rw.Body.String()); got != `{"error":"`+errCode+`"}` {
			t.Fatalf("poll body = %s, want error %s", got, errCode)
		}
		if ct := rw.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type = %q", ct)
		}
	}
	return rw
}

func TestLoginStartReply(t *testing.T) {
	s := newLoginStack(t)
	r := s.start(t, "198.51.100.1")
	if !regexp.MustCompile(`^[ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{4}-[ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{4}$`).MatchString(r.UserCode) {
		t.Fatalf("user_code = %q", r.UserCode)
	}
	if len(r.DeviceCode) != 43 || strings.ContainsAny(r.DeviceCode, "+/=") {
		t.Fatalf("device_code = %q, want 32 bytes base64url", r.DeviceCode)
	}
	if r.VerifyURL != "https://proxy.test/claudio/login" || r.IntervalS != 3 || r.ExpiresInS != 600 {
		t.Fatalf("reply = %+v", r)
	}
	// Only the hash of the device code is held.
	s.logins.mu.Lock()
	defer s.logins.mu.Unlock()
	if len(s.logins.byHash) != 1 {
		t.Fatalf("entries = %d", len(s.logins.byHash))
	}
	for _, e := range s.logins.byHash {
		if strings.Contains(fmt.Sprintf("%+v", *e), r.DeviceCode) {
			t.Fatal("entry holds the plaintext device code")
		}
	}
}

func TestLoginApprovedIsRedeemedOnce(t *testing.T) {
	s := newLoginStack(t)
	ctx := context.Background()
	ut, err := usertoken.CreateWithEmail(ctx, s.db, "alice@vocdoni.org", "alice@vocdoni.org")
	if err != nil {
		t.Fatal(err)
	}
	r := s.start(t, "198.51.100.1")
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusPreconditionRequired, "authorization_pending")

	if !s.logins.Check(strings.ToLower(r.UserCode)) {
		t.Fatal("Check: pending code not found (case and dash must be ignored)")
	}
	if !s.logins.Approve(strings.ReplaceAll(r.UserCode, "-", ""), ut.ID) {
		t.Fatal("Approve refused a pending code")
	}
	if s.logins.Approve(r.UserCode, ut.ID) || s.logins.Deny(r.UserCode) || s.logins.Check(r.UserCode) {
		t.Fatal("a decided code was decided again")
	}

	rw := s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusOK, "")
	var got struct{ Token, Email, Name string }
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Token != ut.Token || got.Email != "alice@vocdoni.org" || got.Name != "alice@vocdoni.org" {
		t.Fatalf("200 body = %s", rw.Body.String())
	}
	if rw.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("token reply is cacheable")
	}
	// Redeemed: the entry is gone.
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusNotFound, "unknown_code")
	// An approved login never rotates the token.
	if again, _ := usertoken.Get(ctx, s.db, ut.ID); again.Token != ut.Token {
		t.Fatal("token rotated")
	}
}

func TestLoginRedeemingIsExclusive(t *testing.T) {
	s := newLoginStack(t)
	dc, uc, err := s.logins.start("198.51.100.1")
	if err != nil {
		t.Fatal(err)
	}
	s.logins.Approve(uc, "utok_x")
	h := hashCode(dc)
	if st, id := s.logins.poll(h); st != pollApproved || id != "utok_x" {
		t.Fatalf("first poll = %v %q", st, id)
	}
	// A concurrent poll while the first is redeeming must not get the token.
	if st, _ := s.logins.poll(h); st != pollPending {
		t.Fatalf("second poll during redemption = %v", st)
	}
	// A failed DB read puts it back for the next poll.
	s.logins.settle(h, outcomeRetry)
	if st, _ := s.logins.poll(h); st != pollApproved {
		t.Fatalf("poll after retry = %v", st)
	}
}

func TestLoginDisabledUserRefusedAtRedemption(t *testing.T) {
	s := newLoginStack(t)
	ctx := context.Background()
	ut, _ := usertoken.CreateWithEmail(ctx, s.db, "bob", "bob@vocdoni.org")
	r := s.start(t, "198.51.100.1")
	s.logins.Approve(r.UserCode, ut.ID)
	if err := usertoken.SetStatus(ctx, s.db, ut.ID, usertoken.StatusDisabled); err != nil {
		t.Fatal(err)
	}
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusForbidden, "denied")
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusForbidden, "denied")
}

func TestLoginDeletedUserAtRedemption(t *testing.T) {
	s := newLoginStack(t)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, s.db, "carol")
	r := s.start(t, "198.51.100.1")
	s.logins.Approve(r.UserCode, ut.ID)
	if err := usertoken.Delete(ctx, s.db, ut.ID); err != nil {
		t.Fatal(err)
	}
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusNotFound, "unknown_code")
}

func TestLoginDeniedKeptTenMinutes(t *testing.T) {
	s := newLoginStack(t)
	r := s.start(t, "198.51.100.1")
	if !s.logins.Deny(r.UserCode) {
		t.Fatal("Deny refused a pending code")
	}
	if s.logins.Approve(r.UserCode, "utok_x") {
		t.Fatal("a denied code was approved")
	}
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusForbidden, "denied")
	s.advance(9 * time.Minute)
	s.logins.Sweep()
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusForbidden, "denied")
	s.advance(time.Minute)
	s.logins.Sweep()
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusNotFound, "unknown_code")
}

func TestLoginExpiredKeptTenMinutes(t *testing.T) {
	s := newLoginStack(t)
	r := s.start(t, "198.51.100.1")
	s.advance(LoginTTL)
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusGone, "expired")
	if s.logins.Check(r.UserCode) || s.logins.Approve(r.UserCode, "utok_x") {
		t.Fatal("an expired code can still be decided")
	}
	s.advance(9 * time.Minute)
	s.logins.Sweep()
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusGone, "expired")
	s.advance(time.Minute)
	s.logins.Sweep()
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusNotFound, "unknown_code")
}

func TestLoginApprovedButExpired(t *testing.T) {
	s := newLoginStack(t)
	r := s.start(t, "198.51.100.1")
	s.logins.Approve(r.UserCode, "utok_x")
	s.advance(LoginTTL)
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusGone, "expired")
}

func TestLoginSlowDownSparesIPBudget(t *testing.T) {
	s := newLoginStack(t)
	r := s.start(t, "198.51.100.1")
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusPreconditionRequired, "authorization_pending")
	for range 3 * loginPollPerMinute {
		if rw := s.poll(t, "198.51.100.1", r.DeviceCode); rw.Code != http.StatusTooManyRequests ||
			strings.TrimSpace(rw.Body.String()) != `{"error":"slow_down"}` {
			t.Fatalf("early poll: %d %s", rw.Code, rw.Body.String())
		}
	}
	// None of those consumed the per-IP budget.
	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusPreconditionRequired, "authorization_pending")
}

func TestLoginPollIPLimit(t *testing.T) {
	s := newLoginStack(t)
	for i := range loginPollPerMinute {
		if rw := s.poll(t, "198.51.100.1", fmt.Sprintf("unknown-%d", i)); rw.Code != http.StatusNotFound {
			t.Fatalf("poll %d: %d", i, rw.Code)
		}
	}
	if rw := s.poll(t, "198.51.100.1", "unknown-x"); rw.Code != http.StatusTooManyRequests {
		t.Fatalf("poll over the IP budget: %d", rw.Code)
	}
	// Other addresses have their own budget; the window resets each minute.
	if rw := s.poll(t, "198.51.100.2", "unknown-x"); rw.Code != http.StatusNotFound {
		t.Fatalf("other IP: %d", rw.Code)
	}
	s.advance(time.Minute)
	if rw := s.poll(t, "198.51.100.1", "unknown-y"); rw.Code != http.StatusNotFound {
		t.Fatalf("next window: %d", rw.Code)
	}
}

func TestLoginStartLimits(t *testing.T) {
	s := newLoginStack(t)
	for range loginStartPerMinute {
		s.start(t, "198.51.100.1")
	}
	if rw := s.post(t, "/v1/claudio/login/start", "198.51.100.1", ""); rw.Code != http.StatusTooManyRequests ||
		strings.TrimSpace(rw.Body.String()) != `{"error":"too_many_pending"}` {
		t.Fatalf("6th start in a minute: %d %s", rw.Code, rw.Body.String())
	}
	s.advance(time.Minute)
	for range maxPendingPerIP - loginStartPerMinute {
		s.start(t, "198.51.100.1")
	}
	// Eight pending from this address: a ninth is refused.
	if rw := s.post(t, "/v1/claudio/login/start", "198.51.100.1", ""); rw.Code != http.StatusTooManyRequests ||
		strings.TrimSpace(rw.Body.String()) != `{"error":"too_many_pending"}` {
		t.Fatalf("9th pending: %d %s", rw.Code, rw.Body.String())
	}
	s.start(t, "198.51.100.2")
}

func TestLoginEvictsOldestPastTotal(t *testing.T) {
	s := newLoginStack(t)
	first, _, err := s.logins.start("203.0.113.0")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxLogins; i++ {
		if _, _, err := s.logins.start(fmt.Sprintf("203.0.%d.%d", i/250, i%250)); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if st, _ := s.logins.poll(hashCode(first)); st != pollPending {
		t.Fatalf("at the cap the first entry must survive, got %v", st)
	}
	if _, _, err := s.logins.start("198.51.100.9"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.logins.poll(hashCode(first)); st != pollUnknown {
		t.Fatalf("oldest entry not evicted: %v", st)
	}
	s.logins.mu.Lock()
	n, codes, order := len(s.logins.byHash), len(s.logins.byCode), s.logins.order.Len()
	s.logins.mu.Unlock()
	if n != maxLogins || codes != maxLogins || order != maxLogins {
		t.Fatalf("sizes after eviction: %d %d %d", n, codes, order)
	}
}

func TestLoginEvictsDecidedBeforeOldest(t *testing.T) {
	s := newLoginStack(t)
	first, _, err := s.logins.start("203.0.113.0")
	if err != nil {
		t.Fatal(err)
	}
	denied, deniedCode, err := s.logins.start("203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	if !s.logins.Deny(deniedCode) {
		t.Fatal("deny failed")
	}
	for i := 2; i < maxLogins; i++ {
		if _, _, err := s.logins.start(fmt.Sprintf("203.0.%d.%d", i/250, i%250)); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if _, _, err := s.logins.start("198.51.100.9"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.logins.poll(hashCode(denied)); st != pollUnknown {
		t.Fatalf("denied entry not evicted before the oldest pending: %v", st)
	}
	if st, _ := s.logins.poll(hashCode(first)); st != pollPending {
		t.Fatalf("oldest pending login evicted while a decided one existed: %v", st)
	}
}

func TestLoginMalformedAndUnknown(t *testing.T) {
	s := newLoginStack(t)
	for _, body := range []string{"", "{", `{"device_code":""}`, `{"device_code":7}`} {
		if rw := s.post(t, "/v1/claudio/login/poll", "198.51.100.1", body); rw.Code != http.StatusBadRequest {
			t.Fatalf("body %q: %d", body, rw.Code)
		}
	}
	if rw := s.poll(t, "198.51.100.1", "never-issued"); rw.Code != http.StatusNotFound ||
		strings.TrimSpace(rw.Body.String()) != `{"error":"unknown_code"}` {
		t.Fatalf("unknown: %d %s", rw.Code, rw.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/claudio/login/start", nil)
	rw := httptest.NewRecorder()
	s.h.ServeHTTP(rw, req)
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET start: %d", rw.Code)
	}
}

func TestLoginRoutes404WithoutSSO(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "nosso.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := New(http.NewServeMux(), db, nil)
	for _, p := range []string{"/v1/claudio/login/start", "/v1/claudio/login/poll"} {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{"device_code":"x"}`)))
		if rw.Code != http.StatusNotFound {
			t.Fatalf("%s without SSO: %d", p, rw.Code)
		}
	}
}

func TestLoginRoutesSkipIdentityBucket(t *testing.T) {
	s := newLoginStack(t)
	// Exhaust the shared anonymous bucket.
	for range 20 {
		rw := httptest.NewRecorder()
		s.h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/v1/claudio", nil))
	}
	rw := httptest.NewRecorder()
	s.h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/v1/claudio", nil))
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("anon bucket not exhausted: %d", rw.Code)
	}
	s.start(t, "198.51.100.1")
}

func TestLoginExactPathsOnly(t *testing.T) {
	s := newLoginStack(t)
	for _, p := range []string{"/v1/claudio/login/start/", "/v1/claudio/login/startx", "/v1/claudio/login"} {
		rw := s.post(t, p, "198.51.100.1", "")
		if rw.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", p, rw.Code)
		}
	}
}

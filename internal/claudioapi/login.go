package claudioapi

// Device-style login for `claudio proxy login` (RFC 8628 shape):
//
//	POST /v1/claudio/login/start  → {device_code, user_code, verify_url, interval_s, expires_in_s}
//	POST /v1/claudio/login/poll   {device_code} → 428 | 429 | 200 {token,email,name} | 410 | 403 | 404
//
// The member types user_code on the proxy's /claudio/login page (internal/webui)
// after signing in with SSO and approves it there; claudio, polling with the
// device_code only it holds, then receives the member's user token.
//
// These are the only /v1/claudio routes without a credential (see
// proxy.AuthMiddleware). They skip the per-identity bucket for per-IP limits
// of their own. Pending logins live in memory only, keyed by
// SHA-256(device_code): neither the plaintext device code nor a token is ever
// held, so a memory dump or debug log cannot redeem a login.

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/usertoken"
)

const (
	loginStartPath = "/v1/claudio/login/start"
	loginPollPath  = "/v1/claudio/login/poll"

	// LoginTTL is how long a code may be approved and redeemed.
	LoginTTL = 10 * time.Minute
	// LoginInterval is the minimum gap between two polls of one device code.
	LoginInterval = 3 * time.Second
	// loginKeep is how long a denied or expired login still answers 403/410.
	loginKeep = 10 * time.Minute

	maxPendingPerIP     = 8
	maxLogins           = 4096
	loginStartPerMinute = 5
	// Room for two concurrent logins polling every 3 s.
	loginPollPerMinute = 40
	maxPollBody        = 4096

	// userCodeAlphabet has 32 letters (no I, O, 0, 1), so a random byte & 31
	// picks one without bias.
	userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	userCodeLen      = 8
)

var errTooManyPending = errors.New("too many pending logins")

type loginState int

const (
	statePending loginState = iota
	stateApproved
	stateRedeeming // one poll is reading the user; others wait
	stateDenied
)

type loginEntry struct {
	hash     [32]byte
	userCode string // userCodeLen letters, no dash
	ip       string
	state    loginState
	userID   string // set on approval; never the token
	expires  time.Time
	decided  time.Time // when denied
	lastPoll time.Time
	elem     *list.Element
}

// Logins holds the pending device logins. It is safe for concurrent use; the
// web UI's approval page and the claudio API share one instance.
type Logins struct {
	verifyURL string
	now       func() time.Time

	mu     sync.Mutex
	byHash map[[32]byte]*loginEntry
	byCode map[string]*loginEntry
	order  *list.List // *loginEntry, oldest first
}

// NewLogins creates the pending-login store. verifyURL is the approval page
// the member opens: the SSO origin + /claudio/login, with no code in it.
func NewLogins(verifyURL string) *Logins {
	return &Logins{
		verifyURL: verifyURL,
		now:       time.Now,
		byHash:    make(map[[32]byte]*loginEntry),
		byCode:    make(map[string]*loginEntry),
		order:     list.New(),
	}
}

func hashCode(deviceCode string) [32]byte { return sha256.Sum256([]byte(deviceCode)) }

// normalizeUserCode upper-cases s and drops dashes and spaces; ok is false
// unless userCodeLen letters of the alphabet remain.
func normalizeUserCode(s string) (string, bool) {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
	if len(s) != userCodeLen {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(userCodeAlphabet, rune(s[i])) {
			return "", false
		}
	}
	return s, true
}

// start creates a pending login for a client at ip and returns the plaintext
// device code (only ever in this reply) and the user code.
func (l *Logins) start(ip string) (deviceCode, userCode string, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	deviceCode = base64.RawURLEncoding.EncodeToString(raw[:])

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	pending := 0
	for _, e := range l.byHash {
		if e.ip == ip && e.state == statePending && now.Before(e.expires) {
			pending++
		}
	}
	if pending >= maxPendingPerIP {
		return "", "", errTooManyPending
	}
	for {
		var b [userCodeLen]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", "", err
		}
		for i := range b {
			b[i] = userCodeAlphabet[b[i]&31]
		}
		if _, taken := l.byCode[string(b[:])]; !taken {
			userCode = string(b[:])
			break
		}
	}
	for len(l.byHash) >= maxLogins {
		l.removeLocked(l.order.Front().Value.(*loginEntry))
	}
	e := &loginEntry{hash: hashCode(deviceCode), userCode: userCode, ip: ip, expires: now.Add(LoginTTL)}
	e.elem = l.order.PushBack(e)
	l.byHash[e.hash] = e
	l.byCode[userCode] = e
	return deviceCode, userCode, nil
}

func (l *Logins) removeLocked(e *loginEntry) {
	delete(l.byHash, e.hash)
	delete(l.byCode, e.userCode)
	l.order.Remove(e.elem)
}

// tooSoon reports whether a poll of this device code comes less than
// LoginInterval after the previous one; otherwise it records the poll.
func (l *Logins) tooSoon(hash [32]byte) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byHash[hash]
	if !ok {
		return false
	}
	now := l.now()
	if !e.lastPoll.IsZero() && now.Sub(e.lastPoll) < LoginInterval {
		return true
	}
	e.lastPoll = now
	return false
}

type pollState int

const (
	pollUnknown pollState = iota
	pollPending
	pollApproved // the caller must settle() the entry
	pollExpired
	pollDenied
)

// poll reads a login's state. An approved login moves to redeeming and its
// user id is returned to exactly one caller, which must then settle it.
func (l *Logins) poll(hash [32]byte) (pollState, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byHash[hash]
	if !ok {
		return pollUnknown, ""
	}
	switch e.state {
	case stateDenied:
		return pollDenied, ""
	case stateRedeeming:
		return pollPending, ""
	}
	if !l.now().Before(e.expires) {
		return pollExpired, ""
	}
	if e.state == stateApproved {
		e.state = stateRedeeming
		return pollApproved, e.userID
	}
	return pollPending, ""
}

type outcome int

const (
	outcomeRedeemed outcome = iota // token returned: forget the login
	outcomeGone                    // the user row is gone: forget the login
	outcomeRefused                 // the user is not active: denied from now on
	outcomeRetry                   // reading the user failed: approved again
)

// settle ends a redemption started by poll.
func (l *Logins) settle(hash [32]byte, o outcome) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byHash[hash]
	if !ok || e.state != stateRedeeming {
		return
	}
	switch o {
	case outcomeRedeemed, outcomeGone:
		l.removeLocked(e)
	case outcomeRefused:
		e.state, e.decided = stateDenied, l.now()
	case outcomeRetry:
		e.state = stateApproved
	}
}

// decidable returns the pending, unexpired login for a typed user code.
func (l *Logins) decidable(userCode string) *loginEntry {
	code, ok := normalizeUserCode(userCode)
	if !ok {
		return nil
	}
	e := l.byCode[code]
	if e == nil || e.state != statePending || !l.now().Before(e.expires) {
		return nil
	}
	return e
}

// Check reports whether a typed user code (case, dashes and spaces ignored)
// names a pending, unexpired login.
func (l *Logins) Check(userCode string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.decidable(userCode) != nil
}

// Approve marks a pending login approved for the user token userID. It
// returns false when the code is unknown, expired or already decided.
func (l *Logins) Approve(userCode, userID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.decidable(userCode)
	if e == nil {
		return false
	}
	e.state, e.userID = stateApproved, userID
	return true
}

// Deny marks a pending login denied. It returns false when the code is
// unknown, expired or already decided.
func (l *Logins) Deny(userCode string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.decidable(userCode)
	if e == nil {
		return false
	}
	e.state, e.decided = stateDenied, l.now()
	return true
}

// Sweep forgets denied logins loginKeep after the decision and every other
// login loginKeep after it expired.
func (l *Logins) Sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, e := range l.byHash {
		end := e.expires
		if e.state == stateDenied {
			end = e.decided
		}
		if !now.Before(end.Add(loginKeep)) {
			l.removeLocked(e)
		}
	}
}

// Run sweeps every minute until ctx ends.
func (l *Logins) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Sweep()
		}
	}
}

// SetLogins turns device login on. Without it both login routes answer 404
// (no SSO configured).
func (h *Handler) SetLogins(l *Logins) { h.logins = l }

// serveLogin dispatches the two credential-less login routes.
func (h *Handler) serveLogin(w http.ResponseWriter, r *http.Request) {
	if h.logins == nil {
		writeJSON(w, http.StatusNotFound, apiError("not_found", "claudio: unknown endpoint"))
		return
	}
	if r.URL.Path == loginStartPath {
		h.methodOnly(w, r, http.MethodPost, h.serveLoginStart)
		return
	}
	h.methodOnly(w, r, http.MethodPost, h.serveLoginPoll)
}

type loginStartResponse struct {
	DeviceCode string `json:"device_code"`
	UserCode   string `json:"user_code"`
	VerifyURL  string `json:"verify_url"`
	IntervalS  int    `json:"interval_s"`
	ExpiresInS int    `json:"expires_in_s"`
}

func (h *Handler) serveLoginStart(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !h.startLimit.allow(ip) {
		writeLoginErr(w, http.StatusTooManyRequests, "too_many_pending")
		return
	}
	deviceCode, userCode, err := h.logins.start(ip)
	if errors.Is(err, errTooManyPending) {
		writeLoginErr(w, http.StatusTooManyRequests, "too_many_pending")
		return
	}
	if err != nil {
		writeLoginErr(w, http.StatusInternalServerError, "server_error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, loginStartResponse{
		DeviceCode: deviceCode,
		UserCode:   userCode[:4] + "-" + userCode[4:],
		VerifyURL:  h.logins.verifyURL,
		IntervalS:  int(LoginInterval / time.Second),
		ExpiresInS: int(LoginTTL / time.Second),
	})
}

type loginTokenResponse struct {
	Token string `json:"token"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (h *Handler) serveLoginPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxPollBody)).Decode(&body); err != nil || body.DeviceCode == "" {
		writeLoginErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	hash := hashCode(body.DeviceCode)
	// Too-fast polls of a live login are answered before the IP budget is
	// touched, so one eager client cannot starve another on the same NAT.
	if h.logins.tooSoon(hash) || !h.pollLimit.allow(clientIP(r)) {
		writeLoginErr(w, http.StatusTooManyRequests, "slow_down")
		return
	}
	st, userID := h.logins.poll(hash)
	switch st {
	case pollPending:
		writeLoginErr(w, http.StatusPreconditionRequired, "authorization_pending")
	case pollExpired:
		writeLoginErr(w, http.StatusGone, "expired")
	case pollDenied:
		writeLoginErr(w, http.StatusForbidden, "denied")
	case pollUnknown:
		writeLoginErr(w, http.StatusNotFound, "unknown_code")
	case pollApproved:
		// Re-read the user: it may have been disabled or deleted since the
		// approval. The current token is returned; approval never rotates it.
		ut, err := usertoken.Get(r.Context(), h.db, userID)
		switch {
		case errors.Is(err, usertoken.ErrNotFound):
			h.logins.settle(hash, outcomeGone)
			writeLoginErr(w, http.StatusNotFound, "unknown_code")
		case err != nil:
			h.logins.settle(hash, outcomeRetry)
			writeLoginErr(w, http.StatusInternalServerError, "server_error")
		case ut.Status != usertoken.StatusActive:
			h.logins.settle(hash, outcomeRefused)
			writeLoginErr(w, http.StatusForbidden, "denied")
		default:
			h.logins.settle(hash, outcomeRedeemed)
			slog.Info("claudio login: token issued", "user", ut.Name)
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, loginTokenResponse{Token: ut.Token, Email: ut.Email, Name: ut.Name})
		}
	}
}

// writeLoginErr writes the login routes' error shape: {"error":"<code>"}.
func writeLoginErr(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

// ipLimiter caps requests per client IP in a fixed one-minute window (a copy
// of webui's signInLimiter: the two packages share no code).
type ipLimiter struct {
	max int
	now func() time.Time

	mu     sync.Mutex
	start  time.Time
	counts map[string]int
}

func newIPLimiter(max int) *ipLimiter {
	return &ipLimiter{max: max, now: time.Now, counts: make(map[string]int)}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now := l.now(); now.Sub(l.start) >= time.Minute {
		l.start, l.counts = now, make(map[string]int)
	}
	l.counts[ip]++
	return l.counts[ip] <= l.max
}

// clientIP is the first X-Forwarded-For entry, else the peer address (as
// webui's clientIP: the proxy runs behind Traefik).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

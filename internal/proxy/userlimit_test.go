package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// limitUpstream counts upstream hits so tests can prove a blocked request never
// reached Anthropic.
func limitUpstream(hits *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"type":"message","model":"claude-sonnet-4-5","usage":{"input_tokens":5,"output_tokens":7}}`))
	}
}

func mkUser(t *testing.T, db *store.DB, name string, units, window int64) *usertoken.UserToken {
	t.Helper()
	ut, err := usertoken.Create(context.Background(), db, name)
	if err != nil {
		t.Fatal(err)
	}
	if units > 0 || window > 0 {
		if err := usertoken.SetLimit(context.Background(), db, ut.ID, units, window); err != nil {
			t.Fatal(err)
		}
	}
	ut.LimitUnits, ut.LimitWindowSeconds = units, window
	return ut
}

func addUsageRow(t *testing.T, db *store.DB, userID string, ts time.Time, in, out int64) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO request_log
		  (user_token_id, credential_id, conv_id, ts, path, status_code,
		   input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
		VALUES (?, 'cred_seed', 'conv_seed', ?, '/v1/messages', 200, ?, ?, 0, 0)`,
		userID, ts.Unix(), in, out)
	if err != nil {
		t.Fatal(err)
	}
}

// msgRequest builds a POST /v1/messages request carrying the given identity.
func msgRequest(path string, id *usertoken.Identity) *http.Request {
	body := []byte(`{"metadata":{"user_id":"sess-limit"},"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	if id != nil {
		req = req.WithContext(usertoken.WithIdentity(req.Context(), id))
	}
	return req
}

func identityFor(ut *usertoken.UserToken) *usertoken.Identity {
	return &usertoken.Identity{
		UserTokenID:        ut.ID,
		UserName:           ut.Name,
		LimitUnits:         ut.LimitUnits,
		LimitWindowSeconds: ut.LimitWindowSeconds,
	}
}

func TestUserLimitBlocksOverCap(t *testing.T) {
	var hits int32
	h, cs, db, _ := setupProxy(t, limitUpstream(&hits))

	// 5000 unit cap over 1h; seed 1200 output tokens = 6000 units.
	ut := mkUser(t, db, "alice", 5000, 3600)
	addUsageRow(t, db, ut.ID, time.Now().Add(-10*time.Minute), 0, 1200)

	// Snapshot credential counters so we can prove they are untouched.
	type credState struct {
		status    string
		req, errs int64
	}
	before := map[string]credState{}
	for _, c := range cs {
		var st credState
		if err := db.QueryRow(
			`SELECT status, request_count, error_count FROM credentials WHERE id=?`,
			c.ID).Scan(&st.status, &st.req, &st.errs); err != nil {
			t.Fatal(err)
		}
		before[c.ID] = st
	}

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, msgRequest("/v1/messages", identityFor(ut)))

	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429; body=%s", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("X-Router-Reason"); got != "user-quota" {
		t.Fatalf("X-Router-Reason = %q, want user-quota", got)
	}
	ra, err := strconv.Atoi(rw.Header().Get("Retry-After"))
	if err != nil || ra < 1 {
		t.Fatalf("Retry-After = %q (%v), want an integer >= 1", rw.Header().Get("Retry-After"), err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("blocked request must not reach upstream")
	}

	// Anthropic-shaped error body.
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rw.Body.String())
	}
	if body.Type != "error" || body.Error.Type != "rate_limit_error" {
		t.Fatalf("unexpected error shape: %+v", body)
	}
	if !bytes.Contains(rw.Body.Bytes(), []byte("usage limit reached")) {
		t.Fatalf("message missing quota text: %q", body.Error.Message)
	}

	// No conversation binding was created.
	var convs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&convs); err != nil {
		t.Fatal(err)
	}
	if convs != 0 {
		t.Fatalf("blocked request created %d conversation bindings", convs)
	}

	// Credential status and counters untouched.
	for _, c := range cs {
		var st credState
		if err := db.QueryRow(
			`SELECT status, request_count, error_count FROM credentials WHERE id=?`,
			c.ID).Scan(&st.status, &st.req, &st.errs); err != nil {
			t.Fatal(err)
		}
		if st != before[c.ID] {
			t.Fatalf("credential %s changed: %+v -> %+v", c.ID, before[c.ID], st)
		}
	}

	// The blocked attempt is logged with no credential/conversation attribution
	// and zero tokens.
	var (
		credID, convID      string
		status              int
		rx, in, out, cc, cr int64
	)
	if err := db.QueryRow(`
		SELECT COALESCE(credential_id,''), conv_id, status_code, bytes_received,
		       input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens
		FROM request_log WHERE credential_id != 'cred_seed' OR credential_id IS NULL
		ORDER BY id DESC LIMIT 1`).
		Scan(&credID, &convID, &status, &rx, &in, &out, &cc, &cr); err != nil {
		t.Fatal(err)
	}
	if credID != "" || convID != "" {
		t.Fatalf("blocked row attributed to cred=%q conv=%q", credID, convID)
	}
	if status != 429 || rx != 0 || in != 0 || out != 0 || cc != 0 || cr != 0 {
		t.Fatalf("blocked row = status %d rx %d tokens %d/%d/%d/%d", status, rx, in, out, cc, cr)
	}
}

func TestUserLimitAllowsUnderCap(t *testing.T) {
	var hits int32
	h, _, db, _ := setupProxy(t, limitUpstream(&hits))

	ut := mkUser(t, db, "bob", 100000, 3600)
	addUsageRow(t, db, ut.ID, time.Now().Add(-time.Minute), 100, 10) // 150 units

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, msgRequest("/v1/messages", identityFor(ut)))
	if rw.Code != 200 {
		t.Fatalf("code = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatal("under-cap request should reach upstream")
	}
}

// Usage that predates the rolling window must not block.
func TestUserLimitIgnoresUsageOutsideWindow(t *testing.T) {
	var hits int32
	h, _, db, _ := setupProxy(t, limitUpstream(&hits))

	ut := mkUser(t, db, "carol", 5000, 3600)
	addUsageRow(t, db, ut.ID, time.Now().Add(-3*time.Hour), 0, 100000) // long expired

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, msgRequest("/v1/messages", identityFor(ut)))
	if rw.Code != 200 {
		t.Fatalf("code = %d, want 200", rw.Code)
	}
}

func TestUserLimitUnlimitedUserNeverBlocked(t *testing.T) {
	var hits int32
	h, _, db, _ := setupProxy(t, limitUpstream(&hits))

	ut := mkUser(t, db, "dave", 0, 0)
	addUsageRow(t, db, ut.ID, time.Now(), 10_000_000, 10_000_000)

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, msgRequest("/v1/messages", identityFor(ut)))
	if rw.Code != 200 {
		t.Fatalf("unlimited user blocked: %d %s", rw.Code, rw.Body.String())
	}

	// A half-configured limit (one side zero) is not a limit either.
	ut.LimitUnits, ut.LimitWindowSeconds = 1, 0
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, msgRequest("/v1/messages", identityFor(ut)))
	if rw.Code != 200 {
		t.Fatalf("half-configured limit blocked: %d", rw.Code)
	}
	ut.LimitUnits, ut.LimitWindowSeconds = 0, 3600
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, msgRequest("/v1/messages", identityFor(ut)))
	if rw.Code != 200 {
		t.Fatalf("half-configured limit blocked: %d", rw.Code)
	}
}

// The admin token is exempt even if a limit somehow rides along on the identity.
func TestUserLimitAdminExempt(t *testing.T) {
	var hits int32
	h, _, db, _ := setupProxy(t, limitUpstream(&hits))

	ut := mkUser(t, db, "erin", 100, 3600)
	addUsageRow(t, db, ut.ID, time.Now(), 0, 100000)

	id := identityFor(ut)
	id.IsAdmin = true
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, msgRequest("/v1/messages", id))
	if rw.Code != 200 {
		t.Fatalf("admin identity blocked: %d %s", rw.Code, rw.Body.String())
	}

	// Anonymous (no identity at all) is likewise unmetered.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, msgRequest("/v1/messages", nil))
	if rw.Code != 200 {
		t.Fatalf("anonymous request blocked: %d", rw.Code)
	}
}

// count_tokens is free: it is never metered nor blocked.
func TestUserLimitCountTokensNotLimited(t *testing.T) {
	var hits int32
	h, _, db, _ := setupProxy(t, limitUpstream(&hits))

	ut := mkUser(t, db, "frank", 100, 3600)
	addUsageRow(t, db, ut.ID, time.Now(), 0, 100000)

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, msgRequest("/v1/messages/count_tokens", identityFor(ut)))
	if rw.Code != 200 {
		t.Fatalf("count_tokens blocked: %d %s", rw.Code, rw.Body.String())
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatal("count_tokens should reach upstream")
	}
}

// An upstream 429 still marks the credential limited and carries no user-quota
// marker — the two 429 paths must stay distinguishable.
func TestUpstream429UnaffectedByUserLimits(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}
	h, _, db, _ := setupProxy(t, upstream)

	ut := mkUser(t, db, "gina", 1_000_000, 3600) // far under the cap
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, msgRequest("/v1/messages", identityFor(ut)))

	if rw.Code != 429 {
		t.Fatalf("code = %d, want 429 passthrough", rw.Code)
	}
	if got := rw.Header().Get("X-Router-Reason"); got == "user-quota" {
		t.Fatal("upstream 429 must not be labelled user-quota")
	}
	if got := rw.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want upstream's 5", got)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM credentials WHERE status='limited'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 limited credential, got %d", n)
	}
}

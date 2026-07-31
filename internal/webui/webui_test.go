package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

const testPassword = "s3cret"

func newTestServer(t *testing.T) (*store.DB, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, New(db, nil, testPassword, false)
}

// do issues a request and returns the recorder. cookie may be nil.
func do(t *testing.T, h http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.RemoteAddr = "192.0.2.10:5555"
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func loginCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	w := do(t, h, http.MethodPost, "/api/login", `{"password":"`+testPassword+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie set on login")
	return nil
}

func TestSessionUnauthenticated(t *testing.T) {
	_, h := newTestServer(t)
	w := do(t, h, http.MethodGet, "/api/session", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("session code = %d", w.Code)
	}
	var body struct {
		Authenticated bool `json:"authenticated"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Authenticated {
		t.Error("expected unauthenticated")
	}
}

func TestProtectedRequires401(t *testing.T) {
	_, h := newTestServer(t)
	w := do(t, h, http.MethodGet, "/api/overview", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("overview without cookie = %d, want 401", w.Code)
	}
}

func TestLoginSessionFlow(t *testing.T) {
	_, h := newTestServer(t)

	// Wrong password.
	if w := do(t, h, http.MethodPost, "/api/login", `{"password":"nope"}`, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad login = %d, want 401", w.Code)
	}

	cookie := loginCookie(t, h)

	// Session now authenticated.
	w := do(t, h, http.MethodGet, "/api/session", "", cookie)
	if !strings.Contains(w.Body.String(), "true") {
		t.Errorf("session not authenticated: %s", w.Body.String())
	}

	// Protected endpoint works with cookie.
	if w := do(t, h, http.MethodGet, "/api/overview", "", cookie); w.Code != http.StatusOK {
		t.Fatalf("overview with cookie = %d: %s", w.Code, w.Body.String())
	}

	// Tampered cookie rejected.
	bad := &http.Cookie{Name: sessionCookie, Value: cookie.Value + "x"}
	if w := do(t, h, http.MethodGet, "/api/overview", "", bad); w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered cookie = %d, want 401", w.Code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	_, h := newTestServer(t)
	// 5 failed attempts allowed, 6th is rate-limited.
	for i := 0; i < loginMaxFails; i++ {
		w := do(t, h, http.MethodPost, "/api/login", `{"password":"wrong"}`, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, w.Code)
		}
	}
	w := do(t, h, http.MethodPost, "/api/login", `{"password":"wrong"}`, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt = %d, want 429", w.Code)
	}
}

func TestStatsUsersEndpoint(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()

	ut, err := usertoken.Create(ctx, db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	seed := func(status int, tin, tout, cacheRead int64, conv string) {
		_, err := db.ExecContext(ctx, `
			INSERT INTO request_log
			  (user_token_id, credential_id, conv_id, ts, path, status_code,
			   bytes_sent, bytes_received, latency_ms,
			   model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
			VALUES (?, 'cred_1', ?, ?, '/v1/messages', ?, 10, 20, 100,
			        'claude', ?, ?, 0, ?)`,
			ut.ID, conv, now, status, tin, tout, cacheRead)
		if err != nil {
			t.Fatal(err)
		}
	}
	seed(200, 100, 50, 5, "conv-a")
	seed(200, 200, 60, 7, "conv-a")
	seed(429, 0, 0, 0, "conv-b")

	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/stats/users?period=24h", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("stats/users = %d: %s", w.Code, w.Body.String())
	}
	var rows []usertoken.UserStat
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 user row, got %d", len(rows))
	}
	r := rows[0]
	if r.Requests != 3 || r.OK != 2 || r.Errors != 1 {
		t.Errorf("counts: requests=%d ok=%d errors=%d, want 3/2/1", r.Requests, r.OK, r.Errors)
	}
	if r.TokensIn != 300 || r.TokensOut != 110 || r.CacheRead != 12 {
		t.Errorf("tokens: in=%d out=%d cacheRead=%d, want 300/110/12", r.TokensIn, r.TokensOut, r.CacheRead)
	}
	if r.Conversations != 2 {
		t.Errorf("conversations = %d, want 2", r.Conversations)
	}
	if r.AvgLatencyMs != 100 {
		t.Errorf("avg latency = %d, want 100", r.AvgLatencyMs)
	}
}

func TestStatsRequestsSeries(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "bob")
	now := time.Now().Unix()
	for i := 0; i < 4; i++ {
		_, err := db.ExecContext(ctx, `
			INSERT INTO request_log
			  (user_token_id, credential_id, conv_id, ts, path, status_code,
			   bytes_sent, bytes_received, latency_ms,
			   model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
			VALUES (?, 'cred_1', 'c', ?, '/v1/messages', 200, 0, 0, 5, 'm', 10, 5, 0, 0)`,
			ut.ID, now-int64(i))
		if err != nil {
			t.Fatal(err)
		}
	}
	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/stats/requests?period=1h&buckets=10&group_by=user", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("stats/requests = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Buckets []int64 `json:"buckets"`
		Series  []struct {
			ID       string  `json:"id"`
			Label    string  `json:"label"`
			Requests []int64 `json:"requests"`
			TokensIn []int64 `json:"tokens_in"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Buckets) != 10 {
		t.Fatalf("buckets = %d, want 10", len(resp.Buckets))
	}
	if len(resp.Series) != 1 || resp.Series[0].Label != "bob" {
		t.Fatalf("series = %+v", resp.Series)
	}
	var total, tin int64
	for i := range resp.Series[0].Requests {
		total += resp.Series[0].Requests[i]
		tin += resp.Series[0].TokensIn[i]
	}
	if total != 4 {
		t.Errorf("total requests across buckets = %d, want 4", total)
	}
	if tin != 40 {
		t.Errorf("total tokens_in = %d, want 40", tin)
	}
}

func TestStaticSPAFallback(t *testing.T) {
	_, h := newTestServer(t)
	// Unknown non-api deep link at root should serve index.html (SPA fallback),
	// not 404, so client-side routing survives a hard refresh.
	w := do(t, h, http.MethodGet, "/dashboard", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("SPA fallback = %d, want 200", w.Code)
	}
}

func TestRootServesIndex(t *testing.T) {
	_, h := newTestServer(t)
	w := do(t, h, http.MethodGet, "/", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("root = %d, want 200", w.Code)
	}
}

func TestUILegacyRedirect(t *testing.T) {
	_, h := newTestServer(t)
	cases := map[string]string{
		"/ui":                      "/",
		"/ui/":                     "/",
		"/ui/credentials":          "/credentials",
		"/ui/dashboard?tab=tokens": "/dashboard?tab=tokens",
	}
	for path, want := range cases {
		w := do(t, h, http.MethodGet, path, "", nil)
		if w.Code != http.StatusPermanentRedirect {
			t.Fatalf("%s: code = %d, want 308", path, w.Code)
		}
		if got := w.Header().Get("Location"); got != want {
			t.Fatalf("%s: Location = %q, want %q", path, got, want)
		}
	}
}

func TestStatsRequestsCustomWindow(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "carol")
	now := time.Now().Unix()
	// Three rows inside the window, one well outside (older than `from`).
	for _, ts := range []int64{now - 100, now - 200, now - 300, now - 100000} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO request_log
			  (user_token_id, credential_id, conv_id, ts, path, status_code,
			   bytes_sent, bytes_received, latency_ms,
			   model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
			VALUES (?, 'cred_1', 'c', ?, '/v1/messages', 200, 0, 0, 5, 'm', 10, 5, 0, 0)`,
			ut.ID, ts); err != nil {
			t.Fatal(err)
		}
	}
	cookie := loginCookie(t, h)
	from := now - 1000
	to := now
	url := "/api/stats/requests?buckets=10&group_by=user" +
		"&from=" + strconv.FormatInt(from, 10) + "&to=" + strconv.FormatInt(to, 10)
	w := do(t, h, http.MethodGet, url, "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("custom window = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Buckets []int64 `json:"buckets"`
		Series  []struct {
			Requests []int64 `json:"requests"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Buckets) != 10 || resp.Buckets[0] != from {
		t.Fatalf("buckets = %v (len %d), want 10 starting at %d", resp.Buckets, len(resp.Buckets), from)
	}
	var total int64
	for _, v := range resp.Series[0].Requests {
		total += v
	}
	if total != 3 {
		t.Fatalf("requests in window = %d, want 3 (the 4th row is outside)", total)
	}
}

func TestOverviewWindowAndFields(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "dave")
	now := time.Now().Unix()
	seed := func(ts int64, status int) {
		_, err := db.ExecContext(ctx, `
			INSERT INTO request_log
			  (user_token_id, credential_id, conv_id, ts, path, status_code,
			   bytes_sent, bytes_received, latency_ms,
			   model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
			VALUES (?, 'cred_1', 'c', ?, '/v1/messages', ?, 0, 0, 100, 'm', 10, 5, 3, 7)`,
			ut.ID, ts, status)
		if err != nil {
			t.Fatal(err)
		}
	}
	seed(now-100, 200) // inside 1h window
	seed(now-200, 429) // inside 1h window (error)
	seed(now-100000, 200)

	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/overview?period=1h", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("overview = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Requests int64 `json:"requests"`
		Tokens   struct {
			Input         int64 `json:"input"`
			Output        int64 `json:"output"`
			CacheRead     int64 `json:"cache_read"`
			CacheCreation int64 `json:"cache_creation"`
		} `json:"tokens"`
		AvgLatencyMs int64   `json:"avg_latency_ms"`
		ErrorRate    float64 `json:"error_rate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if resp.Requests != 2 {
		t.Fatalf("requests=%d, want 2 (1h window excludes the old row)", resp.Requests)
	}
	if resp.Tokens.Input != 20 || resp.Tokens.CacheRead != 14 || resp.Tokens.CacheCreation != 6 {
		t.Fatalf("tokens=%+v", resp.Tokens)
	}
	if resp.AvgLatencyMs != 100 {
		t.Fatalf("avg_latency_ms=%d, want 100", resp.AvgLatencyMs)
	}
	if resp.ErrorRate < 0.49 || resp.ErrorRate > 0.51 {
		t.Fatalf("error_rate=%v, want ~0.5", resp.ErrorRate)
	}
	// Old _24h field names must be gone.
	if strings.Contains(w.Body.String(), "_24h") {
		t.Fatalf("overview still emits _24h fields: %s", w.Body.String())
	}
}

func TestStatsTotals(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "erin")
	now := time.Now().Unix()
	for i := 0; i < 3; i++ {
		_, err := db.ExecContext(ctx, `
			INSERT INTO request_log
			  (user_token_id, credential_id, conv_id, ts, path, status_code,
			   bytes_sent, bytes_received, latency_ms,
			   model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
			VALUES (?, 'cred_1', 'c', ?, '/v1/messages', 200, 0, 0, 5, 'm', 10, 5, 2, 4)`,
			ut.ID, now-int64(i))
		if err != nil {
			t.Fatal(err)
		}
	}
	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/stats/totals?period=1h&buckets=12", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("stats/totals = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Buckets  []int64 `json:"buckets"`
		Requests []int64 `json:"requests"`
		Errors   []int64 `json:"errors"`
		Tokens   struct {
			Input         []int64 `json:"input"`
			Output        []int64 `json:"output"`
			CacheRead     []int64 `json:"cache_read"`
			CacheCreation []int64 `json:"cache_creation"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Buckets) != 12 || len(resp.Requests) != 12 || len(resp.Tokens.Input) != 12 {
		t.Fatalf("bucket lengths off: %d/%d/%d", len(resp.Buckets), len(resp.Requests), len(resp.Tokens.Input))
	}
	var reqs, in, cr int64
	for i := range resp.Requests {
		reqs += resp.Requests[i]
		in += resp.Tokens.Input[i]
		cr += resp.Tokens.CacheRead[i]
	}
	if reqs != 3 || in != 30 || cr != 12 {
		t.Fatalf("totals: requests=%d input=%d cache_read=%d, want 3/30/12", reqs, in, cr)
	}
}

func TestStatsSelection(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	now := time.Now().Unix()
	ca, _ := creds.Insert(ctx, db, "A", "max", "sk-ant-oat-a", "rt-a", time.Now().Add(time.Hour), 5)
	cb, _ := creds.Insert(ctx, db, "B", "max", "sk-ant-oat-b", "rt-b", time.Now().Add(time.Hour), 5)
	ins := func(id, cid string, ts int64) {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO conversations (id, credential_id, created_at, last_seen_at, request_count, status)
			VALUES (?, ?, ?, ?, 1, 'active')`, id, cid, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	ins("cv1", ca.ID, now-100)
	ins("cv2", ca.ID, now-200)
	ins("cv3", cb.ID, now-150)
	ins("cv4", ca.ID, now-100000) // outside window

	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/stats/selection?period=1h&buckets=10", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("stats/selection = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Buckets []int64 `json:"buckets"`
		Series  []struct {
			CredentialID string  `json:"credential_id"`
			Picks        []int64 `json:"picks"`
		} `json:"series"`
		Totals []struct {
			CredentialID string  `json:"credential_id"`
			Picks        int64   `json:"picks"`
			SharePct     float64 `json:"share_pct"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Buckets) != 10 {
		t.Fatalf("buckets=%d, want 10", len(resp.Buckets))
	}
	got := map[string]int64{}
	for _, tt := range resp.Totals {
		got[tt.CredentialID] = tt.Picks
	}
	if got[ca.ID] != 2 || got[cb.ID] != 1 {
		t.Fatalf("totals picks a=%d b=%d, want 2/1 (old conv excluded)", got[ca.ID], got[cb.ID])
	}
	var shareSum float64
	for _, tt := range resp.Totals {
		shareSum += tt.SharePct
	}
	if shareSum < 99.9 || shareSum > 100.1 {
		t.Fatalf("share_pct sum=%v, want ~100", shareSum)
	}
}

func TestUsageHistoryAlignedGrid(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ca, _ := creds.Insert(ctx, db, "A", "max", "sk-ant-oat-a", "rt-a", time.Now().Add(time.Hour), 5)
	cb, _ := creds.Insert(ctx, db, "B", "max", "sk-ant-oat-b", "rt-b", time.Now().Add(time.Hour), 5)
	now := time.Now().Unix()
	snap := func(cid string, ts int64, fh float64) {
		if _, err := db.ExecContext(ctx, `INSERT INTO usage_history
			(credential_id, captured_at, five_hour_pct, five_hour_resets_at,
			 seven_day_pct, seven_day_resets_at, seven_day_sonnet_pct, seven_day_sonnet_resets_at)
			VALUES (?, ?, ?, NULL, 0, NULL, 0, NULL)`, cid, ts, fh); err != nil {
			t.Fatal(err)
		}
	}
	// A has snapshots at t1,t2; B only at t2 → B must be null at t1.
	snap(ca.ID, now-200, 10)
	snap(ca.ID, now-100, 20)
	snap(cb.ID, now-100, 30)

	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/usage/history?period=1h", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("usage/history = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Buckets []int64 `json:"buckets"`
		Series  []struct {
			CredentialID string     `json:"credential_id"`
			FiveHourPct  []*float64 `json:"five_hour_pct"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Buckets) != 2 {
		t.Fatalf("buckets=%v, want 2 (union of t1,t2)", resp.Buckets)
	}
	byID := map[string][]*float64{}
	for _, s := range resp.Series {
		if len(s.FiveHourPct) != len(resp.Buckets) {
			t.Fatalf("series %s len=%d != buckets %d", s.CredentialID, len(s.FiveHourPct), len(resp.Buckets))
		}
		byID[s.CredentialID] = s.FiveHourPct
	}
	// B has no snapshot at the first bucket → null; a value at the second.
	b := byID[cb.ID]
	if b[0] != nil {
		t.Fatalf("expected B null at first bucket, got %v", *b[0])
	}
	if b[1] == nil || *b[1] != 30 {
		t.Fatalf("expected B=30 at second bucket, got %v", b[1])
	}
}

func TestUsageCurrentSelection(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ca, _ := creds.Insert(ctx, db, "A", "max", "sk-ant-oat-a", "rt-a", time.Now().Add(time.Hour), 5)
	cb, _ := creds.Insert(ctx, db, "B", "max", "sk-ant-oat-b", "rt-b", time.Now().Add(time.Hour), 5)
	now := time.Now().Unix()
	// A healthy (low usage); B saturated on 7d.
	if _, err := db.ExecContext(ctx, `INSERT INTO usage_history
		(credential_id, captured_at, five_hour_pct, five_hour_resets_at,
		 seven_day_pct, seven_day_resets_at, seven_day_sonnet_pct, seven_day_sonnet_resets_at)
		VALUES (?, ?, 10, NULL, 10, NULL, 0, NULL)`, ca.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO usage_history
		(credential_id, captured_at, five_hour_pct, five_hour_resets_at,
		 seven_day_pct, seven_day_resets_at, seven_day_sonnet_pct, seven_day_sonnet_resets_at)
		VALUES (?, ?, 5, NULL, 100, NULL, 0, NULL)`, cb.ID, now); err != nil {
		t.Fatal(err)
	}
	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/usage/current", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("usage/current = %d: %s", w.Code, w.Body.String())
	}
	var rows []struct {
		CredentialID string `json:"credential_id"`
		Selection    struct {
			Room5h    float64 `json:"room_5h"`
			Room7d    float64 `json:"room_7d"`
			Score     float64 `json:"score"`
			SharePct  float64 `json:"share_pct"`
			Saturated bool    `json:"saturated"`
		} `json:"selection"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sel := map[string]struct {
		saturated bool
		share     float64
		score     float64
	}{}
	for _, r := range rows {
		sel[r.CredentialID] = struct {
			saturated bool
			share     float64
			score     float64
		}{r.Selection.Saturated, r.Selection.SharePct, r.Selection.Score}
	}
	if !sel[cb.ID].saturated {
		t.Fatal("B should be saturated (7d=100)")
	}
	if sel[cb.ID].score != 0 {
		t.Fatalf("saturated B score=%v, want 0", sel[cb.ID].score)
	}
	// A carries the entire share (B contributes 0).
	if sel[ca.ID].share < 99.9 {
		t.Fatalf("A share=%v, want ~100", sel[ca.ID].share)
	}
}

func TestUserPromptsPaginated(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "frank")
	now := time.Now().Unix()
	ins := func(ts int64, prompt string) {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO prompt_log (user_token_id, conv_id, ts, model, prompt)
			VALUES (?, 'c', ?, 'm', ?)`, ut.ID, ts, prompt); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		ins(now-int64(i), "p"+strconv.Itoa(i)) // p0 newest
	}

	cookie := loginCookie(t, h)
	type resp struct {
		Items []struct {
			TS     string `json:"ts"`
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		} `json:"items"`
		Total   int  `json:"total"`
		Limit   int  `json:"limit"`
		Offset  int  `json:"offset"`
		HasMore bool `json:"has_more"`
	}
	get := func(url string) resp {
		t.Helper()
		w := do(t, h, http.MethodGet, url, "", cookie)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", url, w.Code, w.Body.String())
		}
		var r resp
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		return r
	}

	first := get("/api/users/" + ut.ID + "/prompts?limit=2")
	if first.Total != 5 || first.Limit != 2 || first.Offset != 0 || !first.HasMore {
		t.Fatalf("page 1 meta = %+v", first)
	}
	if len(first.Items) != 2 || first.Items[0].Prompt != "p0" {
		t.Fatalf("page 1 items = %+v (want newest-first)", first.Items)
	}

	last := get("/api/users/" + ut.ID + "/prompts?limit=2&offset=4")
	if last.HasMore {
		t.Fatalf("last page should not report has_more: %+v", last)
	}
	if len(last.Items) != 1 || last.Items[0].Prompt != "p4" {
		t.Fatalf("last page items = %+v", last.Items)
	}
}

// seedConv writes conversation_message rows for a conversation.
func seedConv(t *testing.T, db *store.DB, userID, convID string, base int64, msgs ...[2]string) {
	t.Helper()
	for i, m := range msgs {
		if _, err := db.Exec(`
			INSERT INTO conversation_message (conv_id, user_token_id, seq, role, content, model, ts)
			VALUES (?, ?, ?, ?, ?, 'claude-test', ?)`,
			convID, userID, i, m[0], m[1], base+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUserCaptureToggle(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "grace")
	cookie := loginCookie(t, h)

	users := func() []userView {
		t.Helper()
		w := do(t, h, http.MethodGet, "/api/users", "", cookie)
		if w.Code != http.StatusOK {
			t.Fatalf("users = %d: %s", w.Code, w.Body.String())
		}
		var out []userView
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		return out
	}
	if got := users(); len(got) != 1 || got[0].FullCapture {
		t.Fatalf("default full_capture should be false: %+v", got)
	}

	w := do(t, h, http.MethodPost, "/api/users/"+ut.ID+"/capture", `{"full":true}`, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("capture = %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		OK          bool `json:"ok"`
		FullCapture bool `json:"full_capture"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if !res.OK || !res.FullCapture {
		t.Fatalf("capture response = %+v", res)
	}
	if got := users(); !got[0].FullCapture {
		t.Fatal("full_capture not reflected in users list")
	}
	// The proxy identity must see it too.
	reloaded, err := usertoken.Get(ctx, db, ut.ID)
	if err != nil || !reloaded.FullCapture {
		t.Fatalf("usertoken.Get full_capture = %v (err %v)", reloaded, err)
	}

	// Toggle back off.
	do(t, h, http.MethodPost, "/api/users/"+ut.ID+"/capture", `{"full":false}`, cookie)
	if got := users(); got[0].FullCapture {
		t.Fatal("full_capture not cleared")
	}
	// Unknown user → 404.
	if w := do(t, h, http.MethodPost, "/api/users/nope/capture", `{"full":true}`, cookie); w.Code != http.StatusNotFound {
		t.Fatalf("unknown user capture = %d, want 404", w.Code)
	}

	// Prompt-suggestion blocking rides the same shape: off by default, settable,
	// visible in the list, and readable by the proxy's identity lookup.
	if got := users(); got[0].BlockSuggestions {
		t.Fatalf("default block_suggestions should be false: %+v", got)
	}
	w = do(t, h, http.MethodPost, "/api/users/"+ut.ID+"/suggestions", `{"block":true}`, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("suggestions = %d: %s", w.Code, w.Body.String())
	}
	var sres struct {
		OK               bool `json:"ok"`
		BlockSuggestions bool `json:"block_suggestions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sres)
	if !sres.OK || !sres.BlockSuggestions {
		t.Fatalf("suggestions response = %+v", sres)
	}
	if got := users(); !got[0].BlockSuggestions {
		t.Fatal("block_suggestions not reflected in users list")
	}
	if reloaded, err := usertoken.Get(ctx, db, ut.ID); err != nil || !reloaded.BlockSuggestions {
		t.Fatalf("usertoken.Get block_suggestions = %v (err %v)", reloaded, err)
	}
	do(t, h, http.MethodPost, "/api/users/"+ut.ID+"/suggestions", `{"block":false}`, cookie)
	if got := users(); got[0].BlockSuggestions {
		t.Fatal("block_suggestions not cleared")
	}
	if w := do(t, h, http.MethodPost, "/api/users/nope/suggestions", `{"block":true}`, cookie); w.Code != http.StatusNotFound {
		t.Fatalf("unknown user suggestions = %d, want 404", w.Code)
	}
}

func TestUserConversationsList(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "heidi")
	now := time.Now().Unix()

	seedConv(t, db, ut.ID, "conv-full", now-100,
		[2]string{"user", "hello"}, [2]string{"assistant", "hi there"})
	// A prompts-only conversation, newer.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO prompt_log (user_token_id, conv_id, ts, model, prompt)
		VALUES (?, 'conv-prompts', ?, 'm', 'just a prompt')`, ut.ID, now-10); err != nil {
		t.Fatal(err)
	}

	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/users/"+ut.ID+"/conversations", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("conversations = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []struct {
			ConvID   string `json:"conv_id"`
			Messages int    `json:"messages"`
			Prompts  int    `json:"prompts"`
			Model    string `json:"model"`
			Source   string `json:"source"`
			FirstTS  string `json:"first_ts"`
			LastTS   string `json:"last_ts"`
		} `json:"items"`
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if resp.Total != 2 || len(resp.Items) != 2 || resp.HasMore {
		t.Fatalf("meta = %+v", resp)
	}
	// Newest last_ts first.
	if resp.Items[0].ConvID != "conv-prompts" || resp.Items[0].Source != "prompts" {
		t.Fatalf("first item = %+v", resp.Items[0])
	}
	full := resp.Items[1]
	if full.ConvID != "conv-full" || full.Source != "full" || full.Messages != 2 || full.Prompts != 0 {
		t.Fatalf("full conv item = %+v", full)
	}
	if full.Model != "claude-test" || full.FirstTS == "" || full.LastTS == "" {
		t.Fatalf("full conv metadata = %+v", full)
	}
}

func TestRecentConversationsStillListed(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	c, _ := creds.Insert(ctx, db, "A", "max", "sk-ant-oat-a", "rt-a", time.Now().Add(time.Hour), 5)
	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO conversations (id, credential_id, created_at, last_seen_at, request_count, status)
		VALUES ('cv1', ?, ?, ?, 3, 'active')`, c.ID, now, now); err != nil {
		t.Fatal(err)
	}
	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/conversations?limit=10", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("conversations = %d: %s", w.Code, w.Body.String())
	}
	var out []convView
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(out) != 1 || out[0].ID != "cv1" || out[0].RequestCount != 3 {
		t.Fatalf("recent bindings list = %+v", out)
	}
}

func TestConversationMessagesAndFallback(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "ivan")
	now := time.Now().Unix()
	seedConv(t, db, ut.ID, "conv-1", now,
		[2]string{"user", "q1"}, [2]string{"assistant", "a1"}, [2]string{"user", "q2"})
	if _, err := db.ExecContext(ctx, `
		INSERT INTO prompt_log (user_token_id, conv_id, ts, model, prompt)
		VALUES (?, 'conv-2', ?, 'm', 'only prompt')`, ut.ID, now); err != nil {
		t.Fatal(err)
	}

	cookie := loginCookie(t, h)
	type resp struct {
		Items []struct {
			Seq     int    `json:"seq"`
			Role    string `json:"role"`
			Content string `json:"content"`
			TS      string `json:"ts"`
		} `json:"items"`
		Total   int    `json:"total"`
		HasMore bool   `json:"has_more"`
		Source  string `json:"source"`
		ConvID  string `json:"conv_id"`
		User    struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"user"`
	}
	get := func(url string) resp {
		t.Helper()
		w := do(t, h, http.MethodGet, url, "", cookie)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", url, w.Code, w.Body.String())
		}
		var r resp
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		return r
	}

	full := get("/api/conversations/conv-1/messages")
	if full.Source != "full" || full.Total != 3 || full.ConvID != "conv-1" {
		t.Fatalf("full meta = %+v", full)
	}
	if full.User.Name != "ivan" || full.User.ID != ut.ID {
		t.Fatalf("user = %+v", full.User)
	}
	if len(full.Items) != 3 || full.Items[0].Seq != 0 || full.Items[1].Role != "assistant" {
		t.Fatalf("items = %+v (want ascending seq)", full.Items)
	}

	paged := get("/api/conversations/conv-1/messages?limit=2")
	if !paged.HasMore || len(paged.Items) != 2 {
		t.Fatalf("paged = %+v", paged)
	}

	// Prompts-only conversation falls back to prompt_log as user turns.
	fb := get("/api/conversations/conv-2/messages")
	if fb.Source != "prompts" || fb.Total != 1 {
		t.Fatalf("fallback meta = %+v", fb)
	}
	if len(fb.Items) != 1 || fb.Items[0].Role != "user" || fb.Items[0].Content != "only prompt" {
		t.Fatalf("fallback items = %+v", fb.Items)
	}
}

func TestConversationExportMarkdown(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "judy")
	now := time.Now().Unix()
	seedConv(t, db, ut.ID, "4f2a9c1b-xyz", now,
		[2]string{"user", "how do I ```fence``` this?"}, [2]string{"assistant", "like so"})
	if _, err := db.ExecContext(ctx, `
		INSERT INTO prompt_log (user_token_id, conv_id, ts, model, prompt)
		VALUES (?, 'conv-p', ?, 'm', 'lonely prompt')`, ut.ID, now); err != nil {
		t.Fatal(err)
	}

	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/conversations/4f2a9c1b-xyz/export.md", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="conversation-4f2a9c1b.md"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	body := w.Body.String()
	for _, want := range []string{
		"# Conversation 4f2a9c1b",
		"| **User** | judy |",
		"| **Model** | claude-test |",
		"| **Messages** | 2 |",
		"| **Source** | full conversation |",
		"### 1 - User - ",
		"### 2 - Assistant - ",
		"how do I ```fence``` this?",
		"\n---\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("export missing %q:\n%s", want, body)
		}
	}

	// Prompts-only export carries the caveat note.
	w2 := do(t, h, http.MethodGet, "/api/conversations/conv-p/export.md", "", cookie)
	if w2.Code != http.StatusOK {
		t.Fatalf("prompts export = %d", w2.Code)
	}
	b2 := w2.Body.String()
	if !strings.Contains(b2, "| **Source** | user prompts only |") ||
		!strings.Contains(b2, "Assistant replies were not captured") {
		t.Fatalf("prompts-only export missing note:\n%s", b2)
	}

	// Unknown conversation → 404.
	if w := do(t, h, http.MethodGet, "/api/conversations/nope/export.md", "", cookie); w.Code != http.StatusNotFound {
		t.Fatalf("unknown export = %d, want 404", w.Code)
	}
}

func TestStatsWindowValidation(t *testing.T) {
	_, h := newTestServer(t)
	cookie := loginCookie(t, h)
	now := time.Now().Unix()
	cases := []string{
		"/api/stats/requests?from=200&to=100", // from >= to
		"/api/stats/requests?from=" + strconv.FormatInt(now, 10) + "&to=" + strconv.FormatInt(now, 10), // equal
		"/api/stats/requests?from=0&to=" + strconv.FormatInt(int64(91*24*3600), 10),                    // span > 90d
		"/api/stats/requests?from=abc&to=100",                                                          // unparseable
		"/api/stats/requests?from=100",                                                                 // missing to
	}
	for _, url := range cases {
		w := do(t, h, http.MethodGet, url, "", cookie)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d, want 400 (body %s)", url, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"error"`) {
			t.Fatalf("%s: body missing error field: %s", url, w.Body.String())
		}
	}
}

// Providers with no quota API still have their tokens counted: usagecapture
// records them for every provider, so the Subscriptions view reports what this
// proxy actually forwarded instead of nothing at all.
func TestUsageCurrentMeteredTokens(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()

	anth, _ := creds.Insert(ctx, db, "A", "max", "sk-ant-oat-a", "rt-a", time.Now().Add(time.Hour), 5)
	glm, _ := creds.InsertKey(ctx, db, provider.GLM, "zai", "pro", "zai-key", "", 1)

	now := time.Now().Unix()
	ins := func(credID string, ts int64, in, out, cread, ccreate int64) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO request_log
			(user_token_id, credential_id, conv_id, ts, path, status_code,
			 bytes_sent, bytes_received, latency_ms,
			 model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
			VALUES (NULL, ?, '', ?, '/v1/messages', 200, 0, 0, 0, 'glm-4.7', ?, ?, ?, ?)`,
			credID, ts, in, out, ccreate, cread); err != nil {
			t.Fatal(err)
		}
	}
	// Two inside the 5h window, one only inside the 7d window.
	ins(glm.ID, now-60, 100, 20, 500, 5)
	ins(glm.ID, now-3600, 200, 30, 700, 10)
	ins(glm.ID, now-(3*24*3600), 1000, 400, 9000, 100)
	// Another credential's traffic must not leak into the GLM totals.
	ins(anth.ID, now-60, 9999, 9999, 9999, 9999)

	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/usage/current", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var rows []struct {
		CredentialID string `json:"credential_id"`
		HasUsageAPI  bool   `json:"has_usage_api"`
		Metered      *struct {
			FiveHour struct {
				Requests                                                  int64 `json:"requests"`
				InputTokens, OutputTokens, CacheReadTokens, CacheCreation int64
			} `json:"five_hour"`
			SevenDay struct {
				Requests     int64 `json:"requests"`
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"seven_day"`
		} `json:"metered"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	var seenGLM, seenAnth bool
	for _, r := range rows {
		switch r.CredentialID {
		case glm.ID:
			seenGLM = true
			if r.Metered == nil {
				t.Fatal("a provider with no usage API must still report metered tokens")
			}
			if got := r.Metered.FiveHour.Requests; got != 2 {
				t.Errorf("5h requests = %d, want 2 (the third row is older than 5h)", got)
			}
			if got := r.Metered.SevenDay.Requests; got != 3 {
				t.Errorf("7d requests = %d, want 3", got)
			}
			if got := r.Metered.SevenDay.OutputTokens; got != 450 {
				t.Errorf("7d output tokens = %d, want 450 — other credentials must not leak in", got)
			}
		case anth.ID:
			seenAnth = true
			// Anthropic reports authoritative percentages; a locally metered
			// figure alongside them would just be a worse second number.
			if r.Metered != nil {
				t.Error("credentials with a real usage API must not carry metered totals")
			}
		}
	}
	if !seenGLM || !seenAnth {
		t.Fatalf("expected both credentials in the response, got %d rows", len(rows))
	}
}

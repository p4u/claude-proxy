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

func TestUserPromptsEndpoint(t *testing.T) {
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
	ins(now-10, "older")
	ins(now, "newest")

	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/users/"+ut.ID+"/prompts?limit=50", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("prompts = %d: %s", w.Code, w.Body.String())
	}
	var rows []struct {
		TS     string `json:"ts"`
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want 2", len(rows))
	}
	if rows[0].Prompt != "newest" {
		t.Fatalf("first prompt=%q, want newest-first ordering", rows[0].Prompt)
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

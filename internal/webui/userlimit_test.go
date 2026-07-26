package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

func seedUsage(t *testing.T, db *store.DB, userID string, ts time.Time, in, out int64) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO request_log
		  (user_token_id, credential_id, conv_id, ts, path, status_code,
		   input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
		VALUES (?, 'cred_x', 'conv_x', ?, '/v1/messages', 200, ?, ?, 0, 0)`,
		userID, ts.Unix(), in, out)
	if err != nil {
		t.Fatal(err)
	}
}

// usersByName fetches GET /api/users and indexes the result by name.
func usersByName(t *testing.T, h http.Handler, c *http.Cookie) map[string]map[string]any {
	t.Helper()
	w := do(t, h, http.MethodGet, "/api/users", "", c)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/users = %d: %s", w.Code, w.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode users: %v (%s)", err, w.Body.String())
	}
	out := map[string]map[string]any{}
	for _, u := range list {
		out[u["name"].(string)] = u
	}
	return out
}

func TestUsersListLimitFields(t *testing.T) {
	db, h := newTestServer(t)
	c := loginCookie(t, h)
	ctx := context.Background()
	now := time.Now()

	// Unlimited user with plenty of usage: must report zeros, never blocked.
	free, _ := usertoken.Create(ctx, db, "free")
	seedUsage(t, db, free.ID, now, 1_000_000, 1_000_000)

	// Healthy user: 1000 output tokens against a 10000 cap => 10%.
	healthy, _ := usertoken.Create(ctx, db, "healthy")
	if err := usertoken.SetLimit(ctx, db, healthy.ID, 10000, 3600); err != nil {
		t.Fatal(err)
	}
	seedUsage(t, db, healthy.ID, now.Add(-time.Minute), 9_000_000, 1000)

	// Blocked user: 10000 output tokens against a 5000 cap => 200%.
	over, _ := usertoken.Create(ctx, db, "over")
	if err := usertoken.SetLimit(ctx, db, over.ID, 5000, 3600); err != nil {
		t.Fatal(err)
	}
	seedUsage(t, db, over.ID, now.Add(-10*time.Minute), 0, 10000)

	users := usersByName(t, h, c)

	f := users["free"]
	if f["limit_output_tokens"].(float64) != 0 || f["limit_window_seconds"].(float64) != 0 {
		t.Fatalf("unlimited user has a limit: %+v", f)
	}
	if f["usage_output_tokens"].(float64) != 0 || f["usage_pct"].(float64) != 0 {
		t.Fatalf("unlimited user must report zero usage, got %+v", f)
	}
	if f["blocked"].(bool) || f["blocked_until"] != nil {
		t.Fatalf("unlimited user must not be blocked: %+v", f)
	}

	hu := users["healthy"]
	if hu["limit_output_tokens"].(float64) != 10000 || hu["limit_window_seconds"].(float64) != 3600 {
		t.Fatalf("healthy limit fields wrong: %+v", hu)
	}
	if hu["usage_output_tokens"].(float64) != 1000 || hu["usage_pct"].(float64) != 10 {
		t.Fatalf("healthy usage = %v (%v%%), want 1000 (10%%)", hu["usage_output_tokens"], hu["usage_pct"])
	}
	if hu["blocked"].(bool) {
		t.Fatal("healthy user must not be blocked")
	}
	// No fake reset time while under the cap.
	if hu["blocked_until"] != nil {
		t.Fatalf("blocked_until must be null under the cap, got %v", hu["blocked_until"])
	}

	ou := users["over"]
	if !ou["blocked"].(bool) {
		t.Fatalf("over-cap user not blocked: %+v", ou)
	}
	if ou["usage_output_tokens"].(float64) != 10000 || ou["usage_pct"].(float64) != 200 {
		t.Fatalf("over usage = %v (%v%%), want 10000 (200%%)", ou["usage_output_tokens"], ou["usage_pct"])
	}
	bu, ok := ou["blocked_until"].(string)
	if !ok {
		t.Fatalf("blocked user must carry blocked_until, got %v", ou["blocked_until"])
	}
	if _, err := time.Parse(time.RFC3339, bu); err != nil {
		t.Fatalf("blocked_until %q is not RFC3339: %v", bu, err)
	}
}

func TestSetUserLimitEndpoint(t *testing.T) {
	db, h := newTestServer(t)
	c := loginCookie(t, h)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "alice")
	path := "/api/users/" + ut.ID + "/limit"

	// Happy path.
	w := do(t, h, http.MethodPost, path, `{"output_tokens":1000000,"window_seconds":86400}`, c)
	if w.Code != http.StatusOK {
		t.Fatalf("set limit = %d: %s", w.Code, w.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res["ok"] != true || res["limit_output_tokens"].(float64) != 1e6 ||
		res["limit_window_seconds"].(float64) != 86400 {
		t.Fatalf("unexpected response: %+v", res)
	}
	got, err := usertoken.Get(ctx, db, ut.ID)
	if err != nil || got.LimitOutputTokens != 1_000_000 || got.LimitWindowSeconds != 86400 {
		t.Fatalf("limit not persisted: %+v %v", got, err)
	}

	// Clearing with both zero.
	w = do(t, h, http.MethodPost, path, `{"output_tokens":0,"window_seconds":0}`, c)
	if w.Code != http.StatusOK {
		t.Fatalf("clear limit = %d: %s", w.Code, w.Body.String())
	}
	got, _ = usertoken.Get(ctx, db, ut.ID)
	if usertoken.HasLimit(got.LimitOutputTokens, got.LimitWindowSeconds) {
		t.Fatal("both zero must clear the limit")
	}

	// Validation failures.
	bad := []string{
		`{"output_tokens":-1,"window_seconds":3600}`,
		`{"output_tokens":100,"window_seconds":-5}`,
		`{"output_tokens":100,"window_seconds":0}`, // one zero, one non-zero
		`{"output_tokens":0,"window_seconds":3600}`,
	}
	for _, b := range bad {
		w := do(t, h, http.MethodPost, path, b, c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %s => %d, want 400 (%s)", b, w.Code, w.Body.String())
		}
	}

	// Unknown user => 404.
	w = do(t, h, http.MethodPost, "/api/users/utok_missing/limit",
		`{"output_tokens":100,"window_seconds":3600}`, c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown user = %d, want 404", w.Code)
	}

	// Unauthenticated => 401.
	w = do(t, h, http.MethodPost, path, `{"output_tokens":0,"window_seconds":0}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", w.Code)
	}
}

// The Edit limit modal reads current usage for an arbitrary window so an
// operator can pick a cap from real numbers instead of guessing.
func TestUserWindowUsageEndpoint(t *testing.T) {
	db, h := newTestServer(t)
	c := loginCookie(t, h)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "alice") // no limit configured
	now := time.Now()
	seedUsage(t, db, ut.ID, now.Add(-30*time.Minute), 5_000_000, 900)
	seedUsage(t, db, ut.ID, now.Add(-5*time.Hour), 5_000_000, 48)

	base := "/api/users/" + ut.ID + "/usage"
	for _, tc := range []struct {
		window string
		want   float64
	}{{"3600", 900}, {"86400", 948}} {
		w := do(t, h, http.MethodGet, base+"?window_seconds="+tc.window, "", c)
		if w.Code != http.StatusOK {
			t.Fatalf("usage %s = %d: %s", tc.window, w.Code, w.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res["output_tokens"].(float64) != tc.want {
			t.Fatalf("window %s: output_tokens = %v, want %v", tc.window, res["output_tokens"], tc.want)
		}
	}

	for _, bad := range []string{"", "?window_seconds=0", "?window_seconds=abc"} {
		if w := do(t, h, http.MethodGet, base+bad, "", c); w.Code != http.StatusBadRequest {
			t.Fatalf("bad window %q = %d, want 400", bad, w.Code)
		}
	}
	if w := do(t, h, http.MethodGet, "/api/users/utok_missing/usage?window_seconds=3600", "", c); w.Code != http.StatusNotFound {
		t.Fatalf("unknown user = %d, want 404", w.Code)
	}
	if w := do(t, h, http.MethodGet, base+"?window_seconds=3600", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", w.Code)
	}
}

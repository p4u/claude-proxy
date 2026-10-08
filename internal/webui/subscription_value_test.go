package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/subscriptionstats"
)

func insertValueCredential(t *testing.T, db *store.DB, id, plan string, weight int) {
	t.Helper()
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT INTO credentials
		(id,label,subscription_type,access_token,refresh_token,expires_at,status,weight,created_at)
		VALUES (?,?,?,'private-test-access','private-test-refresh',?,'active',?,?)`,
		id, "Subscription "+id, plan, now+86400, weight, now-30*86400)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSubscriptionValueAuthenticationAndEmpty(t *testing.T) {
	_, h := newTestServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := "/api/stats/subscriptions"
		if method == http.MethodPost {
			path = "/api/credentials/cred_test/tier"
		}
		if w := do(t, h, method, path, `{ "tier": "20x" }`, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s: %d", path, w.Code)
		}
	}
	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/api/stats/subscriptions", "", cookie)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("statistics: %d %s", w.Code, w.Body.String())
	}
	var report subscriptionstats.Report
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Window != "seven_day" || report.To-report.From != 30*86400+1 || report.Requests != 0 {
		t.Fatalf("unexpected default report: %+v", report)
	}
	for _, field := range []string{"groups", "accounts", "daily", "capacity_history", "models"} {
		if !strings.Contains(w.Body.String(), `"`+field+`":[]`) {
			t.Errorf("empty %s must be [], got %s", field, w.Body.String())
		}
	}
	if w := do(t, h, http.MethodPost, "/api/stats/subscriptions", "", cookie); w.Code != http.StatusNotFound {
		t.Fatalf("read-only statistics accepted POST: %d", w.Code)
	}
}

func TestSubscriptionValueWindowValidation(t *testing.T) {
	_, h := newTestServer(t)
	cookie := loginCookie(t, h)
	now := time.Now().Unix()
	for _, query := range []string{
		"period=bad", "period=365d", "quota_window=weekly", "from=bad&to=123",
		"from=100", "to=200", "from=200&to=100", "from=0&to=7776001",
		"from=-1&to=100", "from=-9223372036854775808&to=9223372036854775807",
		fmt.Sprintf("from=%d&to=%d", now, now+86400),
	} {
		w := do(t, h, http.MethodGet, "/api/stats/subscriptions?"+query, "", cookie)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s", query, w.Code, w.Body.String())
		}
	}
	for _, query := range []string{"period=90d", "period=7d&quota_window=five_hour", "from=100&to=200&period=bad"} {
		w := do(t, h, http.MethodGet, "/api/stats/subscriptions?"+query, "", cookie)
		if w.Code != http.StatusOK {
			t.Errorf("%s: got %d %s", query, w.Code, w.Body.String())
		}
	}
}

func TestSubscriptionValueWindowUsesReportClock(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	for _, tc := range []struct {
		period string
		span   time.Duration
	}{{"", 30 * 24 * time.Hour}, {"1h", time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}, {"90d", 90 * 24 * time.Hour}} {
		r, err := http.NewRequest(http.MethodGet, "/api/stats/subscriptions?period="+tc.period, nil)
		if err != nil {
			t.Fatal(err)
		}
		from, to, err := subscriptionValueWindow(r, now)
		if err != nil || from != now.Add(-tc.span).Unix() || to != now.Unix()+1 {
			t.Errorf("period %q: from=%d to=%d err=%v; want range based on report clock %d", tc.period, from, to, err, now.Unix())
		}
	}
}

func TestSubscriptionValueTierUpdateAndAccounting(t *testing.T) {
	db, h := newTestServer(t)
	insertValueCredential(t, db, "cred_max", "max", 7)
	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodPost, "/api/credentials/cred_max/tier", `{"tier":" 20x "}`, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("set tier: %d %s", w.Code, w.Body.String())
	}
	credential, err := creds.Get(context.Background(), db, "cred_max")
	if err != nil {
		t.Fatal(err)
	}
	if credential.RateLimitTier != "20x" || credential.Weight != 7 || credential.Status != creds.StatusActive {
		t.Fatalf("tier update changed selection state: %+v", credential)
	}
	list := do(t, h, http.MethodGet, "/api/credentials", "", cookie)
	if !strings.Contains(list.Body.String(), `"rate_limit_tier":"20x"`) {
		t.Fatalf("credential DTO lacks tier: %s", list.Body.String())
	}
	at := time.Now().Add(-time.Minute).Unix()
	_, err = db.Exec(`INSERT INTO request_log
		(credential_id,ts,path,status_code,model,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens)
		VALUES ('cred_max',?,'/v1/messages',200,'claude-test',100,20,30,400)`, at)
	if err != nil {
		t.Fatal(err)
	}
	w = do(t, h, http.MethodGet, "/api/stats/subscriptions?period=7d", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("statistics: %d %s", w.Code, w.Body.String())
	}
	var report subscriptionstats.Report
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Requests != 1 || report.Tokens.Total != 550 || len(report.Groups) != 1 || len(report.Accounts) != 1 {
		t.Fatalf("unexpected recorded totals: %+v", report)
	}
	if report.Groups[0].Tier != "20x" || report.Groups[0].Estimate != nil || report.Accounts[0].Attribution != "credential" {
		t.Fatalf("unexpected group/evidence: %+v", report.Groups[0])
	}
	for _, secret := range []string{"private-test-access", "private-test-refresh"} {
		if strings.Contains(w.Body.String(), secret) || strings.Contains(list.Body.String(), secret) {
			t.Fatal("credential secret in statistics response")
		}
	}
	if w := do(t, h, http.MethodPost, "/api/credentials/cred_max/tier", `{"tier":""}`, cookie); w.Code != http.StatusOK {
		t.Fatalf("clear tier: %d %s", w.Code, w.Body.String())
	}
	credential, err = creds.Get(context.Background(), db, "cred_max")
	if err != nil || credential.RateLimitTier != "" || credential.Weight != 7 {
		t.Fatalf("clear tier failed or changed weight: %v", err)
	}
}

func TestSubscriptionValueTierValidation(t *testing.T) {
	db, h := newTestServer(t)
	insertValueCredential(t, db, "cred_team", "team", 5)
	cookie := loginCookie(t, h)
	for _, body := range []string{"", "{", `{}`, `{"tier":null}`, `{"tier":5}`, `{"tier":"bad\u0000tier"}`, `{"tier":"` + strings.Repeat("x", 129) + `"}`} {
		w := do(t, h, http.MethodPost, "/api/credentials/cred_team/tier", body, cookie)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: got %d %s", body, w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		id   string
		code int
	}{{"missing", http.StatusNotFound}, {"gateway_codex", http.StatusConflict}, {"gateway_gemini", http.StatusConflict}} {
		w := do(t, h, http.MethodPost, "/api/credentials/"+tc.id+"/tier", `{"tier":"5x"}`, cookie)
		if w.Code != tc.code {
			t.Errorf("%s: got %d want %d", tc.id, w.Code, tc.code)
		}
	}
	if w := do(t, h, http.MethodGet, "/api/credentials/cred_team/tier", "", cookie); w.Code != http.StatusNotFound {
		t.Fatalf("tier mutation accepted GET: %d", w.Code)
	}
}

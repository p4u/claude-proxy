package codexgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Trimmed from a live fetchAvailableModels response (2026-10-01, free tier):
// every gemini-* model shares one bucket; Antigravity's Claude and internal
// tab models report their own, which must be ignored.
const liveModels = `{"models":{
 "gemini-pro-agent":      {"quotaInfo":{"remainingFraction":0.9991104,"resetTime":"2026-10-01T14:32:43Z"}},
 "gemini-3.8-flash-high": {"quotaInfo":{"remainingFraction":0.9991104,"resetTime":"2026-10-01T14:32:43Z"}},
 "claude-sonnet-4-6":     {"quotaInfo":{"remainingFraction":0.1,"resetTime":"2026-10-01T15:01:31Z"}},
 "tab_flash_lite_preview":{"quotaInfo":{"remainingFraction":1}}
}}`

func TestParseGeminiQuota(t *testing.T) {
	q, err := parseGeminiQuota([]byte(liveModels))
	if err != nil {
		t.Fatal(err)
	}
	reset, _ := time.Parse(time.RFC3339, "2026-10-01T14:32:43Z")
	if !q.HasSignals || !q.HasFiveHour || q.HasSevenDay {
		t.Fatalf("window flags = %+v", q)
	}
	if q.FiveHourPct != 0.1 || q.FiveHourResets != reset.Unix() {
		t.Fatalf("quota = %v%% reset %d, want 0.1%% reset %d (the claude-* bucket must be ignored)", q.FiveHourPct, q.FiveHourResets, reset.Unix())
	}
}

// Protobuf JSON omits zero values, so a spent bucket has no remainingFraction.
func TestParseGeminiQuotaExhaustedBucket(t *testing.T) {
	q, err := parseGeminiQuota([]byte(`{"models":{
	  "gemini-pro-agent":{"quotaInfo":{"resetTime":"2026-10-01T14:32:43Z"}},
	  "gemini-3-flash":{"quotaInfo":{"remainingFraction":0.4,"resetTime":"2026-10-01T14:32:43Z"}}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	if q.FiveHourPct != 100 {
		t.Fatalf("exhausted bucket read as %v%% used, want 100%%", q.FiveHourPct)
	}

	if _, err := parseGeminiQuota([]byte(`{"models":{"claude-sonnet-4-6":{"quotaInfo":{"remainingFraction":1}}}}`)); err == nil {
		t.Fatal("a response with no Gemini quota must be an error, not a 0% reading")
	}
}

func TestParseGeminiTier(t *testing.T) {
	for body, want := range map[string]string{
		`{"currentTier":{"id":"free-tier","name":"Antigravity"}}`: "free",
		`{"currentTier":{"id":"g1-pro-tier"}}`:                    "g1-pro",
		`{}`:                                                      "",
		`not json`:                                                "",
	} {
		if got := parseGeminiTier([]byte(body)); got != want {
			t.Errorf("parseGeminiTier(%s) = %q, want %q", body, got, want)
		}
	}
}

// End to end against a fake sidecar: the Gemini channel lists only
// antigravity accounts, reads quota and tier through api-call with an
// Antigravity User-Agent and the $TOKEN$ placeholder, and caches the result.
func TestGeminiAccountsReadQuotaThroughSidecar(t *testing.T) {
	var apiCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": "codex-a.json", "type": "codex", "auth_index": "c1"},
				{"name": "antigravity-g@example.com.json", "type": "antigravity", "provider": "antigravity",
					"auth_index": "g1", "email": "g@example.com", "project_id": "p-1"},
			}})
		case "/v0/management/api-call":
			apiCalls.Add(1)
			var req struct {
				AuthIndex string            `json:"auth_index"`
				URL       string            `json:"url"`
				Header    map[string]string `json:"header"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.AuthIndex != "g1" || req.Header["Authorization"] != "Bearer $TOKEN$" ||
				!strings.HasPrefix(req.Header["User-Agent"], "antigravity/") {
				http.Error(w, "bad api-call", http.StatusBadRequest)
				return
			}
			body := liveModels
			if strings.HasSuffix(req.URL, ":loadCodeAssist") {
				body = `{"currentTier":{"id":"free-tier"}}`
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": body})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, APIKey: "api", ManagementKey: "management"})
	if err != nil {
		t.Fatal(err)
	}

	accounts, err := c.Accounts(context.Background(), GeminiChannel)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Email != "g@example.com" {
		t.Fatalf("gemini accounts = %#v", accounts)
	}
	q := accounts[0].Quota
	if !q.HasSignals || q.FiveHourPct != 0.1 || q.PlanType != "free" {
		t.Fatalf("quota = %+v", q)
	}
	if n := apiCalls.Load(); n != 2 {
		t.Fatalf("api-call count = %d, want 2 (quota + tier)", n)
	}
	if _, err := c.Accounts(context.Background(), GeminiChannel); err != nil {
		t.Fatal(err)
	}
	if n := apiCalls.Load(); n != 2 {
		t.Fatalf("second listing made %d more api-calls; quota must be cached", n-2)
	}

	codex, err := c.Accounts(context.Background(), CodexChannel)
	if err != nil || len(codex) != 1 || codex[0].Name != "codex-a.json" {
		t.Fatalf("codex accounts = %#v, %v", codex, err)
	}
}

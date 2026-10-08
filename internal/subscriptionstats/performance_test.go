package subscriptionstats

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

func TestOpenAINormalizationIsPerRequest(t *testing.T) {
	f := newFixture(t)
	f.credential("a", "custom_openai", "", "", 1)
	f.request("a", "same-model", testNow.Add(-time.Minute), Tokens{Input: 5, CacheRead: 10})
	f.request("a", "same-model", testNow.Add(-time.Second), Tokens{Input: 100, CacheRead: 20})
	r := f.build(testNow.Add(-time.Hour), testNow, "seven_day")
	want := Tokens{Input: 80, CacheRead: 30, Total: 110}
	if r.Tokens != want || r.Accounts[0].Tokens != want || r.Groups[0].Tokens != want || r.Daily[0].Tokens != want || r.Models[0].Tokens != want {
		t.Fatalf("normalization was applied after aggregating requests: report=%+v", r.Tokens)
	}
}

func TestModelDetailLimitKeepsCompleteTotals(t *testing.T) {
	if testing.Short() {
		t.Skip("bounded 100k-model fixture")
	}
	f := newFixture(t)
	f.credential("a", "anthropic", "max", "20x", 1)
	f.exec(`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<?)
		INSERT INTO request_log(credential_id,ts,model,path,status_code,output_tokens)
		SELECT 'a',?,printf('model-%06d',x),'/v1/messages',200,1 FROM n`, maxRows+6, testNow.Add(-time.Minute).Unix())
	// A retained detail bucket must still count later traffic after the detail
	// cap is reached. Neither the daily nor account totals depend on that cap.
	f.request("a", "model-000000", testNow.Add(-time.Second), Tokens{Output: 100})
	r := f.build(testNow.Add(-time.Hour), testNow, "seven_day")
	wantRequests, wantTokens := int64(maxRows+8), int64(maxRows+107)
	if !r.Truncated || r.Requests != wantRequests || r.Tokens.Output != wantTokens || r.Tokens.Total != wantTokens {
		t.Fatalf("detail limit changed complete totals: requests=%d tokens=%+v truncated=%v", r.Requests, r.Tokens, r.Truncated)
	}
	if len(r.Models) != maxRows || len(r.Daily) != 1 || len(r.Groups) != 1 || len(r.Accounts) != 1 {
		t.Fatalf("detail sizes: models=%d daily=%d groups=%d accounts=%d", len(r.Models), len(r.Daily), len(r.Groups), len(r.Accounts))
	}
	if r.Daily[0].Tokens != r.Tokens || r.Groups[0].Tokens != r.Tokens || r.Accounts[0].Tokens != r.Tokens {
		t.Fatal("model truncation leaked into unrelated breakdowns")
	}
	if r.Models[0].Model != "model-000000" || r.Models[0].Tokens.Output != 101 || r.Models[0].Requests != 2 {
		t.Fatalf("retained detail lost later traffic: %+v", r.Models[0])
	}
	if !strings.Contains(strings.Join(r.Notes, " "), "limit was reached") {
		t.Fatal("detail truncation was not disclosed")
	}
}

func TestCombinedDimensionsDoNotTruncateIndependentViews(t *testing.T) {
	if testing.Short() {
		t.Skip("bounded 105k-cell fixture")
	}
	f := newFixture(t)
	f.credential("a", "anthropic", "max", "20x", 1)
	// Each view is small (one account, 21 days, 5,000 models), although the
	// Cartesian product exceeds the detail cap. A shared scan must not cap that
	// product and quietly undercount otherwise complete individual views.
	f.exec(`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<104999)
		INSERT INTO request_log(credential_id,ts,model,path,status_code,output_tokens)
		SELECT 'a',?-86400*(x/5000),printf('model-%04d',x%5000),'/v1/messages',200,1 FROM n`, testNow.Add(-time.Minute).Unix())
	r := f.build(testNow.Add(-30*24*time.Hour), testNow, "seven_day")
	if r.Truncated || r.Requests != 105_000 || r.Tokens.Total != 105_000 || len(r.Daily) != 21 || len(r.Models) != 5_000 {
		t.Fatalf("combined dimensions lost detail: requests=%d tokens=%d days=%d models=%d truncated=%v", r.Requests, r.Tokens.Total, len(r.Daily), len(r.Models), r.Truncated)
	}
	if r.Accounts[0].Tokens != r.Tokens || r.Groups[0].Tokens != r.Tokens {
		t.Fatal("account/group rollups do not reconcile")
	}
	for _, day := range r.Daily {
		if day.Requests != 5_000 || day.Tokens.Output != 5_000 {
			t.Fatalf("incomplete day: %+v", day)
		}
	}
	for _, model := range r.Models {
		if model.Requests != 21 || model.Tokens.Output != 21 {
			t.Fatalf("incomplete model: %+v", model)
		}
	}
}

func TestAccountDetailLimitKeepsOtherViewsComplete(t *testing.T) {
	if testing.Short() {
		t.Skip("bounded 100k-account fixture")
	}
	f := newFixture(t)
	f.exec(`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<?)
		INSERT INTO request_log(credential_id,ts,model,path,status_code,output_tokens)
		SELECT printf('deleted-%06d',x),?,'m','/v1/messages',200,1 FROM n`, maxRows+6, testNow.Add(-time.Minute).Unix())
	f.request("deleted-000000", "m", testNow.Add(-time.Second), Tokens{Output: 100})
	r := f.build(testNow.Add(-time.Hour), testNow, "seven_day")
	if !r.Truncated || len(r.Accounts) != maxRows || r.Requests != maxRows+8 || r.Tokens.Output != maxRows+107 {
		t.Fatalf("account cap lost totals: accounts=%d requests=%d tokens=%+v", len(r.Accounts), r.Requests, r.Tokens)
	}
	if r.Groups[0].Tokens != r.Tokens || r.Daily[0].Tokens != r.Tokens || r.Models[0].Tokens != r.Tokens {
		t.Fatal("account truncation leaked into other views")
	}
	if accountByID(t, r, "deleted-000000").Tokens.Output != 101 {
		t.Fatal("retained account lost later traffic")
	}
}

func TestCredentialMetadataLimitPreservesNormalization(t *testing.T) {
	if testing.Short() {
		t.Skip("bounded 100k-credential fixture")
	}
	f := newFixture(t)
	f.exec(`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<?)
		INSERT INTO credentials(id,provider,subscription_type,access_token,refresh_token,expires_at,status,weight,created_at)
		SELECT printf('a-%06d',x),'glm','pro','synthetic','',?,'disabled',1,? FROM n`, maxRows-1, testNow.Unix()+86400, testNow.Unix()-86400)
	f.credential("z-omitted", "custom_openai", "", "", 1)
	f.request("a-000000", "m", testNow.Add(-time.Minute), Tokens{Input: 100, CacheRead: 20})
	f.request("z-omitted", "m", testNow.Add(-time.Minute), Tokens{Input: 5, CacheRead: 10})
	r := f.build(testNow.Add(-time.Hour), testNow, "seven_day")
	if !r.Truncated || len(r.Accounts) != maxRows || r.Requests != 2 || r.Tokens != (Tokens{Input: 100, CacheRead: 30, Total: 130}) {
		t.Fatalf("capped metadata corrupted normalized totals: accounts=%d requests=%d tokens=%+v", len(r.Accounts), r.Requests, r.Tokens)
	}
	for _, a := range r.Accounts {
		if a.ID == "z-omitted" || a.Attribution == "unknown" {
			t.Fatal("omitted existing metadata was misreported as a deleted credential")
		}
	}
}

func TestDailyDetailLimitKeepsOtherViewsComplete(t *testing.T) {
	if testing.Short() {
		t.Skip("bounded 100k-day/cohort fixture")
	}
	f := newFixture(t)
	f.exec(`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<1111)
		INSERT INTO credentials(id,provider,subscription_type,rate_limit_tier,access_token,refresh_token,expires_at,status,weight,created_at)
		SELECT printf('a-%04d',x),'glm','pro',printf('tier-%04d',x),'synthetic','',?,'disabled',1,? FROM n`, testNow.Unix()+86400, testNow.Unix()-90*86400)
	f.exec(`WITH RECURSIVE days(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM days WHERE x<89)
		INSERT INTO request_log(credential_id,ts,model,path,status_code,output_tokens)
		SELECT c.id,?-x*86400,'m','/v1/messages',200,1 FROM credentials c CROSS JOIN days`, testNow.Add(-time.Minute).Unix())
	r := f.build(testNow.Add(-90*24*time.Hour), testNow, "seven_day")
	if !r.Truncated || len(r.Daily) != maxRows || r.Requests != 100_080 || r.Tokens.Output != 100_080 {
		t.Fatalf("daily cap lost totals: days=%d requests=%d tokens=%+v", len(r.Daily), r.Requests, r.Tokens)
	}
	for _, a := range r.Accounts {
		if a.Requests != 90 || a.Tokens.Output != 90 {
			t.Fatal("daily cap lost account traffic")
		}
	}
	for _, m := range r.Models {
		if m.Requests != 90 || m.Tokens.Output != 90 {
			t.Fatal("daily cap lost model traffic")
		}
	}
}

// Run explicitly with -run '^$' -bench BenchmarkBuildProductionVolume -benchtime=1x.
// Keep timing assertions out of CI: measure the full million-row request log and
// quota calibration, not just an indexed COUNT or a small in-memory fixture.
func BenchmarkBuildProductionVolume(b *testing.B) {
	db, err := store.Open(filepath.Join(b.TempDir(), "stats.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	exec := func(query string, args ...any) {
		b.Helper()
		if _, err := db.ExecContext(b.Context(), query, args...); err != nil {
			b.Fatal(err)
		}
	}
	for i := range 13 {
		plan, tier := "team", "5x"
		if i < 4 {
			plan, tier = "max", "20x"
		}
		exec(`INSERT INTO credentials(id,label,provider,subscription_type,rate_limit_tier,access_token,refresh_token,expires_at,status,weight,created_at)
			VALUES(?,?,'anthropic',?,?,'synthetic','synthetic',?,'disabled',1,?)`,
			fmt.Sprintf("cred_%d", i), fmt.Sprintf("Synthetic %d", i), plan, tier, testNow.Add(24*time.Hour).Unix(), testNow.Add(-200*24*time.Hour).Unix())
	}
	exec(`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<999999)
		INSERT INTO request_log(credential_id,conv_id,ts,path,status_code,model,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens)
		SELECT 'cred_'||(x%13),printf('synthetic-session-%064d',x/10),?-(CASE
			WHEN x<130000 THEN 1+x*604799/130000
			WHEN x<346000 THEN 604800+(x-130000)*1987200/216000
			WHEN x<632000 THEN 2592000+(x-346000)*5184000/286000
			ELSE 7776000+(x-632000)*9504000/368000 END),
			'/v1/messages',200,'claude-synthetic-'||(x%7),1000,100,50,9000 FROM n`, testNow.Unix())
	exec(`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<37101),
		observations AS (SELECT x,?-300-(x/13)*600 AS ts FROM n),
		resets AS (SELECT x,ts,?+3600-((?+3600-ts-1)/18000)*18000 AS fh,
			?+86400-((?+86400-ts-1)/604800)*604800 AS sd FROM observations)
		INSERT INTO usage_history(credential_id,captured_at,five_hour_pct,five_hour_resets_at,five_hour_observed,seven_day_pct,seven_day_resets_at,seven_day_observed)
		SELECT 'cred_'||(x%13),ts,(ts-(fh-18000))*100.0/18000,fh,1,
			(ts-(sd-604800))*100.0/604800,sd,(x%13)<4 FROM resets`,
		testNow.Unix(), testNow.Unix(), testNow.Unix(), testNow.Unix(), testNow.Unix())
	for _, tc := range []struct {
		days   int
		window string
	}{{7, "seven_day"}, {30, "seven_day"}, {90, "seven_day"}, {90, "five_hour"}} {
		b.Run(fmt.Sprintf("%dd/%s", tc.days, tc.window), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ctx, cancel := context.WithTimeout(b.Context(), time.Minute)
				r, err := Build(ctx, db.DB, testNow.Add(-time.Duration(tc.days)*24*time.Hour), testNow, testNow, tc.window)
				cancel()
				if err != nil {
					b.Fatal(err)
				}
				if r.Truncated || r.Requests < 130_000 || r.Tokens.Total != r.Requests*10_150 || len(r.CapacityHistory) == 0 {
					b.Fatalf("incomplete report: requests=%d tokens=%d samples=%d truncated=%v", r.Requests, r.Tokens.Total, len(r.CapacityHistory), r.Truncated)
				}
			}
		})
	}
}

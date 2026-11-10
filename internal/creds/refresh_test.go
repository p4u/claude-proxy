package creds

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

// fakeAnthropic serves the token and profile endpoints. Configure it before
// the first request; counters are safe to read at any time.
type fakeAnthropic struct {
	tokenCalls    atomic.Int32
	profileCalls  atomic.Int32
	tokenStatus   int    // 0 answers 200 with fresh tokens
	tokenBody     string // replaces the 200 body when set
	deadlineIn    int    // refresh_token_expires_in in seconds; 0 omits it
	profileStatus int    // 0 answers 200
	onToken       func() // runs before the token endpoint answers
}

func (f *fakeAnthropic) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			f.tokenCalls.Add(1)
			if f.onToken != nil {
				f.onToken()
			}
			if f.tokenStatus != 0 {
				w.WriteHeader(f.tokenStatus)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token expired"}`))
				return
			}
			body := f.tokenBody
			if body == "" {
				extra := ""
				if f.deadlineIn > 0 {
					extra = fmt.Sprintf(`,"refresh_token_expires_in":%d`, f.deadlineIn)
				}
				body = `{"access_token":"sk-ant-oat-fresh","refresh_token":"ref-fresh","expires_in":28800` + extra + `}`
			}
			_, _ = w.Write([]byte(body))
		case "/profile":
			f.profileCalls.Add(1)
			if f.profileStatus != 0 {
				w.WriteHeader(f.profileStatus)
				return
			}
			_, _ = w.Write([]byte(`{"account":{"email":"a@example.com"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	prevToken, prevProfile := TokenURL, ProfileURL
	TokenURL, ProfileURL = srv.URL+"/token", srv.URL+"/profile"
	t.Cleanup(func() { TokenURL, ProfileURL = prevToken, prevProfile; srv.Close() })
}

// logBuf captures a refresher's log lines.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// count returns how many lines are at level and carry label=label.
func (l *logBuf) count(level, label string) int {
	n := 0
	for line := range strings.SplitSeq(l.String(), "\n") {
		fields := strings.Fields(line)
		if hasField(fields, "level="+level) && hasField(fields, "label="+label) {
			n++
		}
	}
	return n
}

func hasField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

func captureLogs(r *Refresher) *logBuf {
	lb := &logBuf{}
	r.SetLogger(slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return lb
}

// insertAt stores an Anthropic credential labelled label with the given
// access-token expiry and refresh deadline (zero = unknown).
func insertAt(t *testing.T, db *store.DB, label string, expires, deadline time.Time) *Credential {
	t.Helper()
	c, err := InsertWithRateLimitTier(context.Background(), db, label, "max", "sk-ant-oat-old", "ref-old", expires, 5, "", deadline)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func near() time.Time { return time.Now().Add(2 * time.Minute) }

func TestRefreshTokens(t *testing.T) {
	ctx := context.Background()
	f := &fakeAnthropic{deadlineIn: 3600}
	f.serve(t)
	ts, err := RefreshTokens(ctx, "old-ref", time.Now().Add(48*time.Hour))
	if err != nil {
		t.Fatalf("refresh-tokens: %v", err)
	}
	if ts.AccessToken != "sk-ant-oat-fresh" || ts.RefreshToken != "ref-fresh" || time.Until(ts.ExpiresAt) < 7*time.Hour {
		t.Fatalf("unexpected tokens: %+v", ts)
	}
	if d := time.Until(ts.RefreshExpiresAt); d < 59*time.Minute || d > time.Hour {
		t.Fatalf("deadline in %v, want the earlier returned one (1h)", d)
	}
	if ts, err = RefreshTokens(ctx, "old-ref", time.Time{}); err != nil || !ts.RefreshExpiresAt.IsZero() {
		t.Fatalf("unknown deadline must stay unknown: %v, %v", ts.RefreshExpiresAt, err)
	}

	rejected := &fakeAnthropic{tokenStatus: http.StatusBadRequest}
	rejected.serve(t)
	if _, err := RefreshTokens(ctx, "bad", time.Time{}); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("400 = %v, want a rejection quoting the OAuth error", err)
	}
}

// AC4: the stored deadline follows Claude Code's min rule.
func TestRefreshDeadlineRule(t *testing.T) {
	day := 24 * time.Hour
	for _, tt := range []struct {
		name       string
		stored     time.Duration // from now; 0 = unknown
		deadlineIn int           // seconds returned; 0 = none
		want       time.Duration // from now; 0 = unknown
	}{
		{"known keeps the earlier stored value", 10 * day, int((20 * day).Seconds()), 10 * day},
		{"known takes an earlier returned value", 20 * day, int((10 * day).Seconds()), 10 * day},
		{"known without a returned value", 10 * day, 0, 10 * day},
		{"unknown stays unknown", 0, int((10 * day).Seconds()), 0},
		{"passed is replaced", -day, int((10 * day).Seconds()), 10 * day},
		{"passed without a returned value becomes unknown", -day, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db := testDB(t)
			(&fakeAnthropic{deadlineIn: tt.deadlineIn}).serve(t)
			var stored time.Time
			if tt.stored != 0 {
				stored = time.Now().Add(tt.stored)
			}
			c := insertAt(t, db, "a", near(), stored)
			got, refreshed, err := NewRefresher(db).Refresh(ctx, c.ID)
			if err != nil || !refreshed {
				t.Fatalf("refresh = %v, refreshed=%v", err, refreshed)
			}
			if tt.want == 0 {
				if !got.RefreshExpiresAt.IsZero() {
					t.Fatalf("deadline = %v, want unknown", got.RefreshExpiresAt)
				}
				return
			}
			if d := time.Until(got.RefreshExpiresAt) - tt.want; d < -time.Minute || d > time.Minute {
				t.Fatalf("deadline in %v, want about %v", time.Until(got.RefreshExpiresAt), tt.want)
			}
		})
	}
}

// AC5: the non-forced Refresh leaves a working token alone.
func TestRefreshSkipsLiveToken(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		status  Status
		expires time.Time
		call    bool
	}{
		{StatusActive, time.Now().Add(time.Hour), false},
		{StatusLimited, time.Now().Add(time.Hour), false},
		{StatusDisabled, time.Now().Add(time.Hour), false},
		{StatusRevoked, time.Now().Add(time.Hour), true},
		{StatusActive, near(), true},
	} {
		t.Run(fmt.Sprintf("%s/call=%v", tt.status, tt.call), func(t *testing.T) {
			db := testDB(t)
			f := &fakeAnthropic{}
			f.serve(t)
			c := insertAt(t, db, "a", tt.expires, time.Time{})
			if err := SetStatus(ctx, db, c.ID, tt.status); err != nil {
				t.Fatal(err)
			}
			_, refreshed, err := NewRefresher(db).Refresh(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if calls := f.tokenCalls.Load(); (calls == 1) != tt.call || refreshed != tt.call {
				t.Fatalf("token calls = %d, refreshed = %v; want call=%v", calls, refreshed, tt.call)
			}
		})
	}
	t.Run("not refreshable", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{}
		f.serve(t)
		key, err := InsertKey(ctx, db, provider.GLM, "glm", "", "glm-key", "", 1)
		if err != nil {
			t.Fatal(err)
		}
		_ = SetStatus(ctx, db, key.ID, StatusRevoked)
		r := NewRefresher(db)
		if _, _, err := r.Refresh(ctx, key.ID); !errors.Is(err, ErrNotRefreshable) {
			t.Fatalf("Refresh = %v, want ErrNotRefreshable", err)
		}
		if _, _, err := r.RefreshRejected(ctx, key.ID, "glm-key"); !errors.Is(err, ErrNotRefreshable) {
			t.Fatalf("RefreshRejected = %v, want ErrNotRefreshable", err)
		}
		if f.tokenCalls.Load()+f.profileCalls.Load() != 0 {
			t.Fatal("a non-refreshable credential reached Anthropic")
		}
	})
}

// AC5b: the background loop never touches a long-lived token.
func TestTickLeavesLongLivedTokenAlone(t *testing.T) {
	db := testDB(t)
	f := &fakeAnthropic{}
	f.serve(t)
	insertAt(t, db, "a", time.Now().AddDate(1, 0, 0), time.Now().Add(27*24*time.Hour))
	NewRefresher(db).tick(context.Background())
	if n := f.tokenCalls.Load(); n != 0 {
		t.Fatalf("tick made %d token calls for a year-long token", n)
	}
}

// AC6 (refresher side): a 401 refreshes only a token the profile endpoint
// also rejects.
func TestRefreshRejected(t *testing.T) {
	ctx := context.Background()
	t.Run("alive token is not refreshed", func(t *testing.T) {
		for _, status := range []int{0, http.StatusForbidden, http.StatusServiceUnavailable} {
			db := testDB(t)
			f := &fakeAnthropic{profileStatus: status}
			f.serve(t)
			c := insertAt(t, db, "a", time.Now().AddDate(1, 0, 0), time.Time{})
			_, refreshed, err := NewRefresher(db).RefreshRejected(ctx, c.ID, c.AccessToken)
			if !errors.Is(err, ErrTokenAlive) || refreshed {
				t.Fatalf("profile %d: err = %v, refreshed = %v; want ErrTokenAlive", status, err, refreshed)
			}
			got, _ := Get(ctx, db, c.ID)
			if f.tokenCalls.Load() != 0 || got.AccessToken != c.AccessToken || got.Status != StatusActive || !got.ExpiresAt.Equal(c.ExpiresAt.Truncate(time.Second)) {
				t.Fatalf("profile %d: token calls %d, stored %+v", status, f.tokenCalls.Load(), got)
			}
		}
	})
	t.Run("confirmed dead token is refreshed", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{profileStatus: http.StatusUnauthorized}
		f.serve(t)
		c := insertAt(t, db, "a", time.Now().Add(time.Hour), time.Time{})
		got, refreshed, err := NewRefresher(db).RefreshRejected(ctx, c.ID, c.AccessToken)
		if err != nil || !refreshed || got.AccessToken != "sk-ant-oat-fresh" || f.tokenCalls.Load() != 1 {
			t.Fatalf("err=%v refreshed=%v got=%+v calls=%d", err, refreshed, got, f.tokenCalls.Load())
		}
	})
	t.Run("near expiry skips the profile check", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{}
		f.serve(t)
		c := insertAt(t, db, "a", near(), time.Time{})
		if _, refreshed, err := NewRefresher(db).RefreshRejected(ctx, c.ID, c.AccessToken); err != nil || !refreshed {
			t.Fatalf("err=%v refreshed=%v", err, refreshed)
		}
		if f.profileCalls.Load() != 0 || f.tokenCalls.Load() != 1 {
			t.Fatalf("profile calls %d, token calls %d", f.profileCalls.Load(), f.tokenCalls.Load())
		}
	})
	t.Run("token already replaced", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{}
		f.serve(t)
		c := insertAt(t, db, "a", time.Now().Add(time.Hour), time.Time{})
		got, refreshed, err := NewRefresher(db).RefreshRejected(ctx, c.ID, "sk-ant-oat-an-older-one")
		if err != nil || refreshed || got.AccessToken != c.AccessToken || f.tokenCalls.Load()+f.profileCalls.Load() != 0 {
			t.Fatalf("err=%v refreshed=%v calls=%d/%d", err, refreshed, f.tokenCalls.Load(), f.profileCalls.Load())
		}
	})
	t.Run("concurrent 401s refresh once", func(t *testing.T) {
		db := testDB(t)
		release := make(chan struct{})
		f := &fakeAnthropic{profileStatus: http.StatusUnauthorized, onToken: func() { <-release }}
		f.serve(t)
		c := insertAt(t, db, "a", time.Now().Add(time.Hour), time.Time{})
		r := NewRefresher(db)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for range 2 {
			wg.Go(func() {
				_, _, err := r.RefreshRejected(ctx, c.ID, c.AccessToken)
				errs <- err
			})
		}
		for f.tokenCalls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		close(release)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if n := f.tokenCalls.Load(); n != 1 {
			t.Fatalf("token calls = %d, want 1", n)
		}
	})
	t.Run("rejected login is revoked", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{profileStatus: http.StatusUnauthorized, tokenStatus: http.StatusBadRequest}
		f.serve(t)
		c := insertAt(t, db, "a", time.Now().Add(time.Hour), time.Time{})
		if _, _, err := NewRefresher(db).RefreshRejected(ctx, c.ID, c.AccessToken); !errors.Is(err, ErrLoginRejected) {
			t.Fatalf("err = %v, want ErrLoginRejected", err)
		}
		if got, _ := Get(ctx, db, c.ID); got.Status != StatusRevoked {
			t.Fatalf("status = %q, want revoked", got.Status)
		}
	})
}

// AC6 (refresher side): a transient failure leaves the status alone, sets a
// one-minute retry_after and is replayed for 30 s without a token call.
func TestTransientRefreshFailure(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name  string
		setup func(f *fakeAnthropic, r *Refresher)
	}{
		{"server error", func(f *fakeAnthropic, _ *Refresher) { f.tokenStatus = http.StatusInternalServerError }},
		{"timeout", func(f *fakeAnthropic, r *Refresher) {
			f.onToken = func() { time.Sleep(300 * time.Millisecond) }
			SetTokenClient(r, &http.Client{Timeout: 50 * time.Millisecond})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := testDB(t)
			r := NewRefresher(db)
			f := &fakeAnthropic{profileStatus: http.StatusUnauthorized}
			tt.setup(f, r)
			f.serve(t)
			c := insertAt(t, db, "a", time.Now().Add(time.Hour), time.Time{})
			_, _, err := r.RefreshRejected(ctx, c.ID, c.AccessToken)
			if err == nil || errors.Is(err, ErrLoginRejected) || errors.Is(err, ErrTokenAlive) {
				t.Fatalf("err = %v, want a transient error", err)
			}
			got, _ := Get(ctx, db, c.ID)
			if got.Status != StatusActive || got.RetryAfter == nil {
				t.Fatalf("status %q retry_after %v, want active with a retry_after", got.Status, got.RetryAfter)
			}
			if d := time.Until(*got.RetryAfter); d < 50*time.Second || d > 61*time.Second {
				t.Fatalf("retry_after in %v, want about 60s", d)
			}
			if _, _, err2 := r.RefreshRejected(ctx, c.ID, c.AccessToken); err2 == nil || f.tokenCalls.Load() != 1 {
				t.Fatalf("second 401: err=%v, token calls %d; want the remembered error and no new call", err2, f.tokenCalls.Load())
			}
		})
	}
}

// AC7: one WARN per failed refresh and one ERROR per revocation, with the
// label, and a 200 without a refresh token leaks nothing of its body.
func TestRefreshLogging(t *testing.T) {
	ctx := context.Background()
	t.Run("transient", func(t *testing.T) {
		db := testDB(t)
		(&fakeAnthropic{tokenStatus: http.StatusBadGateway}).serve(t)
		c := insertAt(t, db, "acct-warn", near(), time.Time{})
		r := NewRefresher(db)
		logs := captureLogs(r)
		_, _, _ = r.Refresh(ctx, c.ID)
		if logs.count("WARN", "acct-warn") != 1 || logs.count("ERROR", "acct-warn") != 0 {
			t.Fatalf("logs:\n%s", logs)
		}
	})
	t.Run("revocation", func(t *testing.T) {
		db := testDB(t)
		(&fakeAnthropic{tokenStatus: http.StatusBadRequest}).serve(t)
		c := insertAt(t, db, "acct-err", near(), time.Now().Add(-time.Hour))
		r := NewRefresher(db)
		logs := captureLogs(r)
		_, _, _ = r.Refresh(ctx, c.ID)
		if logs.count("ERROR", "acct-err") != 1 || logs.count("WARN", "acct-err") != 0 || !strings.Contains(logs.String(), "deadline_passed=true") {
			t.Fatalf("logs:\n%s", logs)
		}
	})
	t.Run("missing refresh token", func(t *testing.T) {
		db := testDB(t)
		body := `{"access_token":"sk-ant-oat-leaked-secret","expires_in":28800,"scope":"user:inference"}`
		(&fakeAnthropic{tokenBody: body}).serve(t)
		c := insertAt(t, db, "acct-leak", near(), time.Time{})
		r := NewRefresher(db)
		logs := captureLogs(r)
		_, _, err := r.Refresh(ctx, c.ID)
		if err == nil {
			t.Fatal("a 200 without a refresh token must fail")
		}
		for _, part := range []string{"sk-ant-oat-leaked-secret", "access_token", "expires_in", "28800", "user:inference"} {
			if strings.Contains(err.Error(), part) || strings.Contains(logs.String(), part) {
				t.Fatalf("%q leaked: err=%q logs=%q", part, err, logs)
			}
		}
	})
	t.Run("long-lived token rejected", func(t *testing.T) {
		db := testDB(t)
		(&fakeAnthropic{profileStatus: http.StatusUnauthorized}).serve(t)
		c := insertAt(t, db, "acct-long", time.Now().AddDate(1, 0, 0), time.Now().Add(20*24*time.Hour))
		r := NewRefresher(db)
		logs := captureLogs(r)
		if _, _, err := r.RefreshRejected(ctx, c.ID, c.AccessToken); err != nil {
			t.Fatal(err)
		}
		if logs.count("ERROR", "acct-long") != 1 || !strings.Contains(logs.String(), "long-lived token rejected before its expiry") {
			t.Fatalf("logs:\n%s", logs)
		}
	})
}

// AC8: a refresh Anthropic has answered is stored even when the caller left.
func TestRefreshSurvivesCallerCancel(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeAnthropic{onToken: cancel}
	f.serve(t)
	c := insertAt(t, db, "a", near(), time.Time{})
	if _, _, err := NewRefresher(db).Refresh(ctx, c.ID); err != nil {
		t.Fatalf("refresh after cancel: %v", err)
	}
	if got, _ := Get(context.Background(), db, c.ID); got.AccessToken != "sk-ant-oat-fresh" {
		t.Fatalf("tokens lost after the caller cancelled: %q", got.AccessToken)
	}
}

// AC8b: a briefly busy database does not lose a completed refresh. The
// refresher's only connection waits 100 ms per attempt; the lock is held for
// 250 ms, so the first two write attempts fail and a later one succeeds.
func TestRefreshSurvivesBusyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout = 100`); err != nil {
		t.Fatal(err)
	}
	locker, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	c := insertAt(t, db, "a", near(), time.Time{})

	var tx *sql.Tx
	f := &fakeAnthropic{onToken: func() {
		var lerr error
		if tx, lerr = locker.Begin(); lerr != nil {
			t.Error(lerr)
			return
		}
		time.AfterFunc(250*time.Millisecond, func() { _ = tx.Rollback() })
	}}
	f.serve(t)
	if _, _, err := NewRefresher(db).Refresh(context.Background(), c.ID); err != nil {
		t.Fatalf("refresh with a busy database: %v", err)
	}
	if got, _ := Get(context.Background(), db, c.ID); got.AccessToken != "sk-ant-oat-fresh" {
		t.Fatalf("tokens not stored: %q", got.AccessToken)
	}
}

// A refresh that started before the tokens were replaced (Update tokens or a
// browser reconnect) must not revoke or overwrite the newer login when its
// stale response finally arrives.
func TestStaleRefreshDoesNotUndoReplacedTokens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"rejected", http.StatusBadRequest},
		{"succeeded", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := testDB(t)
			entered, release := make(chan struct{}), make(chan struct{})
			f := &fakeAnthropic{tokenStatus: tc.status, onToken: func() { close(entered); <-release }}
			f.serve(t)
			c := insertAt(t, db, "a", near(), time.Time{})
			r := NewRefresher(db)
			done := make(chan error, 1)
			go func() { _, _, err := r.Refresh(ctx, c.ID); done <- err }()

			<-entered
			if err := UpdateTokensWithSubscription(ctx, db, c.ID, "sk-ant-oat-relogin", "ref-relogin", time.Now().Add(8*time.Hour), "", "", time.Time{}); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("stale refresh reported %v; the credential is healthy", err)
			}
			got, _ := Get(ctx, db, c.ID)
			if got.RefreshToken != "ref-relogin" || got.AccessToken != "sk-ant-oat-relogin" || got.Status != StatusActive {
				t.Fatalf("newer login undone: %+v", got)
			}
		})
	}
}

// A stored refresh heals revoked but leaves disabled and limited alone.
func TestRefreshKeepsOperatorStatus(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct{ from, want Status }{
		{StatusRevoked, StatusActive},
		{StatusDisabled, StatusDisabled},
		{StatusLimited, StatusLimited},
	} {
		db := testDB(t)
		(&fakeAnthropic{}).serve(t)
		c := insertAt(t, db, "a", near(), time.Time{})
		_ = SetStatus(ctx, db, c.ID, tt.from)
		got, refreshed, err := NewRefresher(db).Refresh(ctx, c.ID)
		if err != nil || !refreshed || got.Status != tt.want {
			t.Fatalf("%s: err=%v refreshed=%v status=%q, want %q", tt.from, err, refreshed, got.Status, tt.want)
		}
	}
}

// R8: a 2xx other than 200 carries tokens too, so neither the error nor the
// log quotes its body, and the refresh counts as a transient failure.
func TestUnexpectedSuccessStatusQuotesNothing(t *testing.T) {
	ctx := context.Background()
	const access, refresh = "sk-ant-oat-leaked-secret", "ref-leaked-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"access_token":"` + access + `","refresh_token":"` + refresh + `","expires_in":28800}`))
	}))
	prev := TokenURL
	TokenURL = srv.URL
	t.Cleanup(func() { TokenURL = prev; srv.Close() })

	_, err := RefreshTokens(ctx, "old-ref", time.Time{})
	if err == nil || !strings.Contains(err.Error(), "201") {
		t.Fatalf("RefreshTokens on 201 = %v, want an error naming the status", err)
	}
	db := testDB(t)
	c := insertAt(t, db, "acct-201", near(), time.Time{})
	r := NewRefresher(db)
	logs := captureLogs(r)
	_, _, rerr := r.Refresh(ctx, c.ID)
	if rerr == nil {
		t.Fatal("Refresh on 201 must fail")
	}
	for _, part := range []string{access, refresh, "access_token", "28800"} {
		if strings.Contains(err.Error(), part) || strings.Contains(rerr.Error(), part) || strings.Contains(logs.String(), part) {
			t.Fatalf("%q leaked: RefreshTokens=%q Refresh=%q logs=%q", part, err, rerr, logs)
		}
	}
	if got, _ := Get(ctx, db, c.ID); got.AccessToken != c.AccessToken || got.Status != StatusActive || got.RetryAfter == nil {
		t.Fatalf("201 must be a transient failure: %+v", got)
	}
}

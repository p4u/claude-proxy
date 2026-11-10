package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// refreshServer is a web UI server whose refresher talks to a fake token
// endpoint answering status (0 = 200 with fresh tokens).
func refreshServer(t *testing.T, status int) (*Server, *store.DB, *atomic.Int32, *syncBuf) {
	t.Helper()
	calls := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-fresh","refresh_token":"ref-fresh","expires_in":28800}`))
	}))
	t.Cleanup(srv.Close)
	prev := creds.TokenURL
	creds.SetTokenURL(srv.URL)
	t.Cleanup(func() { creds.SetTokenURL(prev) })
	db, err := store.Open(filepath.Join(t.TempDir(), "webui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	logs := &syncBuf{}
	r := creds.NewRefresher(db)
	r.SetLogger(slog.New(slog.NewTextHandler(logs, nil)))
	return &Server{db: db, refresher: r}, db, calls, logs
}

func postRefresh(s *Server, id string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	path := "/" + id + "/refresh"
	s.handleCredentials(rec, httptest.NewRequest(http.MethodPost, "/api/credentials"+path, nil), path)
	return rec
}

// AC5: the manual refresh leaves a working token alone and says so.
func TestRefreshEndpointReportsRefreshed(t *testing.T) {
	ctx := context.Background()
	s, db, calls, _ := refreshServer(t, 0)
	for _, tt := range []struct {
		expires   time.Time
		refreshed bool
	}{
		{time.Now().AddDate(1, 0, 0), false},
		{time.Now().Add(time.Minute), true},
	} {
		c, err := creds.Insert(ctx, db, "a", "max", "sk-ant-oat-a", "ref-"+tt.expires.String(), tt.expires, 5)
		if err != nil {
			t.Fatal(err)
		}
		before := calls.Load()
		rec := postRefresh(s, c.ID)
		var out struct {
			OK        bool `json:"ok"`
			Refreshed bool `json:"refreshed"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !out.OK || out.Refreshed != tt.refreshed {
			t.Fatalf("refresh = %d %s, want refreshed=%v", rec.Code, rec.Body.String(), tt.refreshed)
		}
		if made := calls.Load() - before; (made == 1) != tt.refreshed {
			t.Fatalf("token calls %d for refreshed=%v", made, tt.refreshed)
		}
	}
}

// AC7b: a revocation reached through the manual refresh logs one ERROR with
// the label.
func TestRefreshEndpointLogsRevocation(t *testing.T) {
	s, db, _, logs := refreshServer(t, http.StatusBadRequest)
	c, err := creds.Insert(context.Background(), db, "acct-web", "max", "sk-ant-oat-a", "ref-a", time.Now().Add(time.Minute), 5)
	if err != nil {
		t.Fatal(err)
	}
	if rec := postRefresh(s, c.ID); rec.Code != http.StatusBadGateway {
		t.Fatalf("refresh = %d %s, want 502", rec.Code, rec.Body.String())
	}
	n := 0
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "level=ERROR") && strings.Contains(line, "label=acct-web") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d ERROR lines with the label, want 1:\n%s", n, logs)
	}
}

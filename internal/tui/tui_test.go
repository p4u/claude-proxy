package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// TestModelRendersData headlessly drives the model (no TTY) to guard against
// panics in Init/Update/View and confirm loaded rows reach the tables.
func TestModelRendersData(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := creds.Insert(ctx, db, "acct-A", "max", "sk-ant-oat-x", "ref", time.Now().Add(time.Hour), 5); err != nil {
		t.Fatalf("insert cred: %v", err)
	}
	if _, err := usertoken.Create(ctx, db, "alice"); err != nil {
		t.Fatalf("create user: %v", err)
	}

	m := &model{db: db, refresher: creds.NewRefresher(db)}
	m.initTables()

	// Run the load commands synchronously and feed their messages back in.
	m.Update(m.loadCreds()())
	m.Update(m.loadUsers()())

	if view := m.View(); !strings.Contains(view, "acct-A") {
		t.Fatalf("credentials view missing cred label:\n%s", view)
	}

	// Switch to users tab and re-render.
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := m.View(); !strings.Contains(got, "alice") {
		t.Fatalf("users view missing user name:\n%s", got)
	}

	// Window resize and quit key should not panic.
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd == nil {
		t.Fatal("q should return a quit command")
	}
}

// TestPasteImport drives the paste-JSON → label → import flow headlessly and
// confirms a credential is created from pasted contents (no file involved).
func TestPasteImport(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Mock the Anthropic token endpoint the liveness check calls.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-fresh","refresh_token":"ref-fresh","expires_in":3600}`))
	}))
	defer srv.Close()
	prev := creds.TokenURL
	creds.SetTokenURL(srv.URL)
	defer creds.SetTokenURL(prev)

	m := &model{db: db, refresher: creds.NewRefresher(db)}
	m.initTables()

	// 'p' opens the paste overlay.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if m.inputKind != inputPasteJSON {
		t.Fatalf("expected paste mode, got %v", m.inputKind)
	}

	// Paste the JSON and submit with ctrl+s → chains to the label prompt.
	m.paste.SetValue(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-x","refreshToken":"ref-x","expiresAt":9999999999000,"subscriptionType":"max"}}`)
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m.inputKind != inputImportLabel {
		t.Fatalf("ctrl+s should advance to label prompt, got %v", m.inputKind)
	}

	// Enter a label and submit → returns the async import command; run it.
	m.input.SetValue("pasted-acct")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("label submit should return an import command")
	}
	if done, ok := cmd().(actionDoneMsg); ok && done.err != nil {
		t.Fatalf("import failed: %v", done.err)
	}

	list, err := creds.List(ctx, db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Label != "pasted-acct" {
		t.Fatalf("expected 1 credential labelled pasted-acct, got %+v", list)
	}
	if list[0].AccessToken != "sk-ant-oat-fresh" {
		t.Fatalf("expected refreshed access token, got %q", list[0].AccessToken)
	}
}

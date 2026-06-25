// Package tui is an interactive Bubble Tea management UI for the proxy's
// credentials and user tokens. It is a thin front-end over the same package
// functions the CLI uses (creds.*, usertoken.*, ingest.*), so its behaviour
// matches `claude-proxy creds`/`users` exactly.
package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usage"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// Run starts the management TUI and blocks until the user quits.
func Run(db *store.DB, refresher *creds.Refresher) error {
	m := &model{db: db, refresher: refresher}
	m.initTables()
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

type tab int

const (
	tabCreds tab = iota
	tabUsers
)

// inputKind identifies which prompt is active in the text-input overlay.
type inputKind int

const (
	inputNone inputKind = iota
	inputWeight
	inputImportFile
	inputImportLabel
	inputUpdateFile
	inputUserName
	inputPasteJSON
)

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	styleTab    = lipgloss.NewStyle().Padding(0, 2)
	styleActive = lipgloss.NewStyle().Padding(0, 2).Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("63"))
	styleHelp   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
)

type model struct {
	db        *store.DB
	refresher *creds.Refresher

	tab       tab
	credTable table.Model
	userTable table.Model
	creds     []*creds.Credential
	users     []*usertoken.UserToken

	input     textinput.Model
	paste     textarea.Model
	inputKind inputKind
	// pending carries state across multi-step prompts (e.g. import file→label,
	// or paste-JSON→label).
	pendingID    string
	pendingFile  string
	pendingLabel string
	pendingJSON  string

	confirm     bool
	confirmText string
	confirmFn   func() tea.Cmd

	status string
	isErr  bool
	busy   bool
	width  int
}

func (m *model) initTables() {
	m.credTable = table.New(
		table.WithColumns([]table.Column{
			{Title: "ID", Width: 22},
			{Title: "Label", Width: 14},
			{Title: "Sub", Width: 6},
			{Title: "Wt", Width: 3},
			{Title: "Status", Width: 9},
			{Title: "5h%", Width: 6},
			{Title: "7d%", Width: 6},
			{Title: "Expires", Width: 20},
		}),
		table.WithFocused(true),
		table.WithHeight(12),
	)
	m.userTable = table.New(
		table.WithColumns([]table.Column{
			{Title: "ID", Width: 22},
			{Title: "Name", Width: 20},
			{Title: "Status", Width: 9},
			{Title: "Created", Width: 20},
			{Title: "Last used", Width: 20},
		}),
		table.WithHeight(12),
	)
	ti := textinput.New()
	ti.CharLimit = 256
	ti.Width = 50
	m.input = ti

	ta := textarea.New()
	ta.Placeholder = `{"claudeAiOauth":{ ... }}`
	ta.SetWidth(72)
	ta.SetHeight(8)
	ta.CharLimit = 8192
	m.paste = ta
}

// --- messages ---

type credsLoadedMsg struct {
	creds []*creds.Credential
	snaps map[string]*usage.Snapshot
}
type usersLoadedMsg struct{ users []*usertoken.UserToken }
type actionDoneMsg struct {
	msg string
	err error
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.loadCreds(), m.loadUsers())
}

// --- data loading commands ---

func (m *model) loadCreds() tea.Cmd {
	db := m.db
	return func() tea.Msg {
		ctx := context.Background()
		list, err := creds.List(ctx, db)
		if err != nil {
			return actionDoneMsg{err: err}
		}
		snaps := make(map[string]*usage.Snapshot, len(list))
		for _, c := range list {
			s, _ := usage.LastSnapshot(ctx, db, c.ID)
			snaps[c.ID] = s
		}
		return credsLoadedMsg{creds: list, snaps: snaps}
	}
}

func (m *model) loadUsers() tea.Cmd {
	db := m.db
	return func() tea.Msg {
		list, err := usertoken.List(context.Background(), db)
		if err != nil {
			return actionDoneMsg{err: err}
		}
		return usersLoadedMsg{users: list}
	}
}

// --- selection helpers ---

func (m *model) selectedCred() *creds.Credential {
	i := m.credTable.Cursor()
	if i < 0 || i >= len(m.creds) {
		return nil
	}
	return m.creds[i]
}

func (m *model) selectedUser() *usertoken.UserToken {
	i := m.userTable.Cursor()
	if i < 0 || i >= len(m.users) {
		return nil
	}
	return m.users[i]
}

func pct(s *usage.Snapshot, fn func(*usage.Snapshot) float64) string {
	if s == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", fn(s))
}

func shortTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

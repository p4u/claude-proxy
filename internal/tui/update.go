package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/ingest"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/usage"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case credsLoadedMsg:
		m.creds = msg.creds
		rows := make([]table.Row, 0, len(msg.creds))
		for _, c := range msg.creds {
			s := msg.snaps[c.ID]
			p := provider.Get(c.Provider)
			// API keys have no real expiry and no usage snapshots; showing a
			// synthetic date and 0.0% would both be misinformation.
			expires := c.ExpiresAt.Local().Format("2006-01-02 15:04")
			if !p.Refreshable {
				expires = "never"
			}
			rows = append(rows, table.Row{
				c.ID, string(p.ID), c.Label, c.SubscriptionType, strconv.Itoa(c.Weight), string(c.Status),
				pct(s, func(s *usage.Snapshot) float64 { return s.FiveHourPct }),
				pct(s, func(s *usage.Snapshot) float64 { return s.SevenDayPct }),
				expires,
			})
		}
		m.credTable.SetRows(rows)
		return m, nil

	case usersLoadedMsg:
		m.users = msg.users
		rows := make([]table.Row, 0, len(msg.users))
		for _, u := range msg.users {
			rows = append(rows, table.Row{
				u.ID, u.Name, string(u.Status),
				shortTime(&u.CreatedAt), shortTime(u.LastUsedAt),
			})
		}
		m.userTable.SetRows(rows)
		return m, nil

	case actionDoneMsg:
		m.busy = false
		if msg.err != nil {
			m.status, m.isErr = msg.err.Error(), true
		} else {
			m.status, m.isErr = msg.msg, false
		}
		// Refresh both views after any mutation.
		return m, tea.Batch(m.loadCreds(), m.loadUsers())

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Forward other messages (e.g. cursor blink) to the active widget.
	return m, m.routeToWidget(msg)
}

func (m *model) routeToWidget(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	if m.inputKind == inputPasteJSON {
		m.paste, cmd = m.paste.Update(msg)
		return cmd
	}
	if m.inputKind != inputNone {
		m.input, cmd = m.input.Update(msg)
		return cmd
	}
	if m.tab == tabCreds {
		m.credTable, cmd = m.credTable.Update(msg)
	} else {
		m.userTable, cmd = m.userTable.Update(msg)
	}
	return cmd
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// 1) Confirmation prompt (delete) takes priority.
	if m.confirm {
		switch msg.String() {
		case "y", "Y":
			m.confirm = false
			fn := m.confirmFn
			m.confirmFn = nil
			m.busy = true
			return m, fn()
		case "n", "N", "esc":
			m.confirm = false
			m.confirmFn = nil
			m.status = "cancelled"
			return m, nil
		}
		return m, nil
	}

	// 2a) Multi-line paste overlay (JSON). enter inserts a newline; ctrl+s
	// submits the pasted document; esc cancels.
	if m.inputKind == inputPasteJSON {
		switch msg.String() {
		case "esc":
			m.resetInput()
			m.status = "cancelled"
			return m, nil
		case "ctrl+s":
			return m.submitInput()
		}
		var cmd tea.Cmd
		m.paste, cmd = m.paste.Update(msg)
		return m, cmd
	}

	// 2b) Single-line text-input overlay.
	if m.inputKind != inputNone {
		switch msg.String() {
		case "esc":
			m.resetInput()
			m.status = "cancelled"
			return m, nil
		case "enter":
			return m.submitInput()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// 3) Normal navigation.
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		if m.tab == tabCreds {
			m.tab = tabUsers
			m.credTable.Blur()
			m.userTable.Focus()
		} else {
			m.tab = tabCreds
			m.userTable.Blur()
			m.credTable.Focus()
		}
		return m, nil
	}

	if m.tab == tabCreds {
		return m.handleCredKey(msg)
	}
	return m.handleUserKey(msg)
}

func (m *model) handleCredKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "i": // import a new credential from a file
		m.startInput(inputImportFile, "Import — path to .credentials.json: ", "")
		return m, nil
	case "p": // import a new credential by pasting JSON
		m.startPaste()
		return m, nil
	case "k": // add a static API-key credential (GLM / MiMo)
		m.startInput(inputAPIKey, "API key (glm or mimo): ", "")
		return m, nil
	}

	c := m.selectedCred()
	if c == nil {
		var cmd tea.Cmd
		m.credTable, cmd = m.credTable.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "r": // force token refresh
		m.busy = true
		m.status = "refreshing " + c.ID + "…"
		return m, m.refreshCred(c.ID)
	case "u": // update tokens from a fresh login file
		m.pendingID = c.ID
		m.startInput(inputUpdateFile, "Update "+c.ID+" — path to fresh .credentials.json: ", "")
		return m, nil
	case "e": // change the endpoint of an API-key credential
		if provider.Get(c.Provider).Refreshable {
			m.status, m.isErr = "OAuth credentials have a fixed endpoint", true
			return m, nil
		}
		m.pendingID = c.ID
		m.pendingProvider = c.Provider
		m.startInput(inputEditEndpoint, endpointPrompt(c.Provider),
			provider.EndpointName(c.Provider, c.BaseURL))
		return m, nil
	case "w": // set weight
		m.pendingID = c.ID
		m.startInput(inputWeight, "New weight for "+c.ID+": ", strconv.Itoa(c.Weight))
		return m, nil
	case "d": // toggle disabled/active
		m.busy = true
		return m, m.toggleCred(c)
	case "x": // delete (confirm)
		id := c.ID
		m.confirm = true
		m.confirmText = "Delete credential " + id + "? (y/n)"
		m.confirmFn = func() tea.Cmd { return m.deleteCred(id) }
		return m, nil
	}

	var cmd tea.Cmd
	m.credTable, cmd = m.credTable.Update(msg)
	return m, cmd
}

func (m *model) handleUserKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "c" { // create a user
		m.startInput(inputUserName, "New user name: ", "")
		return m, nil
	}

	u := m.selectedUser()
	if u == nil {
		var cmd tea.Cmd
		m.userTable, cmd = m.userTable.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "d": // toggle disabled/active
		m.busy = true
		return m, m.toggleUser(u)
	case "R": // rotate token
		m.busy = true
		return m, m.rotateUser(u.ID)
	case "x": // delete (confirm)
		id := u.ID
		m.confirm = true
		m.confirmText = "Delete user token " + id + "? (y/n)"
		m.confirmFn = func() tea.Cmd { return m.deleteUser(id) }
		return m, nil
	}

	var cmd tea.Cmd
	m.userTable, cmd = m.userTable.Update(msg)
	return m, cmd
}

// --- input handling ---

func (m *model) startInput(kind inputKind, prompt, value string) {
	m.inputKind = kind
	m.input.Prompt = prompt
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.input.Focus()
}

func (m *model) startPaste() {
	m.inputKind = inputPasteJSON
	m.paste.Reset()
	m.paste.Focus()
}

func (m *model) resetInput() {
	m.inputKind = inputNone
	m.input.Blur()
	m.input.SetValue("")
	m.paste.Blur()
	m.paste.Reset()
	m.pendingID, m.pendingFile, m.pendingLabel, m.pendingJSON = "", "", "", ""
	m.pendingProvider = ""
}

func (m *model) submitInput() (tea.Model, tea.Cmd) {
	// The paste overlay submits the textarea contents, not the text input.
	if m.inputKind == inputPasteJSON {
		js := strings.TrimSpace(m.paste.Value())
		if js == "" {
			m.status, m.isErr = "paste the .credentials.json contents first (ctrl+s to submit)", true
			return m, nil
		}
		m.pendingJSON = js
		m.paste.Blur()
		m.startInput(inputImportLabel, "Label (blank = subscription type): ", "")
		return m, nil
	}

	val := strings.TrimSpace(m.input.Value())
	switch m.inputKind {
	case inputWeight:
		id := m.pendingID
		m.resetInput()
		w, err := strconv.Atoi(val)
		if err != nil || w < 1 {
			m.status, m.isErr = "weight must be a positive integer", true
			return m, nil
		}
		m.busy = true
		return m, m.setWeight(id, w)
	case inputImportFile:
		if val == "" {
			m.status, m.isErr = "path is required", true
			return m, nil
		}
		m.pendingFile = val
		m.startInput(inputImportLabel, "Label (blank = subscription type): ", "")
		return m, nil
	case inputImportLabel:
		// Either a pasted JSON document or a file path produced this label step.
		if m.pendingJSON != "" {
			js, label := m.pendingJSON, val
			m.resetInput()
			m.busy = true
			m.status = "importing pasted credentials…"
			return m, m.importCredJSON(js, label)
		}
		file, label := m.pendingFile, val
		m.resetInput()
		m.busy = true
		m.status = "importing " + file + "…"
		return m, m.importCred(file, label)
	case inputAPIKey:
		if val == "" {
			m.status, m.isErr = "API key is required", true
			return m, nil
		}
		m.pendingJSON = val // reused as the pending secret between steps
		// The provider is inferred from the key's own prefix: MiMo Token Plan
		// keys start with "tp-", Z.AI keys do not. Asking would be one more
		// prompt for something the key already states.
		p := provider.GLM
		if strings.HasPrefix(val, "tp-") || strings.HasPrefix(val, "sk-") {
			p = provider.MiMo
		}
		m.pendingProvider = p
		m.startInput(inputAPIKeyEndpoint, endpointPrompt(p), "")
		return m, nil
	case inputAPIKeyEndpoint:
		m.pendingLabel = val // endpoint choice, carried to the next step
		m.startInput(inputAPIKeyLabel, "Label (blank = provider name): ", "")
		return m, nil
	case inputAPIKeyLabel:
		key, endpoint, label := m.pendingJSON, m.pendingLabel, val
		p := m.pendingProvider
		m.resetInput()
		m.busy = true
		m.status = "verifying API key…"
		return m, m.addKeyCred(p, key, label, endpoint)
	case inputEditEndpoint:
		id := m.pendingID
		m.resetInput()
		if val == "" {
			m.status, m.isErr = "endpoint is required", true
			return m, nil
		}
		m.busy = true
		m.status = "verifying key at the new endpoint…"
		return m, m.setEndpoint(id, val)
	case inputUpdateFile:
		id, file := m.pendingID, val
		m.resetInput()
		if file == "" {
			m.status, m.isErr = "path is required", true
			return m, nil
		}
		m.busy = true
		m.status = "updating " + id + "…"
		return m, m.updateCred(id, file)
	case inputUserName:
		m.resetInput()
		if val == "" {
			m.status, m.isErr = "name is required", true
			return m, nil
		}
		m.busy = true
		return m, m.createUser(val)
	}
	m.resetInput()
	return m, nil
}

// --- action commands (run async; return actionDoneMsg) ---

func (m *model) refreshCred(id string) tea.Cmd {
	r := m.refresher
	return func() tea.Msg {
		c, err := r.RefreshNow(context.Background(), id)
		if err != nil {
			return actionDoneMsg{err: fmt.Errorf("refresh %s: %w", id, err)}
		}
		return actionDoneMsg{msg: fmt.Sprintf("refreshed %s (expires %s)", id, c.ExpiresAt.Local().Format("15:04"))}
	}
}

func (m *model) updateCred(id, file string) tea.Cmd {
	db := m.db
	return func() tea.Msg {
		c, err := ingest.UpdateFromFile(context.Background(), db, id, file)
		if err != nil {
			return actionDoneMsg{err: fmt.Errorf("update %s: %w", id, err)}
		}
		return actionDoneMsg{msg: fmt.Sprintf("updated %s → active (expires %s)", id, c.ExpiresAt.Local().Format("15:04"))}
	}
}

func (m *model) importCred(file, label string) tea.Cmd {
	db := m.db
	return func() tea.Msg {
		c, err := ingest.Import(context.Background(), db, file, label, 0)
		if err != nil {
			return actionDoneMsg{err: fmt.Errorf("import: %w", err)}
		}
		return actionDoneMsg{msg: fmt.Sprintf("imported %s (%s)", c.ID, c.Label)}
	}
}

func (m *model) importCredJSON(js, label string) tea.Cmd {
	db := m.db
	return func() tea.Msg {
		c, err := ingest.ImportFromJSON(context.Background(), db, []byte(js), label, 0)
		if err != nil {
			return actionDoneMsg{err: fmt.Errorf("import (paste): %w", err)}
		}
		return actionDoneMsg{msg: fmt.Sprintf("imported %s (%s)", c.ID, c.Label)}
	}
}

// addKeyCred verifies and stores a static API-key credential. Verification hits
// the provider's /v1/models, so this blocks briefly — the caller sets busy.
// endpointPrompt lists a provider's selectable clusters inline, because a key
// bound to the wrong one fails with a bare "Invalid API Key" that says nothing
// about the region.
func endpointPrompt(p provider.ID) string {
	eps := provider.Get(p).Endpoints
	if len(eps) == 0 {
		return "Endpoint (blank = default): "
	}
	names := make([]string, 0, len(eps))
	for _, e := range eps {
		names = append(names, e.Name)
	}
	return "Endpoint [" + strings.Join(names, "|") + "] or https://… (blank = " + eps[0].Name + "): "
}

func (m *model) addKeyCred(p provider.ID, apiKey, label, endpoint string) tea.Cmd {
	db := m.db
	return func() tea.Msg {
		c, err := ingest.ImportKey(context.Background(), db, p, label, "", apiKey, endpoint, 0)
		if err != nil {
			return actionDoneMsg{err: fmt.Errorf("add key: %w", err)}
		}
		return actionDoneMsg{msg: fmt.Sprintf("added %s (%s, %s)", c.ID, c.Label, c.Provider)}
	}
}

// setEndpoint re-points a key credential; the key is re-verified against the
// new endpoint before anything is written, so a wrong pick cannot break a
// working credential.
func (m *model) setEndpoint(id, endpoint string) tea.Cmd {
	db := m.db
	return func() tea.Msg {
		c, err := ingest.UpdateKeyEndpoint(context.Background(), db, id, endpoint)
		if err != nil {
			return actionDoneMsg{err: fmt.Errorf("set endpoint: %w", err)}
		}
		return actionDoneMsg{msg: fmt.Sprintf("%s → %s (%s)",
			c.ID, provider.ResolveBaseURL(c.Provider, c.BaseURL), c.Status)}
	}
}

func (m *model) setWeight(id string, w int) tea.Cmd {
	db := m.db
	return func() tea.Msg {
		if err := creds.SetWeight(context.Background(), db, id, w); err != nil {
			return actionDoneMsg{err: err}
		}
		return actionDoneMsg{msg: fmt.Sprintf("set weight %d on %s", w, id)}
	}
}

func (m *model) toggleCred(c *creds.Credential) tea.Cmd {
	db := m.db
	id := c.ID
	target := creds.StatusDisabled
	verb := "disabled"
	if c.Status == creds.StatusDisabled {
		target, verb = creds.StatusActive, "enabled"
	}
	return func() tea.Msg {
		if err := creds.SetStatus(context.Background(), db, id, target); err != nil {
			return actionDoneMsg{err: err}
		}
		return actionDoneMsg{msg: verb + " " + id}
	}
}

func (m *model) deleteCred(id string) tea.Cmd {
	db := m.db
	return func() tea.Msg {
		if err := creds.Delete(context.Background(), db, id); err != nil {
			return actionDoneMsg{err: err}
		}
		return actionDoneMsg{msg: "removed " + id}
	}
}

func (m *model) createUser(name string) tea.Cmd {
	db := m.db
	return func() tea.Msg {
		ut, err := usertoken.Create(context.Background(), db, name)
		if err != nil {
			return actionDoneMsg{err: err}
		}
		return actionDoneMsg{msg: fmt.Sprintf("created %s  token=%s", ut.ID, ut.Token)}
	}
}

func (m *model) toggleUser(u *usertoken.UserToken) tea.Cmd {
	db := m.db
	id := u.ID
	target := usertoken.StatusDisabled
	verb := "disabled"
	if u.Status == usertoken.StatusDisabled {
		target, verb = usertoken.StatusActive, "enabled"
	}
	return func() tea.Msg {
		if err := usertoken.SetStatus(context.Background(), db, id, target); err != nil {
			return actionDoneMsg{err: err}
		}
		return actionDoneMsg{msg: verb + " " + id}
	}
}

func (m *model) rotateUser(id string) tea.Cmd {
	db := m.db
	return func() tea.Msg {
		tok, err := usertoken.Refresh(context.Background(), db, id)
		if err != nil {
			return actionDoneMsg{err: err}
		}
		return actionDoneMsg{msg: fmt.Sprintf("rotated %s  token=%s", id, tok)}
	}
}

func (m *model) deleteUser(id string) tea.Cmd {
	db := m.db
	return func() tea.Msg {
		if err := usertoken.Delete(context.Background(), db, id); err != nil {
			return actionDoneMsg{err: err}
		}
		return actionDoneMsg{msg: "removed " + id}
	}
}

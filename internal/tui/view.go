package tui

import "strings"

func (m *model) View() string {
	var b strings.Builder

	// Title + tab bar.
	b.WriteString(styleTitle.Render("claude-proxy") + "  ")
	b.WriteString(tabLabel("Credentials", m.tab == tabCreds))
	b.WriteString(tabLabel("Users", m.tab == tabUsers))
	b.WriteString("\n\n")

	// Active table.
	if m.tab == tabCreds {
		b.WriteString(m.credTable.View())
	} else {
		b.WriteString(m.userTable.View())
	}
	b.WriteString("\n")

	// Overlay: confirm prompt, text input, or status line.
	switch {
	case m.confirm:
		b.WriteString("\n" + styleErr.Render(m.confirmText))
	case m.inputKind == inputPasteJSON:
		b.WriteString("\n" + styleHelp.Render("Paste the .credentials.json contents, then ctrl+s to import (esc to cancel):") + "\n")
		b.WriteString(m.paste.View())
	case m.inputKind != inputNone:
		b.WriteString("\n" + m.input.View())
	case m.status != "":
		style := styleOK
		prefix := "✓ "
		if m.isErr {
			style, prefix = styleErr, "✗ "
		}
		b.WriteString("\n" + style.Render(prefix+m.status))
	default:
		b.WriteString("\n")
	}

	// Help footer.
	b.WriteString("\n\n" + styleHelp.Render(m.helpLine()))
	return b.String()
}

func tabLabel(name string, active bool) string {
	if active {
		return styleActive.Render(name) + " "
	}
	return styleTab.Render(name) + " "
}

func (m *model) helpLine() string {
	if m.confirm {
		return "y confirm · n/esc cancel"
	}
	if m.inputKind == inputPasteJSON {
		return "ctrl+s import · esc cancel"
	}
	if m.inputKind != inputNone {
		return "enter submit · esc cancel"
	}
	common := "↑/↓ move · tab switch · q quit"
	if m.tab == tabCreds {
		return "creds: r refresh · u update-token · w weight · d disable/enable · x delete · i import · p paste   " + common
	}
	return "users: c create · R rotate-token · d disable/enable · x delete   " + common
}

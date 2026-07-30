package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/desenyon/modelrouter/internal/api"
)

type ProvidersView struct {
	providers     []api.Provider
	tbl           table.Model
	width, height int
}

func NewProvidersView(providers []api.Provider, w, h int) ProvidersView {
	tbl := table.New(table.WithFocused(true))
	st := table.DefaultStyles()
	st.Header = st.Header.Bold(true).Foreground(cPrimary).
		BorderStyle(lipgloss.NormalBorder()).BorderForeground(cFaint).BorderBottom(true)
	st.Selected = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Background(cPrimary).Bold(true)
	tbl.SetStyles(st)

	v := ProvidersView{providers: providers, tbl: tbl}
	v.SetSize(w, h)

	v.setRows()
	return v
}

func (v *ProvidersView) policy(p api.Provider) string {
	if !p.PolicyAvailable {
		return "unknown"
	}
	training := "no-train"
	if p.DataPolicy.Training || p.DataPolicy.TrainingOpenRouter {
		training = "trains"
	}
	retention := "no-log"
	if p.DataPolicy.RetainsPrompts {
		retention = "logs"
	}
	userID := "no-id"
	if p.DataPolicy.RequiresUserIDs {
		userID = "user-id"
	}
	flags := []string{training, retention, userID}
	if p.ModerationRequired {
		flags = append(flags, "moderated")
	}
	return strings.Join(flags, " ")
}

func (v *ProvidersView) compactPolicy(p api.Provider) string {
	if !p.PolicyAvailable {
		return "unknown"
	}
	training, retention, userID := "T-", "L-", "ID-"
	if p.DataPolicy.Training || p.DataPolicy.TrainingOpenRouter {
		training = "T+"
	}
	if p.DataPolicy.RetainsPrompts {
		retention = "L+"
	}
	if p.DataPolicy.RequiresUserIDs {
		userID = "ID+"
	}
	flags := []string{training, retention, userID}
	if p.ModerationRequired {
		flags = append(flags, "M")
	}
	return strings.Join(flags, " ")
}

func (v *ProvidersView) setRows() {
	rows := make([]table.Row, len(v.providers))
	for i, p := range v.providers {
		hq := p.Headquarters
		if hq == "" {
			hq = "-"
		}
		chat := "legacy"
		if p.HasChatCompletions {
			chat = "chat"
		}
		byok := "·"
		if p.BYOKEnabled {
			byok = "✓"
		}
		links := 0
		for _, u := range []string{p.PrivacyURL(), p.TermsURL(), p.StatusURL()} {
			if u != "" {
				links++
			}
		}
		if v.width < 110 {
			rows[i] = table.Row{p.Label(), hq, chat, byok, v.compactPolicy(p)}
			continue
		}
		rows[i] = table.Row{p.Label(), p.Slug, hq, chat, byok, v.policy(p), fmt.Sprintf("%d/3", links)}
	}
	v.tbl.SetRows(rows)
}

func (v *ProvidersView) SetProviders(providers []api.Provider) {
	cursor := v.tbl.Cursor()
	v.providers = providers
	v.setRows()
	if cursor < len(providers) {
		v.tbl.SetCursor(cursor)
	}
}

func (v *ProvidersView) SetSize(w, h int) {
	v.width, v.height = w, h
	if h < 4 {
		h = 4
	}
	v.tbl.SetHeight(h - 2)
	if w < 110 {
		nameW := w - 43
		if nameW < 18 {
			nameW = 18
		}
		v.tbl.SetColumns([]table.Column{
			{Title: "PROVIDER", Width: nameW},
			{Title: "HQ", Width: 4},
			{Title: "API", Width: 7},
			{Title: "BYOK", Width: 5},
			{Title: "POLICY", Width: 20},
		})
		v.setRows()
		return
	}
	nameW := w - (20 + 4 + 7 + 5 + 32 + 7) - 16
	if nameW < 18 {
		nameW = 18
	}
	if nameW > 36 {
		nameW = 36
	}
	v.tbl.SetColumns([]table.Column{
		{Title: "PROVIDER", Width: nameW},
		{Title: "SLUG", Width: 20},
		{Title: "HQ", Width: 4},
		{Title: "API", Width: 7},
		{Title: "BYOK", Width: 5},
		{Title: "DATA POLICY", Width: 32},
		{Title: "LINKS", Width: 7},
	})
	v.setRows()
}

func (v *ProvidersView) Update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	v.tbl, cmd = v.tbl.Update(msg)
	return cmd
}

func (v *ProvidersView) HandleMouse(msg tea.MouseMsg) {
	if msg.Action != tea.MouseActionPress {
		return
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		v.tbl.MoveUp(2)
	case tea.MouseButtonWheelDown:
		v.tbl.MoveDown(2)
	}
}

func (v *ProvidersView) View() string {
	explanation := "trains/logs/user-id mark provider use; no-* marks the opposite"
	if v.width < 110 {
		explanation = "T/L/ID: +=used, -=not used · M=moderated"
	}
	header := " " + styleDim.Render(fmt.Sprintf("%d providers · %s", len(v.providers), explanation))
	return header + "\n" + v.tbl.View()
}

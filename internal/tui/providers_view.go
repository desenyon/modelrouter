package tui

import (
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

	rows := make([]table.Row, len(providers))
	check := func(u *string) string {
		if u != nil && *u != "" {
			return "✓"
		}
		return "·"
	}
	for i, p := range providers {
		dc := strings.Join(p.Datacenters, ",")
		if dc == "" {
			dc = "-"
		}
		hq := p.Headquarters
		if hq == "" {
			hq = "-"
		}
		rows[i] = table.Row{p.Name, p.Slug, hq, dc, check(p.PrivacyPolicyURL), check(p.TermsOfServiceURL), check(p.StatusPageURL)}
	}
	v.tbl.SetRows(rows)
	return v
}

func (v *ProvidersView) SetSize(w, h int) {
	v.width, v.height = w, h
	if h < 4 {
		h = 4
	}
	v.tbl.SetHeight(h - 2)
	nameW := w - (18 + 6 + 24 + 8 + 5 + 7) - 16
	if nameW < 18 {
		nameW = 18
	}
	if nameW > 36 {
		nameW = 36
	}
	v.tbl.SetColumns([]table.Column{
		{Title: "PROVIDER", Width: nameW},
		{Title: "SLUG", Width: 18},
		{Title: "HQ", Width: 6},
		{Title: "DATACENTERS", Width: 24},
		{Title: "PRIVACY", Width: 8},
		{Title: "TOS", Width: 5},
		{Title: "STATUS", Width: 7},
	})
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
	header := " " + styleDim.Render("inference providers routed through OpenRouter")
	return header + "\n" + v.tbl.View()
}

package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	lgtable "github.com/charmbracelet/lipgloss/table"

	"github.com/desenyon/modelrouter/internal/api"
)

var appPeriods = []string{"day", "week", "month"}

type RankingsView struct {
	rankings  *api.Rankings
	err       error
	periodIdx int

	vp            viewport.Model
	width, height int
}

func NewRankingsView(w, h int) RankingsView {
	v := RankingsView{periodIdx: 1} // default: week
	v.vp = viewport.New(w, h)
	v.SetSize(w, h)
	return v
}

func (v *RankingsView) SetSize(w, h int) {
	v.width, v.height = w, h
	v.vp.Width = w
	v.vp.Height = h
	v.vp.SetContent(v.content())
}

func (v *RankingsView) SetData(r *api.Rankings, err error) {
	v.rankings, v.err = r, err
	v.vp.SetContent(v.content())
}

func (v *RankingsView) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "p":
			v.periodIdx = (v.periodIdx + 1) % len(appPeriods)
			y := v.vp.YOffset
			v.vp.SetContent(v.content())
			v.vp.SetYOffset(y)
			return nil
		case "g", "home":
			v.vp.GotoTop()
			return nil
		case "G", "end":
			v.vp.GotoBottom()
			return nil
		}
	}
	var cmd tea.Cmd
	v.vp, cmd = v.vp.Update(msg)
	return cmd
}

func (v *RankingsView) View() string { return v.vp.View() }

func (v *RankingsView) ScrollPercent() float64 { return v.vp.ScrollPercent() }

func (v *RankingsView) content() string {
	if v.err != nil && v.rankings == nil {
		return "\n " + styleErr.Render("rankings unavailable: ") + v.err.Error() +
			"\n " + styleDim.Render("(unofficial frontend API — may have changed)")
	}
	if v.rankings == nil {
		return "\n " + styleDim.Render("loading rankings...")
	}
	r := v.rankings
	var b strings.Builder
	if v.err != nil {
		b.WriteString(" " + styleErr.Render("live refresh failed · showing prior rankings snapshot") + "\n")
	}

	if !r.FetchedAt.IsZero() {
		b.WriteString(" " + styleDim.Render(
			"updated "+r.FetchedAt.Format("15:04:05")+" · auto-refreshes every 5m") + "\n\n")
	}

	// --- top models by token usage ---
	top := r.TopModels()
	b.WriteString(" " + styleSection.Render("TOP MODELS BY TOKENS") +
		styleDim.Render("  ("+r.LatestDate()+", prompt+completion)") + "\n")
	limit := 20
	if len(top) < limit {
		limit = len(top)
	}
	var maxTok int64 = 1
	if len(top) > 0 {
		maxTok = top[0].TotalTokens
	}
	barW := v.width - 64
	if barW > 36 {
		barW = 36
	}
	if barW < 8 {
		barW = 8
	}
	for i, u := range top[:limit] {
		change := "      "
		if u.Change != nil {
			if *u.Change >= 0 {
				change = lipgloss.NewStyle().Foreground(cAccent).Render(fmt.Sprintf("▲%4.0f%%", *u.Change*100))
			} else {
				change = lipgloss.NewStyle().Foreground(cRed).Render(fmt.Sprintf("▼%4.0f%%", -*u.Change*100))
			}
		}
		b.WriteString(fmt.Sprintf(" %s %s %s %s %s %s\n",
			styleDim.Render(fmt.Sprintf("%2d", i+1)),
			lipgloss.NewStyle().Foreground(cCyan).Width(42).Render(truncate(u.Slug, 42)),
			bar(int(u.TotalTokens/1e6), int(maxTok/1e6), barW, cPrimary),
			lipgloss.NewStyle().Foreground(cText).Bold(true).Width(7).Align(lipgloss.Right).Render(api.FmtTokens(u.TotalTokens)),
			styleDim.Render(fmt.Sprintf("%8s req", api.FmtTokens(u.Requests))),
			change))
	}
	b.WriteString("\n")

	// --- market share ---
	week, share := r.LatestShare()
	if len(share) > 0 {
		b.WriteString(" " + styleSection.Render("MARKET SHARE BY AUTHOR") + styleDim.Render("  (week of "+week+")") + "\n")
		var total int64
		for _, it := range share {
			total += it.Count
		}
		var maxShare int64 = 1
		for _, it := range share {
			if it.Count > maxShare {
				maxShare = it.Count
			}
		}
		hues := []lipgloss.Color{cPrimary, cPink, cCyan, cAccent, cYellow}
		for i, it := range share {
			pct := float64(it.Count) / float64(total) * 100
			b.WriteString(fmt.Sprintf(" %s %s %s %s\n",
				lipgloss.NewStyle().Foreground(cText).Width(12).Render(it.Label),
				bar(int(it.Count/1e6), int(maxShare/1e6), 36, hues[i%len(hues)]),
				lipgloss.NewStyle().Foreground(cText).Bold(true).Width(7).Align(lipgloss.Right).Render(api.FmtTokens(it.Count)),
				styleDim.Render(fmt.Sprintf("%5.1f%%", pct))))
		}
		b.WriteString("\n")
	}

	// --- top apps ---
	period := appPeriods[v.periodIdx]
	apps := map[string][]api.AppRank{"day": r.Apps.Day, "week": r.Apps.Week, "month": r.Apps.Month}[period]
	b.WriteString(" " + styleSection.Render("TOP APPS") +
		styleDim.Render("  (per "+period+" — press p to cycle)") + "\n")
	at := lgtable.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(cFaint)).
		Headers("#", "APP", "TOKENS", "REQUESTS", "URL").
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == lgtable.HeaderRow {
				return lipgloss.NewStyle().Foreground(cPrimary).Bold(true).Padding(0, 1)
			}
			return lipgloss.NewStyle().Foreground(cText).Padding(0, 1)
		})
	alimit := 15
	if len(apps) < alimit {
		alimit = len(apps)
	}
	for _, a := range apps[:alimit] {
		at.Row(fmt.Sprintf("%d", a.Rank), truncate(a.App.Title, 32),
			api.FmtTokens(a.Tokens()), api.FmtTokens(a.TotalRequests), truncate(a.App.MainURL, 36))
	}
	b.WriteString(at.Render() + "\n\n")

	// --- performance ---
	b.WriteString(" " + styleSection.Render("PERFORMANCE LEADERS") + styleDim.Render("  (p50, busiest models)") + "\n")
	pt := lgtable.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(cFaint)).
		Headers("MODEL", "REQUESTS", "P50 LATENCY", "P50 TPS", "FASTEST VIA", "CHEAP-FAST VIA").
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == lgtable.HeaderRow {
				return lipgloss.NewStyle().Foreground(cPrimary).Bold(true).Padding(0, 1)
			}
			return lipgloss.NewStyle().Foreground(cText).Padding(0, 1)
		})
	perf := r.Performance
	plimit := 15
	if len(perf) < plimit {
		plimit = len(perf)
	}
	for _, p := range perf[:plimit] {
		pt.Row(truncate(p.ID, 40), api.FmtTokens(p.RequestCount),
			fmt.Sprintf("%.0fms", p.P50Latency), fmt.Sprintf("%.0f", p.P50Throughput),
			p.BestThroughputProvider, p.BestLatencyProvider)
	}
	b.WriteString(pt.Render() + "\n\n")

	b.WriteString(" " + styleSection.Render("SCRIPTABLE COMMANDS") + "\n")
	for _, c := range []string{
		"modelrouter rankings              # top models by tokens",
		"modelrouter rankings share        # author market share",
		"modelrouter rankings apps --period day|week|month",
		"modelrouter rankings perf         # latency/throughput leaders",
		"modelrouter rankings all --json   # everything, machine-readable",
	} {
		b.WriteString(" " + lipgloss.NewStyle().Foreground(cYellow).Render("$ ") + styleDim.Render(c) + "\n")
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

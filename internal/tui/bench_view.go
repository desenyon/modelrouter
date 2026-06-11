package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/desenyon/modelrouter/internal/api"
)

type BenchView struct {
	bench *api.Benchmarks
	err   error

	vp            viewport.Model
	width, height int
}

func NewBenchView(w, h int) BenchView {
	v := BenchView{}
	v.vp = viewport.New(w, h)
	v.SetSize(w, h)
	return v
}

func (v *BenchView) SetSize(w, h int) {
	v.width, v.height = w, h
	v.vp.Width = w
	v.vp.Height = h
	v.vp.SetContent(v.content())
}

func (v *BenchView) SetData(b *api.Benchmarks, err error) {
	v.bench, v.err = b, err
	v.vp.SetContent(v.content())
}

func (v *BenchView) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
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

func (v *BenchView) View() string { return v.vp.View() }

func (v *BenchView) ScrollPercent() float64 { return v.vp.ScrollPercent() }

func (v *BenchView) aaChart(category string, color lipgloss.Color) string {
	rows := v.bench.TopAA(category)
	var b strings.Builder
	b.WriteString(" " + styleSection.Render("AA "+strings.ToUpper(category)) + "\n")
	if len(rows) == 0 {
		return b.String() + styleDim.Render(" no data") + "\n"
	}
	maxScore := rows[0].Score
	barW := 24
	for i, r := range rows {
		if i >= 12 {
			break
		}
		b.WriteString(fmt.Sprintf(" %s %s %s %s\n",
			styleDim.Render(fmt.Sprintf("%2d", i+1)),
			lipgloss.NewStyle().Foreground(cCyan).Width(38).Render(truncate(r.Slug(), 38)),
			bar(int(r.Score*10), int(maxScore*10), barW, color),
			lipgloss.NewStyle().Foreground(cText).Bold(true).Render(fmt.Sprintf("%5.1f", r.Score))))
	}
	return b.String()
}

func (v *BenchView) content() string {
	if v.err != nil {
		return "\n " + styleErr.Render("benchmarks unavailable: ") + v.err.Error() +
			"\n " + styleDim.Render("(unofficial frontend API — may have changed)")
	}
	if v.bench == nil {
		return "\n " + styleDim.Render("loading benchmarks...")
	}
	var b strings.Builder
	b.WriteString(" " + styleDim.Render("Artificial Analysis scores + Design Arena elo, as shown on openrouter.ai/rankings") + "\n\n")

	hues := []lipgloss.Color{cPrimary, cAccent, cPink}
	for i, cat := range api.AACategories {
		b.WriteString(v.aaChart(cat, hues[i%len(hues)]) + "\n")
	}

	b.WriteString(" " + styleSection.Render("DESIGN ARENA") + styleDim.Render("  (elo · win rate)") + "\n")
	var cols []string
	for _, dc := range api.DACategories {
		rows := v.bench.TopDA(dc.Key)
		if len(rows) == 0 {
			continue
		}
		var col strings.Builder
		col.WriteString(lipgloss.NewStyle().Foreground(cYellow).Bold(true).Render(dc.Label) + "\n")
		for i, r := range rows {
			if i >= 5 {
				break
			}
			col.WriteString(fmt.Sprintf("%s %s\n",
				lipgloss.NewStyle().Foreground(cCyan).Width(26).Render(truncate(r.DisplayName, 26)),
				styleDim.Render(fmt.Sprintf("%4.0f · %4.1f%%", r.Score, r.WinRate))))
		}
		cols = append(cols, lipgloss.NewStyle().MarginRight(3).MarginBottom(1).Render(col.String()))
	}
	// flow the category columns into rows that fit the width
	perRow := v.width / 44
	if perRow < 1 {
		perRow = 1
	}
	for i := 0; i < len(cols); i += perRow {
		end := i + perRow
		if end > len(cols) {
			end = len(cols)
		}
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, cols[i:end]...) + "\n")
	}

	cheap := v.bench.CheapestRequests()
	if len(cheap) > 0 {
		b.WriteString(" " + styleSection.Render("ESTIMATED COST PER REQUEST") + styleDim.Render("  (OpenRouter blended estimate, cheapest first)") + "\n")
		limit := 15
		if len(cheap) < limit {
			limit = len(cheap)
		}
		maxCost := cheap[len(cheap)-1].Cost
		for i, c := range cheap[:limit] {
			b.WriteString(fmt.Sprintf(" %s %s %s %s\n",
				styleDim.Render(fmt.Sprintf("%2d", i+1)),
				lipgloss.NewStyle().Foreground(cCyan).Width(38).Render(truncate(c.Slug, 38)),
				bar(int(c.Cost*1000), int(maxCost*1000), 24, cAccent),
				lipgloss.NewStyle().Foreground(cText).Bold(true).Render(fmt.Sprintf("$%.3f", c.Cost))))
		}
	}
	return b.String()
}

package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/desenyon/modelrouter/internal/api"
)

type StatsView struct {
	stats         api.Stats
	vp            viewport.Model
	width, height int
}

func NewStatsView(s api.Stats, w, h int) StatsView {
	v := StatsView{stats: s}
	v.vp = viewport.New(w, h)
	v.SetSize(w, h)
	return v
}

func (v *StatsView) SetSize(w, h int) {
	v.width, v.height = w, h
	v.vp.Width = w
	v.vp.Height = h
	v.vp.SetContent(v.content())
}

func (v *StatsView) Update(msg tea.Msg) tea.Cmd {
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

func (v *StatsView) View() string { return v.vp.View() }

func (v *StatsView) ScrollPercent() float64 { return v.vp.ScrollPercent() }

func statCard(label, value string, color lipgloss.Color) string {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(cFaint).
		Padding(0, 2).Align(lipgloss.Center).
		Render(lipgloss.NewStyle().Foreground(color).Bold(true).Render(value) + "\n" + styleDim.Render(label))
}

func (v *StatsView) barChart(title string, items []api.CountItem, color lipgloss.Color) string {
	var b strings.Builder
	b.WriteString(" " + styleSection.Render(title) + "\n")
	maxCount, maxLabel := 1, 0
	for _, it := range items {
		if it.Count > maxCount {
			maxCount = it.Count
		}
		if len(it.Label) > maxLabel {
			maxLabel = len(it.Label)
		}
	}
	barW := v.width - maxLabel - 12
	if barW > 48 {
		barW = 48
	}
	if barW < 10 {
		barW = 10
	}
	for _, it := range items {
		b.WriteString(fmt.Sprintf(" %s %s %s\n",
			lipgloss.NewStyle().Foreground(cText).Width(maxLabel).Render(it.Label),
			bar(it.Count, maxCount, barW, color),
			styleDim.Render(fmt.Sprintf("%d", it.Count))))
	}
	return b.String()
}

func (v *StatsView) content() string {
	s := v.stats
	var b strings.Builder

	cards := lipgloss.JoinHorizontal(lipgloss.Top,
		statCard(fmt.Sprintf("models · %d text", s.TextModels), fmt.Sprintf("%d", s.TotalModels), cPrimary),
		statCard("authors", fmt.Sprintf("%d", s.TotalAuthors), cCyan),
		statCard("providers", fmt.Sprintf("%d", s.TotalProviders), cPink),
		statCard("free", fmt.Sprintf("%d", s.FreeModels), cAccent),
		statCard("tool use", fmt.Sprintf("%d", s.ToolModels), cYellow),
		statCard("reasoning", fmt.Sprintf("%d", s.ReasoningModels), cPrimary),
		statCard("vision", fmt.Sprintf("%d", s.VisionModels), cCyan),
		statCard("audio", fmt.Sprintf("%d", s.AudioModels), cPink),
	)
	b.WriteString(cards + "\n")

	b.WriteString(" " + styleSection.Render("PRICING (paid text models, prompt $/M tokens)") + "  " +
		styleDim.Render(fmt.Sprintf("median %s in / %s out · max %s",
			api.FmtPrice(s.MedianPromptPerM), api.FmtPrice(s.MedianCompletionPerM), api.FmtPrice(s.MaxPromptPerM))) + "\n\n")

	left := v.barChart("TOP AUTHORS", s.TopAuthors, cPrimary)
	right := v.barChart("PROMPT PRICE DISTRIBUTION", s.PriceBuckets, cAccent) + "\n" +
		v.barChart("CONTEXT LENGTH", s.CtxBuckets, cCyan)
	if v.width >= 110 {
		lw := v.width / 2
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top,
			lipgloss.NewStyle().Width(lw).Render(left),
			lipgloss.NewStyle().Width(v.width-lw).Render(right)) + "\n")
	} else {
		b.WriteString(left + "\n" + right + "\n")
	}

	b.WriteString(v.barChart("MOST SUPPORTED PARAMETERS", s.TopParams, cPink) + "\n")

	b.WriteString(" " + styleSection.Render("NEWEST MODELS") + "\n")
	for _, m := range s.Newest {
		price := m.InputPrice() + " / " + m.OutputPrice()
		b.WriteString(fmt.Sprintf(" %s  %s %s %s\n",
			styleDim.Render(m.CreatedTime().Format("2006-01-02")),
			lipgloss.NewStyle().Foreground(cCyan).Render(lipgloss.NewStyle().Width(44).Render(m.ID)),
			lipgloss.NewStyle().Foreground(cAccent).Width(18).Render(price),
			styleDim.Render(api.FmtCtx(m.ContextLength)+" ctx")))
	}
	return b.String()
}

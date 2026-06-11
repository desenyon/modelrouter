package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	lgtable "github.com/charmbracelet/lipgloss/table"

	"github.com/desenyon/modelrouter/internal/api"
)

type DetailView struct {
	model   api.Model
	eps     *api.ModelEndpoints
	epsErr  error
	bench   *api.ModelBench
	loading bool

	vp            viewport.Model
	width, height int
}

func NewDetailView(m api.Model, w, h int) *DetailView {
	d := &DetailView{model: m, loading: true}
	d.vp = viewport.New(w, h)
	d.SetSize(w, h)
	return d
}

func (d *DetailView) SetSize(w, h int) {
	d.width, d.height = w, h
	d.vp.Width = w
	d.vp.Height = h
	d.vp.SetContent(d.content())
}

func (d *DetailView) SetEndpoints(eps *api.ModelEndpoints, err error) {
	d.eps, d.epsErr = eps, err
	d.loading = false
	d.vp.SetContent(d.content())
}

func (d *DetailView) SetBench(b *api.ModelBench) {
	d.bench = b
	d.vp.SetContent(d.content())
}

func (d *DetailView) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "g", "home":
			d.vp.GotoTop()
			return nil
		case "G", "end":
			d.vp.GotoBottom()
			return nil
		}
	}
	var cmd tea.Cmd
	d.vp, cmd = d.vp.Update(msg)
	return cmd
}

func (d *DetailView) ScrollPercent() float64 { return d.vp.ScrollPercent() }

func (d *DetailView) View(spin spinner.Model) string {
	if d.loading {
		// re-render so the spinner animates while endpoints load
		d.vp.SetContent(d.contentWithSpinner(spin.View()))
	}
	return d.vp.View()
}

func (d *DetailView) content() string { return d.contentWithSpinner("") }

func (d *DetailView) contentWithSpinner(spin string) string {
	m := d.model
	var b strings.Builder
	wrap := lipgloss.NewStyle().Width(d.width - 4)

	title := lipgloss.NewStyle().Bold(true).Foreground(cText).Render(m.Name)
	b.WriteString(" " + title + "\n")
	meta := []string{
		lipgloss.NewStyle().Foreground(cCyan).Render(m.ID),
		"created " + m.CreatedTime().Format("2006-01-02"),
		api.FmtCtx(m.ContextLength) + " context",
		m.Architecture.Modality,
	}
	if m.KnowledgeCutoff != nil && *m.KnowledgeCutoff != "" {
		meta = append(meta, "cutoff "+*m.KnowledgeCutoff)
	}
	if m.HuggingFaceID != nil && *m.HuggingFaceID != "" {
		meta = append(meta, "hf:"+*m.HuggingFaceID)
	}
	if m.IsFree() {
		meta = append(meta, styleFree.Render("FREE"))
	}
	b.WriteString(styleDim.Render(" "+strings.Join(meta, "  ·  ")) + "\n\n")

	if m.Description != "" {
		b.WriteString(" " + styleSection.Render("DESCRIPTION") + "\n")
		b.WriteString(wrap.PaddingLeft(1).Foreground(cText).Render(m.Description) + "\n\n")
	}

	b.WriteString(" " + styleSection.Render("PRICING") + styleDim.Render("  ($ per million tokens)") + "\n")
	b.WriteString(d.pricingGrid(m.Pricing) + "\n\n")

	if d.bench != nil && !d.bench.Empty() {
		b.WriteString(d.benchSection() + "\n")
	}

	if len(m.SupportedParameters) > 0 {
		b.WriteString(" " + styleSection.Render("SUPPORTED PARAMETERS") + "\n ")
		params := append([]string(nil), m.SupportedParameters...)
		sort.Strings(params)
		var line string
		for _, p := range params {
			chip := styleBadge.Render(p) + " "
			if lipgloss.Width(line)+lipgloss.Width(chip) > d.width-2 {
				b.WriteString(line + "\n ")
				line = ""
			}
			line += chip
		}
		b.WriteString(line + "\n\n")
	}

	b.WriteString(" " + styleSection.Render("PROVIDER ENDPOINTS") + "\n")
	switch {
	case d.loading:
		b.WriteString(" " + spin + styleDim.Render(" loading endpoints...") + "\n")
	case d.epsErr != nil:
		b.WriteString(" " + styleErr.Render("failed: "+d.epsErr.Error()) + "\n")
	case d.eps == nil || len(d.eps.Endpoints) == 0:
		b.WriteString(styleDim.Render(" no active endpoints") + "\n")
	default:
		b.WriteString(d.endpointsTable() + "\n")
	}
	return b.String()
}

// benchSection renders Artificial Analysis scores and Design Arena results.
func (d *DetailView) benchSection() string {
	var b strings.Builder
	b.WriteString(" " + styleSection.Render("BENCHMARKS") +
		styleDim.Render("  (Artificial Analysis · Design Arena)") + "\n")

	if len(d.bench.AA) > 0 {
		var parts []string
		for _, cat := range api.AACategories {
			if score, ok := d.bench.AA[cat]; ok {
				parts = append(parts, " "+styleDim.Render(cat+" ")+
					lipgloss.NewStyle().Foreground(cYellow).Bold(true).Render(fmt.Sprintf("%.1f", score))+
					" "+bar(int(score*10), 1000, 12, cYellow))
			}
		}
		b.WriteString(strings.Join(parts, styleDim.Render("  │")) + "\n")
	}

	if len(d.bench.DA) > 0 {
		var parts []string
		for _, dc := range api.DACategories {
			if r, ok := d.bench.DA[dc.Label]; ok {
				parts = append(parts, " "+styleDim.Render(dc.Label+" ")+
					lipgloss.NewStyle().Foreground(cPink).Bold(true).Render(fmt.Sprintf("%.0f", r.Score))+
					styleDim.Render(fmt.Sprintf(" (%.0f%% win)", r.WinRate)))
			}
		}
		wrap := lipgloss.NewStyle().Width(d.width - 2)
		b.WriteString(wrap.Render(styleDim.Render(" design arena elo:")+strings.Join(parts, styleDim.Render("  ·"))) + "\n")
	}

	var costs []string
	if d.bench.CostPerRequest != nil {
		costs = append(costs, " "+styleDim.Render("est. cost/request ")+
			lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render(fmt.Sprintf("$%.4f", *d.bench.CostPerRequest)))
	}
	if d.bench.WeightedInputPrice != nil {
		costs = append(costs, " "+styleDim.Render("weighted input $/M ")+
			lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render(fmt.Sprintf("$%.3f", *d.bench.WeightedInputPrice)))
	}
	if len(costs) > 0 {
		b.WriteString(strings.Join(costs, styleDim.Render("  │")) + "\n")
	}
	return b.String()
}

func (d *DetailView) pricingGrid(p api.Pricing) string {
	type cell struct{ label, val string }
	cells := []cell{
		{"prompt", api.FmtPrice(p.PromptPerM())},
		{"completion", api.FmtPrice(p.CompletionPerM())},
	}
	if p.InputCacheRead != "" {
		cells = append(cells, cell{"cache read", api.FmtPrice(p.CacheReadPerM())})
	}
	if p.InputCacheWrite != "" {
		cells = append(cells, cell{"cache write", api.FmtPrice(p.CacheWritePerM())})
	}
	if p.InternalReasoning != "" && p.InternalReasoning != "0" {
		cells = append(cells, cell{"reasoning", api.FmtPrice(perM(p.InternalReasoning))})
	}
	if p.Image != "" && p.Image != "0" {
		cells = append(cells, cell{"image (each)", "$" + trimZeros(p.Image)})
	}
	if p.Audio != "" && p.Audio != "0" {
		cells = append(cells, cell{"audio", "$" + trimZeros(p.Audio)})
	}
	if p.WebSearch != "" && p.WebSearch != "0" {
		cells = append(cells, cell{"web search (each)", "$" + trimZeros(p.WebSearch)})
	}
	var parts []string
	for _, c := range cells {
		parts = append(parts, " "+styleDim.Render(c.label+" ")+
			lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render(c.val))
	}
	return strings.Join(parts, styleDim.Render("  │"))
}

func perM(s string) float64 {
	var v float64
	fmt.Sscanf(s, "%g", &v)
	return v * 1e6
}

func trimZeros(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func (d *DetailView) endpointsTable() string {
	eps := append([]api.Endpoint(nil), d.eps.Endpoints...)
	sort.SliceStable(eps, func(i, j int) bool {
		return eps[i].Pricing.PromptPerM() < eps[j].Pricing.PromptPerM()
	})

	t := lgtable.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(cFaint)).
		Headers("PROVIDER", "QUANT", "CTX", "MAX OUT", "IN $/M", "OUT $/M", "UPTIME 1D", "LATENCY", "TPS").
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == lgtable.HeaderRow {
				return lipgloss.NewStyle().Foreground(cPrimary).Bold(true).Padding(0, 1)
			}
			return lipgloss.NewStyle().Foreground(cText).Padding(0, 1)
		})

	for _, e := range eps {
		up := "-"
		if e.UptimeLast1d != nil {
			up = uptimeStyle(*e.UptimeLast1d).Render(fmt.Sprintf("%.2f%%", *e.UptimeLast1d))
		}
		lat := "-"
		if e.LatencyLast30m != nil {
			lat = (time.Duration(*e.LatencyLast30m * float64(time.Second))).Round(10 * time.Millisecond).String()
		}
		tps := "-"
		if e.ThroughputLast30m != nil {
			tps = fmt.Sprintf("%.0f", *e.ThroughputLast30m)
		}
		maxOut := "-"
		if e.MaxCompletionTokens != nil {
			maxOut = api.FmtCtx(*e.MaxCompletionTokens)
		}
		quant := e.Quantization
		if quant == "" || quant == "unknown" {
			quant = "-"
		}
		t.Row(e.ProviderName, quant, api.FmtCtx(e.ContextLength), maxOut,
			api.FmtPrice(e.Pricing.PromptPerM()), api.FmtPrice(e.Pricing.CompletionPerM()), up, lat, tps)
	}
	return t.Render()
}

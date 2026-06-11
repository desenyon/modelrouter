// Package cli renders static (non-interactive) output for scripting and piping.
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	lgtable "github.com/charmbracelet/lipgloss/table"

	"github.com/desenyon/modelrouter/internal/api"
)

var (
	cPrimary = lipgloss.Color("#7D56F4")
	cAccent  = lipgloss.Color("#04B575")
	cCyan    = lipgloss.Color("#41D6E0")
	cDim     = lipgloss.AdaptiveColor{Light: "#9A9A9A", Dark: "#626262"}
	cFaint   = lipgloss.AdaptiveColor{Light: "#B5B5B5", Dark: "#3A3A3A"}

	headerStyle = lipgloss.NewStyle().Foreground(cPrimary).Bold(true).Padding(0, 1)
	cellStyle   = lipgloss.NewStyle().Padding(0, 1)
	dimStyle    = lipgloss.NewStyle().Foreground(cDim)
	titleStyle  = lipgloss.NewStyle().Bold(true)
)

func JSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newTable(headers ...string) *lgtable.Table {
	return lgtable.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(cFaint)).
		Headers(headers...).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == lgtable.HeaderRow {
				return headerStyle
			}
			return cellStyle
		})
}

func PrintModels(models []api.Model) {
	t := newTable("MODEL", "CTX", "IN $/M", "OUT $/M", "MODS", "CAPS", "CREATED")
	for _, m := range models {
		t.Row(m.ID, api.FmtCtx(m.ContextLength),
			api.FmtPrice(m.Pricing.PromptPerM()), api.FmtPrice(m.Pricing.CompletionPerM()),
			api.FmtModalities(m.Architecture), api.FmtCaps(m),
			m.CreatedTime().Format("2006-01-02"))
	}
	fmt.Println(t.Render())
	fmt.Println(dimStyle.Render(fmt.Sprintf(" %d models · caps: T=tools R=reasoning S=structured", len(models))))
}

func PrintProviders(providers []api.Provider) {
	t := newTable("PROVIDER", "SLUG", "HQ", "DATACENTERS")
	for _, p := range providers {
		dc := strings.Join(p.Datacenters, ", ")
		if dc == "" {
			dc = "-"
		}
		hq := p.Headquarters
		if hq == "" {
			hq = "-"
		}
		t.Row(p.Name, p.Slug, hq, dc)
	}
	fmt.Println(t.Render())
}

func PrintModelDetail(m api.Model, eps *api.ModelEndpoints, epsErr error) {
	fmt.Println()
	fmt.Println(" " + titleStyle.Render(m.Name))
	meta := []string{m.ID, "created " + m.CreatedTime().Format("2006-01-02"),
		api.FmtCtx(m.ContextLength) + " context", m.Architecture.Modality}
	if m.KnowledgeCutoff != nil && *m.KnowledgeCutoff != "" {
		meta = append(meta, "cutoff "+*m.KnowledgeCutoff)
	}
	fmt.Println(dimStyle.Render(" " + strings.Join(meta, "  ·  ")))
	fmt.Println()
	if m.Description != "" {
		fmt.Println(lipgloss.NewStyle().Width(96).PaddingLeft(1).Render(m.Description))
		fmt.Println()
	}
	priceStyle := lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	fmt.Printf(" %s %s in · %s out",
		titleStyle.Foreground(cPrimary).Render("pricing:"),
		priceStyle.Render(api.FmtPrice(m.Pricing.PromptPerM())),
		priceStyle.Render(api.FmtPrice(m.Pricing.CompletionPerM())))
	if m.Pricing.InputCacheRead != "" {
		fmt.Printf(" · %s cache-read", priceStyle.Render(api.FmtPrice(m.Pricing.CacheReadPerM())))
	}
	fmt.Println(dimStyle.Render("  ($/M tokens)"))

	params := append([]string(nil), m.SupportedParameters...)
	sort.Strings(params)
	fmt.Println(" " + titleStyle.Foreground(cPrimary).Render("params: ") + dimStyle.Render(strings.Join(params, ", ")))
	fmt.Println()

	switch {
	case epsErr != nil:
		fmt.Println(dimStyle.Render(" endpoints unavailable: " + epsErr.Error()))
	case eps == nil || len(eps.Endpoints) == 0:
		fmt.Println(dimStyle.Render(" no active endpoints"))
	default:
		t := newTable("PROVIDER", "QUANT", "CTX", "MAX OUT", "IN $/M", "OUT $/M", "UPTIME 1D", "LATENCY", "TPS")
		for _, e := range eps.Endpoints {
			up, lat, tps, maxOut := "-", "-", "-", "-"
			if e.UptimeLast1d != nil {
				up = fmt.Sprintf("%.2f%%", *e.UptimeLast1d)
			}
			if e.LatencyLast30m != nil {
				lat = (time.Duration(*e.LatencyLast30m * float64(time.Second))).Round(10 * time.Millisecond).String()
			}
			if e.ThroughputLast30m != nil {
				tps = fmt.Sprintf("%.0f", *e.ThroughputLast30m)
			}
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
		fmt.Println(t.Render())
	}
}

func PrintStats(s api.Stats) {
	fmt.Println()
	fmt.Printf(" %s  %d models · %d authors · %d providers · %d free · %d tool-use · %d reasoning · %d vision\n",
		titleStyle.Foreground(cPrimary).Render("openrouter catalog:"),
		s.TotalModels, s.TotalAuthors, s.TotalProviders, s.FreeModels, s.ToolModels, s.ReasoningModels, s.VisionModels)
	fmt.Printf(" %s  median %s in / %s out · max %s in\n\n",
		titleStyle.Foreground(cPrimary).Render("paid pricing ($/M):"),
		api.FmtPrice(s.MedianPromptPerM), api.FmtPrice(s.MedianCompletionPerM), api.FmtPrice(s.MaxPromptPerM))

	printBars("top authors", s.TopAuthors, cPrimary)
	printBars("prompt price distribution (paid)", s.PriceBuckets, cAccent)
	printBars("context length", s.CtxBuckets, cCyan)

	fmt.Println(" " + titleStyle.Foreground(cPrimary).Render("newest models"))
	for _, m := range s.Newest {
		fmt.Printf("   %s  %-44s %s / %s\n", m.CreatedTime().Format("2006-01-02"), m.ID,
			api.FmtPrice(m.Pricing.PromptPerM()), api.FmtPrice(m.Pricing.CompletionPerM()))
	}
}

func printBars(title string, items []api.CountItem, color lipgloss.Color) {
	fmt.Println(" " + titleStyle.Foreground(cPrimary).Render(title))
	maxCount, maxLabel := 1, 0
	for _, it := range items {
		if it.Count > maxCount {
			maxCount = it.Count
		}
		if len(it.Label) > maxLabel {
			maxLabel = len(it.Label)
		}
	}
	for _, it := range items {
		filled := it.Count * 40 / maxCount
		if it.Count > 0 && filled == 0 {
			filled = 1
		}
		fmt.Printf("   %-*s %s%s %d\n", maxLabel, it.Label,
			lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", filled)),
			lipgloss.NewStyle().Foreground(cFaint).Render(strings.Repeat("░", 40-filled)),
			it.Count)
	}
	fmt.Println()
}

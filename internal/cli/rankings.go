package cli

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/desenyon/modelrouter/internal/api"
)

func PrintTopModels(r *api.Rankings, limit int) {
	top := r.TopModels()
	if limit > 0 && len(top) > limit {
		top = top[:limit]
	}
	fmt.Println()
	fmt.Println(" " + titleStyle.Foreground(cPrimary).Render("top models by tokens") +
		dimStyle.Render("  ("+r.LatestDate()+", prompt+completion)"))
	var maxTok int64 = 1
	if len(top) > 0 {
		maxTok = top[0].TotalTokens
	}
	for i, u := range top {
		filled := int(u.TotalTokens * 32 / maxTok)
		if filled == 0 {
			filled = 1
		}
		change := ""
		if u.Change != nil {
			if *u.Change >= 0 {
				change = lipgloss.NewStyle().Foreground(cAccent).Render(fmt.Sprintf("  ▲%.0f%%", *u.Change*100))
			} else {
				change = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F87")).Render(fmt.Sprintf("  ▼%.0f%%", -*u.Change*100))
			}
		}
		fmt.Printf(" %2d  %-46s %s%s %7s %10s req%s\n", i+1, u.Slug,
			lipgloss.NewStyle().Foreground(cPrimary).Render(strings.Repeat("█", filled)),
			lipgloss.NewStyle().Foreground(cFaint).Render(strings.Repeat("░", 32-filled)),
			api.FmtTokens(u.TotalTokens), api.FmtTokens(u.Requests), change)
	}
}

func PrintApps(r *api.Rankings, period string) {
	apps := map[string][]api.AppRank{"day": r.Apps.Day, "week": r.Apps.Week, "month": r.Apps.Month}[period]
	fmt.Println()
	fmt.Println(" " + titleStyle.Foreground(cPrimary).Render("top apps") + dimStyle.Render("  (per "+period+")"))
	t := newTable("#", "APP", "TOKENS", "REQUESTS", "URL")
	for _, a := range apps {
		t.Row(fmt.Sprintf("%d", a.Rank), a.App.Title,
			api.FmtTokens(a.Tokens()), api.FmtTokens(a.TotalRequests), a.App.MainURL)
	}
	fmt.Println(t.Render())
}

func PrintMarketShare(r *api.Rankings) {
	week, share := r.LatestShare()
	fmt.Println()
	fmt.Println(" " + titleStyle.Foreground(cPrimary).Render("market share by author") + dimStyle.Render("  (week of "+week+")"))
	var total, maxShare int64
	for _, it := range share {
		total += it.Count
		if it.Count > maxShare {
			maxShare = it.Count
		}
	}
	if maxShare == 0 {
		maxShare = 1
	}
	for _, it := range share {
		filled := int(it.Count * 36 / maxShare)
		if filled == 0 {
			filled = 1
		}
		fmt.Printf("   %-12s %s%s %7s %5.1f%%\n", it.Label,
			lipgloss.NewStyle().Foreground(cAccent).Render(strings.Repeat("█", filled)),
			lipgloss.NewStyle().Foreground(cFaint).Render(strings.Repeat("░", 36-filled)),
			api.FmtTokens(it.Count), float64(it.Count)/float64(total)*100)
	}
}

func PrintPerformance(r *api.Rankings, limit int) {
	perf := r.Performance
	if limit > 0 && len(perf) > limit {
		perf = perf[:limit]
	}
	fmt.Println()
	fmt.Println(" " + titleStyle.Foreground(cPrimary).Render("performance leaders") + dimStyle.Render("  (p50, busiest models)"))
	t := newTable("MODEL", "REQUESTS", "P50 LATENCY", "P50 TPS", "FASTEST VIA", "LOWEST-LATENCY VIA")
	for _, p := range perf {
		t.Row(p.ID, api.FmtTokens(p.RequestCount),
			fmt.Sprintf("%.0fms", p.P50Latency), fmt.Sprintf("%.0f", p.P50Throughput),
			p.BestThroughputProvider, p.BestLatencyProvider)
	}
	fmt.Println(t.Render())
}

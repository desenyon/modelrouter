package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/desenyon/modelrouter/internal/api"
)

func PrintBenchmarks(b *api.Benchmarks) {
	if !b.FetchedAt.IsZero() {
		fmt.Println(dimStyle.Render(" live OpenRouter benchmark feed · updated " + b.FetchedAt.Format("2006-01-02 15:04:05")))
	}
	for _, cat := range b.AllAACategories() {
		rows := b.TopAA(cat)
		if len(rows) == 0 {
			continue
		}
		fmt.Println()
		fmt.Println(" " + titleStyle.Foreground(cPrimary).Render("artificial analysis · "+cat))
		maxScore := rows[0].Score
		for i, r := range rows {
			filled := int(r.Score * 30 / maxScore)
			if filled == 0 {
				filled = 1
			}
			fmt.Printf(" %2d  %-44s %s%s %5.1f\n", i+1, r.Slug(),
				lipgloss.NewStyle().Foreground(cPrimary).Render(strings.Repeat("█", filled)),
				lipgloss.NewStyle().Foreground(cFaint).Render(strings.Repeat("░", 30-filled)),
				r.Score)
		}
	}

	fmt.Println()
	fmt.Println(" " + titleStyle.Foreground(cPrimary).Render("design arena") + dimStyle.Render("  (elo · win rate, top 5 per category)"))
	for _, dc := range b.DACategories() {
		rows := b.TopDA(dc.Key)
		if len(rows) == 0 {
			continue
		}
		if len(rows) > 5 {
			rows = rows[:5]
		}
		var parts []string
		for _, r := range rows {
			parts = append(parts, fmt.Sprintf("%s %.0f (%.0f%%)", r.DisplayName, r.Score, r.WinRate))
		}
		fmt.Printf("   %-28s %s\n", dc.Label, dimStyle.Render(strings.Join(parts, " · ")))
	}

	cheap := b.CheapestRequests()
	if len(cheap) > 0 {
		fmt.Println()
		fmt.Println(" " + titleStyle.Foreground(cPrimary).Render("estimated cost per request") + dimStyle.Render("  (cheapest first)"))
		limit := 15
		if len(cheap) < limit {
			limit = len(cheap)
		}
		for i, c := range cheap[:limit] {
			fmt.Printf(" %2d  %-44s $%.4f\n", i+1, c.Slug, c.Cost)
		}
	}
}

// PrintModelBench renders the benchmark lines inside `model <id>` output.
func PrintModelBench(mb *api.ModelBench) {
	if mb == nil || mb.Empty() {
		return
	}
	var parts []string
	for _, cat := range mb.AACategories() {
		if s, ok := mb.AA[cat]; ok {
			parts = append(parts, fmt.Sprintf("%s %.1f", cat, s))
		}
	}
	if len(parts) > 0 {
		fmt.Println(" " + titleStyle.Foreground(cPrimary).Render("benchmarks: ") + dimStyle.Render("AA ") + strings.Join(parts, " · "))
	}
	var da []string
	keys := make([]string, 0, len(mb.DA))
	for label := range mb.DA {
		keys = append(keys, label)
	}
	sort.Strings(keys)
	for _, label := range keys {
		r := mb.DA[label]
		da = append(da, fmt.Sprintf("%s %.0f (%.0f%%)", label, r.Score, r.WinRate))
	}
	if len(da) > 0 {
		fmt.Println("             " + dimStyle.Render("arena ") + strings.Join(da, " · "))
	}
	if mb.CostPerRequest != nil {
		fmt.Printf("             %s$%.4f\n", dimStyle.Render("est. cost/request "), *mb.CostPerRequest)
	}
	fmt.Println()
}

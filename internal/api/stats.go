package api

import "sort"

type CountItem struct {
	Label string
	Count int
}

type Stats struct {
	TotalModels    int
	TotalAuthors   int
	TotalProviders int
	FreeModels     int
	ToolModels     int
	ReasoningModels int
	VisionModels   int
	AudioModels    int
	MedianPromptPerM     float64
	MedianCompletionPerM float64
	MaxPromptPerM        float64
	TopAuthors    []CountItem
	PriceBuckets  []CountItem // prompt $/M distribution (paid models)
	CtxBuckets    []CountItem
	TopParams     []CountItem
	Newest        []Model
}

func ComputeStats(models []Model, providers []Provider) Stats {
	s := Stats{TotalModels: len(models), TotalProviders: len(providers)}

	authors := map[string]int{}
	params := map[string]int{}
	var promptPrices, completionPrices []float64

	priceBuckets := []struct {
		label string
		max   float64
	}{
		{"< $0.10", 0.1}, {"< $0.50", 0.5}, {"< $1", 1}, {"< $3", 3},
		{"< $10", 10}, {"< $30", 30}, {"$30+", 1e18},
	}
	priceCounts := make([]int, len(priceBuckets))

	ctxBuckets := []struct {
		label string
		max   int
	}{
		{"≤ 8K", 8_192}, {"≤ 33K", 33_000}, {"≤ 131K", 131_072},
		{"≤ 262K", 262_144}, {"≤ 1M", 1_000_000}, {"> 1M", 1 << 62},
	}
	ctxCounts := make([]int, len(ctxBuckets))

	for _, m := range models {
		authors[m.Author()]++
		for _, p := range m.SupportedParameters {
			params[p]++
		}
		if m.IsFree() {
			s.FreeModels++
		}
		if m.HasParam("tools") {
			s.ToolModels++
		}
		if m.HasParam("reasoning") || m.HasParam("include_reasoning") {
			s.ReasoningModels++
		}
		if m.HasInput("image") {
			s.VisionModels++
		}
		if m.HasInput("audio") {
			s.AudioModels++
		}
		pp := m.Pricing.PromptPerM()
		if !m.IsFree() {
			promptPrices = append(promptPrices, pp)
			completionPrices = append(completionPrices, m.Pricing.CompletionPerM())
			for i, b := range priceBuckets {
				if pp < b.max {
					priceCounts[i]++
					break
				}
			}
			if pp > s.MaxPromptPerM {
				s.MaxPromptPerM = pp
			}
		}
		for i, b := range ctxBuckets {
			if m.ContextLength <= b.max {
				ctxCounts[i]++
				break
			}
		}
	}

	s.TotalAuthors = len(authors)
	s.MedianPromptPerM = median(promptPrices)
	s.MedianCompletionPerM = median(completionPrices)

	for a, n := range authors {
		s.TopAuthors = append(s.TopAuthors, CountItem{a, n})
	}
	sortCounts(s.TopAuthors)
	if len(s.TopAuthors) > 12 {
		s.TopAuthors = s.TopAuthors[:12]
	}

	for p, n := range params {
		s.TopParams = append(s.TopParams, CountItem{p, n})
	}
	sortCounts(s.TopParams)
	if len(s.TopParams) > 10 {
		s.TopParams = s.TopParams[:10]
	}

	for i, b := range priceBuckets {
		s.PriceBuckets = append(s.PriceBuckets, CountItem{b.label, priceCounts[i]})
	}
	for i, b := range ctxBuckets {
		s.CtxBuckets = append(s.CtxBuckets, CountItem{b.label, ctxCounts[i]})
	}

	newest := make([]Model, len(models))
	copy(newest, models)
	SortModels(newest, SortNewest, false)
	if len(newest) > 8 {
		newest = newest[:8]
	}
	s.Newest = newest
	return s
}

func sortCounts(items []CountItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		return items[i].Label < items[j].Label
	})
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	mid := len(v) / 2
	if len(v)%2 == 0 {
		return (v[mid-1] + v[mid]) / 2
	}
	return v[mid]
}

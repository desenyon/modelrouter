package api

import "sort"

// Benchmarks mirrors /api/frontend/rankings/benchmarks: Artificial Analysis
// scores, Design Arena results, and OpenRouter's blended cost estimates.
type Benchmarks struct {
	AA                  map[string][]AAScore `json:"aaData"` // intelligence, coding, agentic
	DA                  map[string][]DARow   `json:"daData"` // models-website, models-svg, ...
	WeightedInputPrices map[string]float64   `json:"weightedInputPrices"`
	CostPerRequest      map[string]float64   `json:"costPerRequest"`
}

type AAScore struct {
	UID            string  `json:"uid"`
	Permaslug      string  `json:"permaslug"`
	OpenrouterSlug *string `json:"openrouter_slug"`
	HeuristicSlug  *string `json:"heuristic_openrouter_slug"`
	Name           string  `json:"aa_name"`
	Score          float64 `json:"score"`
}

// Slug returns the best display/matching id for an AA row.
func (s AAScore) Slug() string {
	if s.OpenrouterSlug != nil && *s.OpenrouterSlug != "" {
		return *s.OpenrouterSlug
	}
	if s.HeuristicSlug != nil && *s.HeuristicSlug != "" {
		return *s.HeuristicSlug
	}
	return s.Permaslug
}

type DARow struct {
	DAModelID    string  `json:"da_model_id"`
	Permaslug    string  `json:"permaslug"`
	OpenrouterID string  `json:"openrouter_id"`
	DisplayName  string  `json:"display_name"`
	Score        float64 `json:"score"` // elo
	WinRate      float64 `json:"win_rate"`
	AvgGenTimeMs float64 `json:"avg_generation_time_ms"`
}

// DACategories maps daData keys to friendly labels, in display order.
var DACategories = []struct{ Key, Label string }{
	{"models-website", "website"},
	{"models-uicomponent", "ui component"},
	{"models-dataviz", "data viz"},
	{"models-gamedev", "game dev"},
	{"models-3d", "3d"},
	{"models-svg", "svg"},
	{"models-codecategories", "code"},
	{"models-asciiart", "ascii art"},
}

var AACategories = []string{"intelligence", "coding", "agentic"}

func (c *Client) Benchmarks(force bool) (*Benchmarks, error) {
	b, err := frontendGet[Benchmarks](c, "rank-bench", "/rankings/benchmarks", force)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ModelBench is everything the benchmark data knows about one model.
type ModelBench struct {
	AA                 map[string]float64 // category -> score (0-100)
	DA                 map[string]DARow   // friendly category label -> result
	CostPerRequest     *float64
	WeightedInputPrice *float64
}

func (mb *ModelBench) Empty() bool {
	return len(mb.AA) == 0 && len(mb.DA) == 0 && mb.CostPerRequest == nil && mb.WeightedInputPrice == nil
}

// ForModel matches a catalog model against benchmark rows by id and canonical slug.
func (b *Benchmarks) ForModel(m Model) *ModelBench {
	mb := &ModelBench{AA: map[string]float64{}, DA: map[string]DARow{}}
	match := func(keys ...string) bool {
		for _, k := range keys {
			if k != "" && (k == m.ID || k == m.CanonicalSlug) {
				return true
			}
		}
		return false
	}
	for cat, rows := range b.AA {
		for _, r := range rows {
			slug := ""
			if r.OpenrouterSlug != nil {
				slug = *r.OpenrouterSlug
			}
			heur := ""
			if r.HeuristicSlug != nil {
				heur = *r.HeuristicSlug
			}
			if match(r.Permaslug, slug, heur, r.UID) {
				mb.AA[cat] = r.Score
				break
			}
		}
	}
	for _, dc := range DACategories {
		for _, r := range b.DA[dc.Key] {
			if match(r.Permaslug, r.OpenrouterID) {
				mb.DA[dc.Label] = r
				break
			}
		}
	}
	for slug, v := range b.CostPerRequest {
		if match(slug) {
			cost := v
			mb.CostPerRequest = &cost
			break
		}
	}
	for slug, v := range b.WeightedInputPrices {
		if match(slug) {
			price := v
			mb.WeightedInputPrice = &price
			break
		}
	}
	return mb
}

// TopAA returns a category's rows sorted by score descending.
func (b *Benchmarks) TopAA(category string) []AAScore {
	rows := append([]AAScore(nil), b.AA[category]...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Score > rows[j].Score })
	return rows
}

// TopDA returns a category's rows sorted by elo descending.
func (b *Benchmarks) TopDA(key string) []DARow {
	rows := append([]DARow(nil), b.DA[key]...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Score > rows[j].Score })
	return rows
}

// CheapestRequests returns (slug, cost) pairs sorted by estimated request cost.
func (b *Benchmarks) CheapestRequests() []struct {
	Slug string
	Cost float64
} {
	out := make([]struct {
		Slug string
		Cost float64
	}, 0, len(b.CostPerRequest))
	for slug, c := range b.CostPerRequest {
		out = append(out, struct {
			Slug string
			Cost float64
		}{slug, c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cost < out[j].Cost })
	return out
}

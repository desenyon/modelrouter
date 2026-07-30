package api

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Benchmarks mirrors /api/frontend/rankings/benchmarks: Artificial Analysis
// scores, Design Arena results, and OpenRouter's blended cost estimates.
type Benchmarks struct {
	AA                  map[string][]AAScore `json:"aaData"` // intelligence, coding, agentic
	DA                  map[string][]DARow   `json:"daData"` // models-website, models-svg, ...
	WeightedInputPrices map[string]float64   `json:"weightedInputPrices"`
	CostPerRequest      map[string]float64   `json:"costPerRequest"`
	FetchedAt           time.Time            `json:"fetched_at"`
}

// UnmarshalJSON tolerates metadata values such as aaData.percentilesBySlug that
// are objects rather than leaderboard row arrays.
func (b *Benchmarks) UnmarshalJSON(data []byte) error {
	var raw struct {
		AA                  map[string]json.RawMessage `json:"aaData"`
		DA                  map[string][]DARow         `json:"daData"`
		WeightedInputPrices map[string]float64         `json:"weightedInputPrices"`
		CostPerRequest      map[string]float64         `json:"costPerRequest"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	b.AA = make(map[string][]AAScore, len(raw.AA))
	for category, rows := range raw.AA {
		trimmed := strings.TrimSpace(string(rows))
		if !strings.HasPrefix(trimmed, "[") {
			continue
		}
		var scores []AAScore
		if err := json.Unmarshal(rows, &scores); err != nil {
			return err
		}
		b.AA[category] = scores
	}
	b.DA = raw.DA
	b.WeightedInputPrices = raw.WeightedInputPrices
	b.CostPerRequest = raw.CostPerRequest
	return nil
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

var AACategories = []string{"intelligence", "coding", "agentic"}

// AllAACategories keeps the familiar indexes first and appends any new
// array-valued index OpenRouter adds to aaData.
func (b *Benchmarks) AllAACategories() []string {
	categories := make([]string, 0, len(b.AA))
	for category := range b.AA {
		categories = append(categories, category)
	}
	return orderAACategories(categories)
}

func orderAACategories(categories []string) []string {
	available := make(map[string]bool, len(categories))
	for _, category := range categories {
		available[category] = true
	}
	seen := make(map[string]bool, len(categories))
	out := make([]string, 0, len(categories))
	for _, category := range AACategories {
		if available[category] {
			out = append(out, category)
			seen[category] = true
		}
	}
	var extra []string
	for _, category := range categories {
		if !seen[category] {
			extra = append(extra, category)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

func (c *Client) Benchmarks(force bool) (*Benchmarks, error) {
	b, err := frontendGet[Benchmarks](c, "rank-bench", "/rankings/benchmarks", force)
	if err != nil {
		return nil, err
	}
	if len(b.AA) == 0 || len(b.DA) == 0 {
		return nil, UnexpectedShapeError{
			Endpoint: "/rankings/benchmarks",
			Detail:   "expected non-empty Artificial Analysis and Design Arena leaderboards",
		}
	}
	b.FetchedAt = c.cacheTime("rank-bench")
	return &b, nil
}

// ModelBenchmarks is the benchmark metadata embedded in the official model
// catalog. The rankings feed remains the richer source for full leaderboards.
type ModelBenchmarks struct {
	ArtificialAnalysis *ArtificialAnalysisScores `json:"artificial_analysis,omitempty"`
	DesignArena        []DesignArenaScore        `json:"design_arena,omitempty"`
}

type ArtificialAnalysisScores struct {
	Intelligence float64 `json:"intelligence_index"`
	Coding       float64 `json:"coding_index"`
	Agentic      float64 `json:"agentic_index"`
}

type DesignArenaScore struct {
	Arena    string  `json:"arena"`
	Category string  `json:"category"`
	Elo      float64 `json:"elo"`
	WinRate  float64 `json:"win_rate"`
	Rank     int     `json:"rank"`
}

type DACategory struct {
	Key      string
	Arena    string
	Category string
	Label    string
}

// DACategories returns every arena/category published by OpenRouter. Categories
// are grouped by arena and sorted so new upstream benchmarks appear
// automatically without a release.
func (b *Benchmarks) DACategories() []DACategory {
	out := make([]DACategory, 0, len(b.DA))
	for key := range b.DA {
		arena, category, ok := strings.Cut(key, "-")
		if !ok {
			arena, category = "other", key
		}
		out = append(out, DACategory{
			Key:      key,
			Arena:    arena,
			Category: category,
			Label:    friendlyCategory(arena) + " · " + friendlyCategory(category),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Arena != out[j].Arena {
			return arenaOrder(out[i].Arena) < arenaOrder(out[j].Arena)
		}
		return out[i].Category < out[j].Category
	})
	return out
}

func arenaOrder(arena string) string {
	switch arena {
	case "models":
		return "0"
	case "agents":
		return "1"
	default:
		return "2" + arena
	}
}

func friendlyCategory(s string) string {
	replacer := strings.NewReplacer(
		"uicomponent", "ui component",
		"dataviz", "data viz",
		"gamedev", "game dev",
		"codecategories", "code",
		"asciiart", "ascii art",
		"graphicdesign", "graphic design",
		"imageediting", "image editing",
		"text-to-speech", "text to speech",
		"agentic", "",
	)
	return strings.TrimSpace(replacer.Replace(s))
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

func (mb *ModelBench) AACategories() []string {
	categories := make([]string, 0, len(mb.AA))
	for category := range mb.AA {
		categories = append(categories, category)
	}
	return orderAACategories(categories)
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
	for _, dc := range b.DACategories() {
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

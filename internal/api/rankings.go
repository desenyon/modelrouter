package api

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The rankings endpoints are OpenRouter's unofficial frontend API — the same
// one the openrouter.ai rankings page calls. Shapes may change without notice.
const frontendBaseURL = "https://openrouter.ai/api/frontend"

// RankingsTTL is shorter than the catalog TTL: leaderboards move all day.
const RankingsTTL = 5 * time.Minute

// ModelDayStat is one model-variant's usage for one day.
type ModelDayStat struct {
	Date              string   `json:"date"`
	ModelPermaslug    string   `json:"model_permaslug"`
	Variant           string   `json:"variant"`
	VariantPermaslug  string   `json:"variant_permaslug"`
	CompletionTokens  int64    `json:"total_completion_tokens"`
	PromptTokens      int64    `json:"total_prompt_tokens"`
	ReasoningTokens   int64    `json:"total_native_tokens_reasoning"`
	CachedTokens      int64    `json:"total_native_tokens_cached"`
	Requests          int64    `json:"count"`
	ToolCalls         int64    `json:"total_tool_calls"`
	ToolCallErrors    int64    `json:"requests_with_tool_call_errors"`
	Change            *float64 `json:"change"`
}

func (s ModelDayStat) TotalTokens() int64 { return s.PromptTokens + s.CompletionTokens }

// Slug returns the display id including a variant suffix like ":free".
func (s ModelDayStat) Slug() string {
	if s.Variant != "" && s.Variant != "standard" {
		return s.ModelPermaslug + ":" + s.Variant
	}
	return s.ModelPermaslug
}

type AppInfo struct {
	ID            int64    `json:"id"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	MainURL       string   `json:"main_url"`
	OriginURL     string   `json:"origin_url"`
	SourceCodeURL string   `json:"source_code_url"`
	Categories    []string `json:"categories"`
}

type AppRank struct {
	Rank          int     `json:"rank"`
	TotalRequests int64   `json:"total_requests"`
	TotalTokens   string  `json:"total_tokens"` // numeric string in the API
	App           AppInfo `json:"app"`
}

func (a AppRank) Tokens() int64 {
	v, _ := strconv.ParseInt(a.TotalTokens, 10, 64)
	return v
}

type AppRankings struct {
	Day   []AppRank `json:"day"`
	Week  []AppRank `json:"week"`
	Month []AppRank `json:"month"`
}

// SharePoint is one week of per-author token totals.
type SharePoint struct {
	X  string           `json:"x"` // week start date
	Ys map[string]int64 `json:"ys"`
}

type PerfRow struct {
	ID                     string  `json:"id"`
	Slug                   string  `json:"slug"`
	Name                   string  `json:"name"`
	Author                 string  `json:"author"`
	RequestCount           int64   `json:"request_count"`
	P50Latency             float64 `json:"p50_latency"` // ms
	P50Throughput          float64 `json:"p50_throughput"`
	BestLatencyProvider    string  `json:"best_latency_provider"`
	BestLatencyPrice       float64 `json:"best_latency_price"`
	BestThroughputProvider string  `json:"best_throughput_provider"`
	BestThroughputPrice    float64 `json:"best_throughput_price"`
	ProviderCount          int     `json:"provider_count"`
}

// Rankings bundles everything the public leaderboard shows.
type Rankings struct {
	ModelDays   []ModelDayStat
	Apps        AppRankings
	MarketShare []SharePoint
	Performance []PerfRow
	FetchedAt   time.Time
}

func frontendGet[T any](c *Client, name, path string, force bool) (T, error) {
	var zero T
	b, err := c.cachedFetchURL(name, frontendBaseURL+path, force, RankingsTTL)
	if err != nil {
		return zero, err
	}
	var env struct {
		Data T `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return zero, fmt.Errorf("%s: %w", path, err)
	}
	return env.Data, nil
}

func (c *Client) Rankings(force bool) (*Rankings, error) {
	modelDays, err := frontendGet[[]ModelDayStat](c, "rank-models", "/rankings/models", force)
	if err != nil {
		return nil, err
	}
	apps, err := frontendGet[AppRankings](c, "rank-apps", "/rankings/apps", force)
	if err != nil {
		return nil, err
	}
	share, err := frontendGet[[]SharePoint](c, "rank-share", "/rankings/market-share", force)
	if err != nil {
		return nil, err
	}
	perf, err := frontendGet[[]PerfRow](c, "rank-perf", "/rankings/performance", force)
	if err != nil {
		return nil, err
	}
	return &Rankings{ModelDays: modelDays, Apps: apps, MarketShare: share,
		Performance: perf, FetchedAt: time.Now()}, nil
}

// TopModels aggregates the latest day's per-variant stats into a leaderboard.
type ModelUsage struct {
	Slug             string   `json:"slug"`
	TotalTokens      int64    `json:"total_tokens"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	ReasoningTokens  int64    `json:"reasoning_tokens"`
	Requests         int64    `json:"requests"`
	ToolCalls        int64    `json:"tool_calls"`
	Change           *float64 `json:"change"`
}

func (r *Rankings) LatestDate() string {
	latest := ""
	for _, s := range r.ModelDays {
		if s.Date > latest {
			latest = s.Date
		}
	}
	return strings.SplitN(latest, " ", 2)[0]
}

func (r *Rankings) TopModels() []ModelUsage {
	latest := ""
	for _, s := range r.ModelDays {
		if s.Date > latest {
			latest = s.Date
		}
	}
	byID := map[string]*ModelUsage{}
	for _, s := range r.ModelDays {
		if s.Date != latest {
			continue
		}
		u, ok := byID[s.Slug()]
		if !ok {
			u = &ModelUsage{Slug: s.Slug()}
			byID[s.Slug()] = u
		}
		u.TotalTokens += s.TotalTokens()
		u.PromptTokens += s.PromptTokens
		u.CompletionTokens += s.CompletionTokens
		u.ReasoningTokens += s.ReasoningTokens
		u.Requests += s.Requests
		u.ToolCalls += s.ToolCalls
		if s.Change != nil {
			u.Change = s.Change
		}
	}
	out := make([]ModelUsage, 0, len(byID))
	for _, u := range byID {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalTokens > out[j].TotalTokens })
	return out
}

// LatestShare returns the most recent week's author shares, sorted descending.
func (r *Rankings) LatestShare() (string, []CountItem64) {
	if len(r.MarketShare) == 0 {
		return "", nil
	}
	last := r.MarketShare[len(r.MarketShare)-1]
	items := make([]CountItem64, 0, len(last.Ys))
	for k, v := range last.Ys {
		items = append(items, CountItem64{Label: k, Count: v})
	}
	sort.Slice(items, func(i, j int) bool {
		// keep "others" at the bottom regardless of size
		if items[i].Label == "others" {
			return false
		}
		if items[j].Label == "others" {
			return true
		}
		return items[i].Count > items[j].Count
	})
	return last.X, items
}

type CountItem64 struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// FmtTokens renders large token counts: 1.2K, 3.4M, 5.6B, 7.8T.
func FmtTokens(n int64) string {
	f := float64(n)
	switch {
	case f >= 1e12:
		return fmt.Sprintf("%.2fT", f/1e12)
	case f >= 1e9:
		return fmt.Sprintf("%.1fB", f/1e9)
	case f >= 1e6:
		return fmt.Sprintf("%.1fM", f/1e6)
	case f >= 1e3:
		return fmt.Sprintf("%.1fK", f/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

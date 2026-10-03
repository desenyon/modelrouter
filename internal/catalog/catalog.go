// Package catalog describes every routable model: provider, upstream id,
// tier band, limits, prices, capabilities, and per-axis ability priors.
package catalog

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Tier is a cross-provider capability band. The vocabulary follows OpenAI's
// own model naming (gpt-6-luna < gpt-5.6-terra < gpt-6.1-sol < gpt-6-astra);
// models from other providers are slotted into the band they compete in.
type Tier string

const (
	Luna  Tier = "luna"  // efficient: high-volume, simple work
	Terra Tier = "terra" // balanced: everyday coding / analysis
	Sol   Tier = "sol"   // frontier-class reasoning at moderate cost
	Astra Tier = "astra" // most capable, most expensive
)

// Tiers in ascending order.
var Tiers = []Tier{Luna, Terra, Sol, Astra}

// Rank returns the ordinal of a tier (luna=0 … astra=3, unknown=-1).
func (t Tier) Rank() int {
	for i, x := range Tiers {
		if x == t {
			return i
		}
	}
	return -1
}

// ParseTier recognizes tier names.
func ParseTier(s string) (Tier, bool) {
	t := Tier(strings.ToLower(strings.TrimSpace(s)))
	return t, t.Rank() >= 0
}

// Axis is one skill dimension shared by request difficulty and model ability.
type Axis int

const (
	Reasoning Axis = iota
	Coding
	Math
	Knowledge
	Creative
	Instruction
	Agentic
	LongContext
	NumAxes
)

// AxisNames in Axis order.
var AxisNames = [NumAxes]string{"reasoning", "coding", "math", "knowledge", "creative", "instruction", "agentic", "long_context"}

// ParseAxis maps a name to an Axis.
func ParseAxis(s string) (Axis, bool) {
	for i, n := range AxisNames {
		if n == s {
			return Axis(i), true
		}
	}
	return 0, false
}

// Vec is a per-axis vector.
type Vec [NumAxes]float64

// Dot returns Σ a_i·b_i.
func (v Vec) Dot(o Vec) float64 {
	s := 0.0
	for i := range v {
		s += v[i] * o[i]
	}
	return s
}

// Map renders the vector with axis names.
func (v Vec) Map() map[string]float64 {
	m := make(map[string]float64, NumAxes)
	for i, x := range v {
		m[AxisNames[i]] = round3(x)
	}
	return m
}

// Pricing is USD per 1M tokens.
type Pricing struct {
	Input      float64 `yaml:"input" json:"input"`
	Output     float64 `yaml:"output" json:"output"`
	CachedIn   float64 `yaml:"cached_input" json:"cached_input"`
	CacheWrite float64 `yaml:"cache_write" json:"cache_write"`
	// Long-context surcharge: prompts above LongThreshold input tokens bill the
	// whole request at Input×LongInMult and Output×LongOutMult.
	LongThreshold int     `yaml:"long_threshold" json:"long_threshold,omitempty"`
	LongInMult    float64 `yaml:"long_input_mult" json:"long_input_mult,omitempty"`
	LongOutMult   float64 `yaml:"long_output_mult" json:"long_output_mult,omitempty"`
}

// Cost computes USD for a usage profile.
func (p Pricing) Cost(input, cached, cacheWrite, output int) float64 {
	inM, outM := 1.0, 1.0
	if p.LongThreshold > 0 && input > p.LongThreshold {
		inM, outM = nz(p.LongInMult, 1), nz(p.LongOutMult, 1)
	}
	uncached := input - cached - cacheWrite
	if uncached < 0 {
		uncached = 0
	}
	cachedPrice := p.CachedIn
	if cachedPrice == 0 {
		cachedPrice = p.Input
	}
	writePrice := p.CacheWrite
	if writePrice == 0 {
		writePrice = p.Input
	}
	return (float64(uncached)*p.Input*inM +
		float64(cached)*cachedPrice*inM +
		float64(cacheWrite)*writePrice*inM +
		float64(output)*p.Output*outM) / 1e6
}

// Thinking styles: how the provider adapter expresses reasoning effort.
const (
	ThinkNone              = ""                   // no reasoning control
	ThinkOpenAI            = "openai"             // reasoning.effort
	ThinkAnthropicAdaptive = "anthropic_adaptive" // output_config.effort, thinking always/adaptive
	ThinkAnthropicBudget   = "anthropic_budget"   // thinking.budget_tokens (Haiku 4.5)
	ThinkGeminiLevel       = "gemini_level"       // thinkingConfig.thinkingLevel
	ThinkGeminiBudget      = "gemini_budget"      // thinkingConfig.thinkingBudget (2.5 family)
)

// Caps are hard capabilities used by the constraint filter and adapters.
type Caps struct {
	Tools            bool `yaml:"tools" json:"tools"`
	ForcedToolChoice bool `yaml:"forced_tool_choice" json:"forced_tool_choice"` // tool_choice required/named
	Vision           bool `yaml:"vision" json:"vision"`
	ImageURLs        bool `yaml:"image_urls" json:"image_urls"` // accepts remote http(s) image URLs
	PDF              bool `yaml:"pdf" json:"pdf"`
	JSONSchema       bool `yaml:"json_schema" json:"json_schema"`
	Sampling         bool `yaml:"sampling" json:"sampling"` // honors temperature/top_p
	Prefill          bool `yaml:"prefill" json:"prefill"`   // accepts trailing assistant message
	Seed             bool `yaml:"seed" json:"seed"`
}

// Model is one routable upstream model.
type Model struct {
	ID         string   `yaml:"id" json:"id"` // canonical "provider/upstream"
	Provider   string   `yaml:"provider" json:"provider"`
	UpstreamID string   `yaml:"upstream_id" json:"upstream_id"`
	Aliases    []string `yaml:"aliases" json:"aliases,omitempty"`
	Display    string   `yaml:"display" json:"display"`
	Tier       Tier     `yaml:"tier" json:"tier"`
	Enabled    bool     `yaml:"enabled" json:"enabled"` // eligible for automatic routing

	Context   int `yaml:"context" json:"context"`       // total window
	MaxInput  int `yaml:"max_input" json:"max_input"`   // input cap (≤ Context)
	MaxOutput int `yaml:"max_output" json:"max_output"` // output cap

	Price      Pricing   `yaml:"price" json:"price"`
	PromoPrice *Pricing  `yaml:"promo_price" json:"promo_price,omitempty"`
	PromoUntil time.Time `yaml:"promo_until" json:"promo_until,omitempty"`

	Caps Caps `yaml:"caps" json:"caps"`

	ThinkingStyle string   `yaml:"thinking" json:"thinking"`
	Efforts       []string `yaml:"efforts" json:"efforts"`               // supported reasoning levels, ascending
	DefaultEffort string   `yaml:"default_effort" json:"default_effort"` // provider default

	// Ability priors on the shared 0–1 difficulty scale, calibrated at the
	// model's DefaultEffort. Learned online from feedback.
	Ability Vec `yaml:"-" json:"-"`

	TTFTms    float64 `yaml:"ttft_ms" json:"ttft_ms"`     // prior time-to-first-token at default effort
	TPS       float64 `yaml:"tps" json:"tps"`             // prior output tokens/sec
	Verbosity float64 `yaml:"verbosity" json:"verbosity"` // relative visible-output length
	Notes     string  `yaml:"notes" json:"notes,omitempty"`
}

// PriceAt returns the price in effect at t.
func (m *Model) PriceAt(t time.Time) Pricing {
	if m.PromoPrice != nil && !m.PromoUntil.IsZero() && t.Before(m.PromoUntil) {
		return *m.PromoPrice
	}
	return m.Price
}

// InputLimit returns the max input tokens.
func (m *Model) InputLimit() int {
	if m.MaxInput > 0 {
		return m.MaxInput
	}
	return m.Context
}

// SupportsEffort reports whether level is accepted.
func (m *Model) SupportsEffort(level string) bool {
	for _, e := range m.Efforts {
		if e == level {
			return true
		}
	}
	return false
}

// Catalog is an immutable set of models with name resolution.
type Catalog struct {
	models []*Model
	byName map[string]*Model
}

// New builds a catalog, validating ids and aliases.
func New(models []*Model) (*Catalog, error) {
	c := &Catalog{byName: make(map[string]*Model, len(models)*4)}
	for _, m := range models {
		if m.ID == "" || m.Provider == "" || m.UpstreamID == "" {
			return nil, fmt.Errorf("catalog: model %q missing id/provider/upstream_id", m.ID)
		}
		if m.Tier.Rank() < 0 {
			return nil, fmt.Errorf("catalog: model %s has unknown tier %q", m.ID, m.Tier)
		}
		if m.Verbosity == 0 {
			m.Verbosity = 1
		}
		if m.TPS == 0 {
			m.TPS = 80
		}
		if m.TTFTms == 0 {
			m.TTFTms = 800
		}
		names := append([]string{m.ID, m.UpstreamID}, m.Aliases...)
		for _, n := range names {
			k := strings.ToLower(n)
			if prev, dup := c.byName[k]; dup && prev != m {
				if n == m.UpstreamID {
					continue // same upstream id on two providers: canonical id disambiguates
				}
				return nil, fmt.Errorf("catalog: name %q used by %s and %s", n, prev.ID, m.ID)
			}
			c.byName[k] = m
		}
		c.models = append(c.models, m)
	}
	sort.SliceStable(c.models, func(i, j int) bool {
		if c.models[i].Tier.Rank() != c.models[j].Tier.Rank() {
			return c.models[i].Tier.Rank() < c.models[j].Tier.Rank()
		}
		return c.models[i].ID < c.models[j].ID
	})
	return c, nil
}

// All returns every model (sorted by tier, then id).
func (c *Catalog) All() []*Model { return c.models }

// Lookup resolves a canonical id, upstream id, or alias.
func (c *Catalog) Lookup(name string) (*Model, bool) {
	m, ok := c.byName[strings.ToLower(strings.TrimSpace(name))]
	return m, ok
}

// GuessProvider infers the provider for an unknown model name so explicit
// upstream ids not in the catalog can still be forwarded.
func GuessProvider(name string) (provider, upstream string) {
	n := strings.TrimSpace(name)
	if i := strings.IndexByte(n, '/'); i > 0 {
		p := strings.ToLower(n[:i])
		switch p {
		case "openai", "anthropic", "gemini":
			return p, n[i+1:]
		case "google":
			return "gemini", n[i+1:]
		}
		return p, n[i+1:]
	}
	l := strings.ToLower(n)
	switch {
	case strings.HasPrefix(l, "claude"):
		return "anthropic", n
	case strings.HasPrefix(l, "gemini"), strings.HasPrefix(l, "gemma"):
		return "gemini", n
	case strings.HasPrefix(l, "gpt"), strings.HasPrefix(l, "chatgpt"),
		len(l) > 1 && l[0] == 'o' && l[1] >= '0' && l[1] <= '9':
		return "openai", n
	}
	return "", n
}

func nz(v, d float64) float64 {
	if v == 0 {
		return d
	}
	return v
}

func round3(v float64) float64 {
	if v < 0 {
		return -float64(int(-v*1000+0.5)) / 1000
	}
	return float64(int(v*1000+0.5)) / 1000
}

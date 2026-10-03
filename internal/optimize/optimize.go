// Package optimize picks the (model, reasoning-effort) arm that maximizes
// expected utility for one request:
//
//	U = V·P(success) − λ·E[cost] − μ·E[latency] − switching penalties
//	subject to hard constraints and P ≥ quality floor
//
// P comes from the IRT success model (request difficulty vs. per-axis model
// ability), cost from token estimates × live prices (cache-aware), latency
// from observed TTFT/throughput. Modes are presets of (V, floor, μ, risk).
package optimize

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/features"
	"github.com/desenyon/modelrouter/internal/predict"
	"github.com/desenyon/modelrouter/internal/session"
)

// Mode is a routing objective preset.
type Mode string

const (
	ModeCost    Mode = "cost"
	ModeBalance Mode = "balance"
	ModeQuality Mode = "quality"
	ModeFast    Mode = "fast"
)

// ParseMode accepts mode names and legacy aliases.
func ParseMode(s string) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "cost", "cheap", "eco", "economy":
		return ModeCost, true
	case "balance", "balanced", "default", "":
		return ModeBalance, true
	case "quality", "intelligence", "max", "best":
		return ModeQuality, true
	case "fast", "speed", "latency":
		return ModeFast, true
	}
	return "", false
}

// Objective holds the utility parameters for a mode.
type Objective struct {
	ValueUSD   float64 `yaml:"value_usd" json:"value_usd"`         // worth of a successful answer
	Floor      float64 `yaml:"quality_floor" json:"quality_floor"` // min P(success)
	LatencyUSD float64 `yaml:"latency_usd_per_s" json:"latency_usd_per_s"`
	Risk       float64 `yaml:"risk" json:"risk"` // δ += risk·σ (aversion to under-routing)
}

// DefaultObjectives are tuned so that: balance sends easy work to luna-tier
// models and only pays for sol/astra when the success gap justifies it;
// quality buys the best success probability nearly regardless of price;
// cost accepts more risk; fast trades money for latency.
func DefaultObjectives() map[Mode]Objective {
	return map[Mode]Objective{
		ModeCost:    {ValueUSD: 0.01, Floor: 0.65, LatencyUSD: 0.0001, Risk: 0},
		ModeBalance: {ValueUSD: 0.06, Floor: 0.80, LatencyUSD: 0.0005, Risk: 0.25},
		ModeQuality: {ValueUSD: 0.60, Floor: 0.90, LatencyUSD: 0.0002, Risk: 0.5},
		ModeFast:    {ValueUSD: 0.05, Floor: 0.72, LatencyUSD: 0.02, Risk: 0.1},
	}
}

// PinKind says how the client constrained the model choice.
type PinKind uint8

const (
	PinAuto     PinKind = iota // router decides
	PinTier                    // restrict to a tier band
	PinProvider                // restrict to a provider
	PinModel                   // exact model
)

// Pin is a client constraint parsed from the requested model name.
type Pin struct {
	Kind     PinKind
	Tier     catalog.Tier
	Provider string
	Model    *catalog.Model
}

// Env is the live context the optimizer reads (all optional).
type Env struct {
	ProviderReady func(provider string) bool // credentials configured
	Health        func(provider, id string) (bool, string)
	Speed         func(id string) (ttftMs, tps float64, n int)
	Ability       func(m *catalog.Model) catalog.Vec // learned abilities
	Now           time.Time
	Lambda        float64 // budget controller cost multiplier (1 = neutral)
	Explore       float64 // exploration probability
	Rand          *rand.Rand
	SwitchUSD     float64 // penalty for switching models mid-conversation
	TokenFactor   map[string]float64
}

// Input is one routing problem.
type Input struct {
	Req     *canon.Request
	Feat    features.Features
	Pred    predict.Prediction
	Mode    Mode
	Obj     Objective
	Pin     Pin
	Session *session.State
}

// Candidate is one evaluated arm.
type Candidate struct {
	Model       *catalog.Model `json:"-"`
	ModelID     string         `json:"model"`
	Tier        catalog.Tier   `json:"tier"`
	Effort      string         `json:"effort,omitempty"`
	P           float64        `json:"p_success"`
	Ability     float64        `json:"ability"`
	CostUSD     float64        `json:"cost_usd"`
	LatencyMs   float64        `json:"latency_ms"`
	Utility     float64        `json:"utility"`
	InTokens    int            `json:"in_tokens"`
	OutTokens   int            `json:"out_tokens"`
	ReasonToks  int            `json:"reasoning_tokens"`
	CachedToks  int            `json:"cached_tokens,omitempty"`
	MaxTokens   int            `json:"max_tokens"`
	Relaxed     []string       `json:"relaxed,omitempty"`
	MeetsFloor  bool           `json:"meets_floor"`
	Explore     bool           `json:"explore,omitempty"`
	penaltyNote string
}

// Exclusion records why a model was not considered.
type Exclusion struct {
	ModelID string `json:"model"`
	Reason  string `json:"reason"`
}

// Plan is the optimizer's output.
type Plan struct {
	Mode        Mode               `json:"mode"`
	Objective   Objective          `json:"objective"`
	Difficulty  float64            `json:"difficulty_risk_adjusted"`
	Chosen      *Candidate         `json:"chosen"`
	Fallbacks   []*Candidate       `json:"fallbacks"`
	Table       []*Candidate       `json:"candidates"`
	Excluded    []Exclusion        `json:"excluded,omitempty"`
	Relaxed     bool               `json:"relaxed,omitempty"`
	Lambda      float64            `json:"lambda"`
	Prediction  predict.Prediction `json:"prediction"`
	Features    features.Features  `json:"features"`
	Passthrough bool               `json:"passthrough,omitempty"`
}

// DefaultMaxTokens caps wire output when the client sets no limit.
const DefaultMaxTokens = 32768

// Optimize evaluates every eligible arm and returns a plan. It returns an
// error only when no model can serve the request at all.
func Optimize(cat *catalog.Catalog, in Input, env Env) (*Plan, error) {
	if env.Now.IsZero() {
		env.Now = time.Now()
	}
	if env.Lambda <= 0 {
		env.Lambda = 1
	}
	plan := &Plan{Mode: in.Mode, Objective: in.Obj, Lambda: env.Lambda, Prediction: in.Pred, Features: in.Feat}
	delta := math.Min(1, in.Pred.Difficulty+in.Obj.Risk*in.Pred.Uncertainty)
	plan.Difficulty = round3(delta)

	models := cat.All()
	if in.Pin.Kind == PinModel {
		models = []*catalog.Model{in.Pin.Model}
	}
	var strict, relaxed []*Candidate
	for _, m := range models {
		hard, soft := constraints(m, in, env)
		if hard != "" {
			plan.Excluded = append(plan.Excluded, Exclusion{m.ID, hard})
			continue
		}
		for _, effort := range efforts(m, in) {
			c := evaluate(m, effort, delta, in, env)
			c.Relaxed = soft
			if c.CostUSD > 0 && in.Req.Router.MaxCostUSD > 0 && c.CostUSD > in.Req.Router.MaxCostUSD {
				c.Relaxed = append(c.Relaxed, "max_cost")
			}
			if in.Req.Router.MaxLatencyMs > 0 && c.LatencyMs > in.Req.Router.MaxLatencyMs {
				c.Relaxed = append(c.Relaxed, "max_latency")
			}
			if len(c.Relaxed) == 0 {
				strict = append(strict, c)
			} else {
				relaxed = append(relaxed, c)
			}
		}
		if len(soft) > 0 {
			plan.Excluded = append(plan.Excluded, Exclusion{m.ID, "soft:" + strings.Join(soft, ",")})
		}
	}
	pool := strict
	if len(pool) == 0 {
		pool = relaxed
		plan.Relaxed = true
	}
	if len(pool) == 0 {
		return plan, fmt.Errorf("no model can serve this request (%d excluded)", len(plan.Excluded))
	}
	floor := in.Obj.Floor
	if in.Req.Router.MinQuality > 0 {
		floor = in.Req.Router.MinQuality
	}
	best := choose(pool, floor, env)
	plan.Chosen = best
	plan.Fallbacks = fallbacks(pool, best, floor, 3)
	if len(plan.Fallbacks) < 2 && !plan.Relaxed && len(relaxed) > 0 {
		// Relaxed arms are still better than failing outright.
		plan.Fallbacks = append(plan.Fallbacks, fallbacks(relaxed, best, floor, 2-len(plan.Fallbacks))...)
	}
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].MeetsFloor != pool[j].MeetsFloor {
			return pool[i].MeetsFloor
		}
		return pool[i].Utility > pool[j].Utility
	})
	n := len(pool)
	if n > 12 {
		n = 12
	}
	plan.Table = pool[:n]
	return plan, nil
}

// choose: among arms meeting the floor maximize utility; if none meet it,
// take the cheapest arm within 0.02 of the best achievable P.
func choose(pool []*Candidate, floor float64, env Env) *Candidate {
	var feasible []*Candidate
	for _, c := range pool {
		c.MeetsFloor = c.P >= floor
		if c.MeetsFloor {
			feasible = append(feasible, c)
		}
	}
	if len(feasible) > 0 {
		sort.SliceStable(feasible, func(i, j int) bool { return feasible[i].Utility > feasible[j].Utility })
		if env.Explore > 0 && env.Rand != nil && len(feasible) > 1 && env.Rand.Float64() < env.Explore {
			// Bounded exploration: a different, still-feasible model gives the
			// learner signal about arms the policy would otherwise never try.
			for _, c := range feasible[1:] {
				if c.Model != feasible[0].Model {
					c.Explore = true
					return c
				}
			}
		}
		return feasible[0]
	}
	maxP := 0.0
	for _, c := range pool {
		maxP = math.Max(maxP, c.P)
	}
	var best *Candidate
	for _, c := range pool {
		if c.P >= maxP-0.02 && (best == nil || c.CostUSD < best.CostUSD) {
			best = c
		}
	}
	return best
}

// fallbacks returns up to n distinct-model alternatives ordered for failover:
// first the best arm on a different provider (independent failure domain),
// then remaining arms by utility, preferring ones that meet the floor.
func fallbacks(pool []*Candidate, chosen *Candidate, floor float64, n int) []*Candidate {
	if n <= 0 {
		return nil
	}
	byModel := map[*catalog.Model]*Candidate{}
	for _, c := range pool {
		if c.Model == chosen.Model {
			continue
		}
		if cur, ok := byModel[c.Model]; !ok || c.Utility > cur.Utility {
			byModel[c.Model] = c
		}
	}
	cands := make([]*Candidate, 0, len(byModel))
	for _, c := range byModel {
		cands = append(cands, c)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		fi, fj := cands[i].P >= floor-0.05, cands[j].P >= floor-0.05
		if fi != fj {
			return fi
		}
		if math.Abs(cands[i].Utility-cands[j].Utility) > 1e-12 {
			return cands[i].Utility > cands[j].Utility
		}
		return cands[i].ModelID < cands[j].ModelID
	})
	var out []*Candidate
	for i, c := range cands {
		if c.Model.Provider != chosen.Model.Provider && c.P >= floor-0.05 {
			out = append(out, c)
			cands = append(cands[:i], cands[i+1:]...)
			break
		}
	}
	for _, c := range cands {
		if len(out) >= n {
			break
		}
		out = append(out, c)
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// constraints returns a hard exclusion reason, or soft violations that are
// tolerated only if nothing satisfies them.
func constraints(m *catalog.Model, in Input, env Env) (hard string, soft []string) {
	pinned := in.Pin.Kind == PinModel
	if env.ProviderReady != nil && !env.ProviderReady(m.Provider) {
		return "no_credentials", nil
	}
	if !pinned && !m.Enabled {
		return "not_auto_routed", nil
	}
	switch in.Pin.Kind {
	case PinTier:
		if m.Tier != in.Pin.Tier {
			return "tier_pin", nil
		}
	case PinProvider:
		if m.Provider != in.Pin.Provider {
			return "provider_pin", nil
		}
	}
	r := in.Req.Router
	if len(r.Allow) > 0 && !matches(m, r.Allow) {
		return "not_allowed", nil
	}
	if len(r.Deny) > 0 && matches(m, r.Deny) {
		return "denied", nil
	}
	f := in.Feat
	tf := tokenFactor(env, m.Provider)
	need := int(float64(f.InputTokens) * tf)
	if need > m.InputLimit() || need+minOut(in) > m.Context {
		return "context_window", nil
	}
	caps := m.Caps
	if f.Tools > 0 && !caps.Tools {
		return "no_tools", nil
	}
	if f.Images > 0 && !caps.Vision {
		return "no_vision", nil
	}
	if f.RemoteImages && !caps.ImageURLs {
		return "no_remote_images", nil
	}
	if f.Files > 0 && !caps.PDF {
		return "no_documents", nil
	}
	if in.Req.ResponseFormat != nil && in.Req.ResponseFormat.Type == canon.FormatJSONSchema && !caps.JSONSchema {
		return "no_json_schema", nil
	}
	if env.Health != nil {
		if ok, why := env.Health(m.Provider, m.ID); !ok {
			if pinned {
				soft = append(soft, why)
			} else {
				return why, nil
			}
		}
	}
	if f.ForcedTool && !caps.ForcedToolChoice {
		soft = append(soft, "forced_tool_choice")
	}
	if f.Prefill && !caps.Prefill {
		soft = append(soft, "prefill")
	}
	if f.WantsSampling && !caps.Sampling {
		soft = append(soft, "sampling_params")
	}
	return "", soft
}

func minOut(in Input) int {
	if in.Feat.MaxTokens > 0 {
		return in.Feat.MaxTokens
	}
	return 1024
}

func matches(m *catalog.Model, list []string) bool {
	for _, s := range list {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == strings.ToLower(m.ID) || s == strings.ToLower(m.UpstreamID) || s == m.Provider || s == string(m.Tier) {
			return true
		}
		for _, a := range m.Aliases {
			if s == strings.ToLower(a) {
				return true
			}
		}
	}
	return false
}

// efforts lists the reasoning levels to evaluate for a model. A client-set
// reasoning_effort is honored (nearest supported level).
func efforts(m *catalog.Model, in Input) []string {
	if len(m.Efforts) == 0 {
		return []string{""}
	}
	if want := in.Req.ReasoningEffort; want != "" {
		return []string{nearest(m.Efforts, want)}
	}
	return m.Efforts
}

func nearest(levels []string, want string) string {
	idx := map[string]int{}
	for i, l := range predict.EffortLevels {
		idx[l] = i
	}
	w, ok := idx[want]
	if !ok {
		return levels[len(levels)/2]
	}
	best, bd := levels[0], 1<<30
	for _, l := range levels {
		d := idx[l] - w
		if d < 0 {
			d = -d*2 + 1
		}
		if d < bd {
			best, bd = l, d
		}
	}
	return best
}

func evaluate(m *catalog.Model, effort string, delta float64, in Input, env Env) *Candidate {
	ability := m.Ability
	if env.Ability != nil {
		ability = env.Ability(m)
	}
	theta := predict.EffectiveAbility(ability, m.DefaultEffort, effort, in.Pred.Axes)
	p := predict.SuccessProb(theta, delta)

	// Soft-constraint and pin-agnostic quality adjustments.
	note := ""
	if in.Feat.ForcedTool && !m.Caps.ForcedToolChoice {
		p *= 0.97 // tool call no longer guaranteed
		note = "forced_tool→auto"
	}

	tf := tokenFactor(env, m.Provider)
	inTok := int(float64(in.Feat.InputTokens) * tf)
	visible := int(float64(in.Pred.OutTokens) * m.Verbosity)
	if in.Feat.MaxTokens > 0 && visible > in.Feat.MaxTokens {
		visible = in.Feat.MaxTokens
	}
	reason := predict.ReasoningTokens(effortOrDefault(m, effort), visible, in.Pred.Difficulty)
	if m.ThinkingStyle == catalog.ThinkNone {
		reason = 0
	}

	cached := 0
	if s := in.Session; s != nil && s.ModelID == m.ID && env.Now.Sub(s.At) < cacheTTL(m.Provider) {
		cached = int(float64(min(s.InputTokens, inTok)) * 0.95)
	}
	price := m.PriceAt(env.Now)
	cost := price.Cost(inTok, cached, 0, visible+reason)

	ttft, tps := m.TTFTms, m.TPS
	if env.Speed != nil {
		if ot, otps, n := env.Speed(m.ID); n >= 3 {
			w := math.Min(1, float64(n)/20)
			if ot > 0 {
				ttft = w*ot + (1-w)*ttft
			}
			if otps > 0 {
				tps = w*otps + (1-w)*tps
			}
		}
	}
	latency := ttft + float64(inTok)/50 /* prefill ≈ 50k tok/s */ + float64(visible+reason)/tps*1000

	u := in.Obj.ValueUSD*p - env.Lambda*cost - in.Obj.LatencyUSD*latency/1000
	if s := in.Session; s != nil && s.ModelID != "" && s.ModelID != m.ID {
		pen := env.SwitchUSD
		if in.Feat.InToolLoop {
			pen *= 4 // switching mid tool-loop risks coherence
		}
		if m.Tier.Rank() < s.TierRank {
			pen *= 2 // hysteresis: downgrades mid-conversation need a bigger margin
		}
		u -= pen
	}
	maxTok := in.Feat.MaxTokens
	if maxTok <= 0 {
		maxTok = DefaultMaxTokens
	}
	if maxTok > m.MaxOutput {
		maxTok = m.MaxOutput
	}
	return &Candidate{
		Model: m, ModelID: m.ID, Tier: m.Tier, Effort: effort, P: round3(p), Ability: round3(theta),
		CostUSD: cost, LatencyMs: math.Round(latency), Utility: u,
		InTokens: inTok, OutTokens: visible, ReasonToks: reason, CachedToks: cached, MaxTokens: maxTok,
		penaltyNote: note,
	}
}

func effortOrDefault(m *catalog.Model, e string) string {
	if e == "" {
		return m.DefaultEffort
	}
	return e
}

// cacheTTL approximates how long a provider keeps a prompt prefix warm.
func cacheTTL(provider string) time.Duration {
	switch provider {
	case "anthropic":
		return 5 * time.Minute
	case "openai":
		return 10 * time.Minute
	case "gemini":
		return 3 * time.Minute
	}
	return 2 * time.Minute
}

func tokenFactor(env Env, provider string) float64 {
	if f, ok := env.TokenFactor[provider]; ok && f > 0 {
		return f
	}
	if provider == "anthropic" {
		return 1.15 // Claude's current tokenizer emits ~1–1.35× more tokens
	}
	return 1
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

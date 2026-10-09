// Package router ties the decision pipeline together:
//
//	model name → pin & mode → features → embedding prediction → optimizer
//
// with session affinity, learned abilities, health, and the budget λ.
package router

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/control"
	"github.com/desenyon/modelrouter/internal/features"
	"github.com/desenyon/modelrouter/internal/health"
	"github.com/desenyon/modelrouter/internal/learn"
	"github.com/desenyon/modelrouter/internal/optimize"
	"github.com/desenyon/modelrouter/internal/predict"
	"github.com/desenyon/modelrouter/internal/session"
)

// Router is safe for concurrent use.
type Router struct {
	Catalog     *catalog.Catalog
	Predictor   *predict.Predictor
	Health      *health.Tracker
	Sessions    *session.Store
	Learner     *learn.Learner // optional
	Budget      *control.Budget
	Objectives  map[optimize.Mode]optimize.Objective
	DefaultMode optimize.Mode
	Ready       func(provider string) bool
	Explore     float64
	SwitchUSD   float64

	rngMu sync.Mutex
	rng   *rand.Rand
}

// Decision is the full routing outcome for one request.
type Decision struct {
	Plan        *optimize.Plan
	Pin         optimize.Pin
	Mode        optimize.Mode
	SessionKey  string
	Session     *session.State
	Vec         []float32
	Lex         predict.Lex
	Full, Prev  string
	RoutingTime time.Duration
}

// Target is a parsed requested-model name.
type Target struct {
	Pin  optimize.Pin
	Mode optimize.Mode // "" = unspecified
	// Passthrough is set for uncatalogued explicit model ids.
	PassProvider, PassModel string
}

// VirtualModels are the router's own model names (listed on /v1/models).
var VirtualModels = []struct{ ID, Description string }{
	{"auto", "Best expected value per request (balance mode)"},
	{"auto:cost", "Cheapest model likely to succeed; accepts more risk"},
	{"auto:quality", "Highest success probability; spends when it helps"},
	{"auto:fast", "Lowest latency among models likely to succeed"},
	{"luna", "Efficient tier: gpt-6-luna, claude-haiku-4-5, gemini flash-lite"},
	{"terra", "Balanced tier: claude-sonnet-5-5, gemini-3.8-flash, gpt-5.6-terra"},
	{"sol", "Frontier tier: gpt-6.1-sol, claude-opus-5-5, gemini-3.1-pro"},
	{"astra", "Most capable tier: gpt-6-astra, claude-fable-5-1"},
	{"openai/auto", "Router restricted to OpenAI models"},
	{"anthropic/auto", "Router restricted to Anthropic models"},
	{"gemini/auto", "Router restricted to Gemini models"},
}

// ParseTarget interprets a requested model name.
func ParseTarget(cat *catalog.Catalog, name string) (Target, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimPrefix(n, "modelrouter/")
	switch n {
	case "", "auto", "router", "modelrouter", "auto-smart", "default":
		return Target{}, nil
	}
	for _, sep := range []string{":", "-"} {
		if rest, ok := strings.CutPrefix(n, "auto"+sep); ok {
			if m, ok := optimize.ParseMode(rest); ok {
				return Target{Mode: m}, nil
			}
			if t, ok := catalog.ParseTier(rest); ok {
				return Target{Pin: optimize.Pin{Kind: optimize.PinTier, Tier: t}}, nil
			}
			if isProvider(rest) {
				return Target{Pin: optimize.Pin{Kind: optimize.PinProvider, Provider: normProvider(rest)}}, nil
			}
		}
	}
	if t, ok := catalog.ParseTier(n); ok {
		return Target{Pin: optimize.Pin{Kind: optimize.PinTier, Tier: t}}, nil
	}
	if p, rest, ok := strings.Cut(n, "/"); ok && rest == "auto" && isProvider(p) {
		return Target{Pin: optimize.Pin{Kind: optimize.PinProvider, Provider: normProvider(p)}}, nil
	}
	if m, ok := cat.Lookup(name); ok {
		return Target{Pin: optimize.Pin{Kind: optimize.PinModel, Model: m}}, nil
	}
	prov, up := catalog.GuessProvider(name)
	if prov == "" {
		return Target{}, fmt.Errorf("unknown model %q: use auto, a tier (luna|terra|sol|astra), or a provider model id", name)
	}
	return Target{PassProvider: prov, PassModel: up}, nil
}

func isProvider(s string) bool {
	switch s {
	case "openai", "anthropic", "gemini", "google":
		return true
	}
	return false
}

func normProvider(s string) string {
	if s == "google" {
		return "gemini"
	}
	return s
}

// Route makes a routing decision for req.
func (r *Router) Route(req *canon.Request) (*Decision, error) {
	start := time.Now()
	if err := req.Validate(); err != nil {
		return nil, err
	}
	tgt, err := ParseTarget(r.Catalog, req.Model)
	if err != nil {
		return nil, err
	}
	mode := r.DefaultMode
	if tgt.Mode != "" {
		mode = tgt.Mode
	}
	if req.Router.Mode != "" {
		m, ok := optimize.ParseMode(req.Router.Mode)
		if !ok {
			return nil, fmt.Errorf("unknown router mode %q (cost|balance|quality|fast)", req.Router.Mode)
		}
		mode = m
	}
	pin := tgt.Pin
	if tgt.PassModel != "" {
		if req.Router.MaxCostUSD > 0 {
			return nil, fmt.Errorf("max_cost_usd requires a catalogued model with known pricing; configure %q first", req.Model)
		}
		// Explicit, uncatalogued model: synthesize an entry so it can be dispatched.
		pin = optimize.Pin{Kind: optimize.PinModel, Model: passthroughModel(tgt.PassProvider, tgt.PassModel)}
	}
	dec := &Decision{Pin: pin, Mode: mode}
	dec.SessionKey = session.Key(req)
	if st, ok := r.Sessions.Get(dec.SessionKey); ok {
		dec.Session = &st
	}
	dec.Full, dec.Prev = session.Fingerprints(req)

	feat := features.Extract(req)
	pred, vec, lex := r.Predictor.Predict(req, feat)
	dec.Vec, dec.Lex = vec, lex

	env := optimize.Env{
		ProviderReady: r.Ready,
		Health:        r.Health.Available,
		Speed: func(id string) (float64, float64, int) {
			t, p, n, _ := r.Health.Speed(id)
			return t, p, n
		},
		Now:       time.Now(),
		Lambda:    r.Budget.Lambda(),
		Explore:   r.Explore,
		SwitchUSD: r.SwitchUSD,
	}
	if r.Learner != nil {
		env.Ability = r.Learner.Ability
	}
	if r.Explore > 0 {
		r.rngMu.Lock()
		if r.rng == nil {
			r.rng = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x9e3779b97f4a7c15))
		}
		env.Rand = rand.New(rand.NewPCG(r.rng.Uint64(), r.rng.Uint64()))
		r.rngMu.Unlock()
	}
	in := optimize.Input{Req: req, Feat: feat, Pred: pred, Mode: mode, Obj: r.Objectives[mode], Pin: pin, Session: dec.Session}
	plan, err := optimize.Optimize(r.Catalog, in, env)
	if err != nil {
		return nil, err
	}
	plan.Passthrough = tgt.PassModel != ""
	dec.Plan = plan
	dec.RoutingTime = time.Since(start)
	return dec, nil
}

// Commit records which model served a conversation turn (for affinity).
func (r *Router) Commit(dec *Decision, c *optimize.Candidate, inputTokens int) {
	if dec.SessionKey == "" || c == nil {
		return
	}
	turns := 1
	if dec.Session != nil {
		turns = dec.Session.Turns + 1
	}
	r.Sessions.Put(dec.SessionKey, session.State{
		ModelID: c.ModelID, Provider: c.Model.Provider, TierRank: c.Model.Tier.Rank(),
		InputTokens: inputTokens, At: time.Now(), Turns: turns,
	})
}

func passthroughModel(provider, upstream string) *catalog.Model {
	lvl := 0.8
	var ab catalog.Vec
	for i := range ab {
		ab[i] = lvl
	}
	return &catalog.Model{
		ID: provider + "/" + upstream, Provider: provider, UpstreamID: upstream, Display: upstream,
		Tier: catalog.Terra, Context: 2_000_000, MaxOutput: 32768, Ability: ab, TTFTms: 1000, TPS: 80, Verbosity: 1,
		Caps:  catalog.Caps{Tools: true, ForcedToolChoice: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true, Sampling: true, Prefill: true, Seed: true},
		Notes: "uncatalogued passthrough; cost unknown",
	}
}

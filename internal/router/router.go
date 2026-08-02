// Package router orchestrates features → score → policy → tier → candidate.
// Policy: prefer Luna; escalate to Terra; use Sol only when the work demands it.
package router

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/desenyon/modelrouter/internal/classifier"
	"github.com/desenyon/modelrouter/internal/config"
	"github.com/desenyon/modelrouter/internal/features"
	"github.com/desenyon/modelrouter/internal/health"
	"github.com/desenyon/modelrouter/internal/models"
	"github.com/desenyon/modelrouter/internal/policy"
)

// Decision is the full routing outcome for one request.
type Decision struct {
	RequestedModel string             `json:"requested_model"`
	Mode           config.Mode        `json:"mode"`
	Tier           models.Tier        `json:"tier"`
	UpstreamModel  string             `json:"upstream_model"`
	Score          float64            `json:"score"`
	Reasons        []string           `json:"reasons"`
	Features       features.Vector    `json:"features"`
	Policy         policy.Effect      `json:"policy"`
	Candidates     []string           `json:"candidates_considered"`
	CircuitState   health.State       `json:"circuit_state,omitempty"`
	LunaMax        float64            `json:"luna_max_effective"`
	TerraMax       float64            `json:"terra_max_effective"`
	Passthrough    bool               `json:"passthrough"`
	AllowCascade   bool               `json:"allow_cascade"`
}

// Engine applies Luna-first routing with adaptive thresholds and health awareness.
type Engine struct {
	cfg      config.Config
	registry models.Registry
	policy   *policy.Engine
	health   *health.Tracker

	adaptMu    sync.Mutex
	lunaMax    float64
	terraMax   float64
	routedN    atomic.Uint64
	solN       atomic.Uint64
	scoreEWMA  float64
	scoreSamples int
}

// New builds a routing engine from config.
func New(cfg config.Config, ht *health.Tracker) *Engine {
	if ht == nil {
		ht = health.New()
	}
	return &Engine{
		cfg:      cfg,
		registry: models.NewRegistry(cfg.Models),
		policy:   policy.New(cfg.Policy),
		health:   ht,
		lunaMax:  cfg.Router.LunaMaxScore,
		terraMax: cfg.Router.TerraMaxScore,
	}
}

// Registry exposes the model registry.
func (e *Engine) Registry() models.Registry { return e.registry }

// Config returns a copy of the engine config.
func (e *Engine) Config() config.Config { return e.cfg }

// Health returns the shared health tracker.
func (e *Engine) Health() *health.Tracker { return e.health }

// Thresholds returns the effective (possibly adapted) thresholds.
func (e *Engine) Thresholds() (luna, terra float64) {
	e.adaptMu.Lock()
	defer e.adaptMu.Unlock()
	return e.lunaMax, e.terraMax
}

// RouteInput is everything needed to make a decision.
type RouteInput struct {
	Model string
	Mode  config.Mode
	Req   classifier.Request
}

// Route classifies the request and selects an upstream model.
func (e *Engine) Route(in RouteInput) Decision {
	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = "auto"
	}

	mode := in.Mode
	if mode == "" {
		mode = e.cfg.ModeOrDefault()
	}

	if !models.IsVirtual(model) {
		return Decision{
			RequestedModel: model,
			Mode:           mode,
			UpstreamModel:  model,
			Passthrough:    true,
			Reasons:        []string{"explicit upstream model"},
			AllowCascade:   true,
			CircuitState:   e.health.StateOf(model),
		}
	}

	var pin models.Tier
	if t, ok := models.ForcedTier(model); ok {
		pin = t
	}

	class := classifier.Classify(in.Req)
	lunaMax, terraMax := e.Thresholds()

	base := e.pickTier(mode, class.Score, lunaMax, terraMax)
	eff := e.policy.Evaluate(mode, class.Score, class.Features, pin)
	tier := policy.Apply(base, eff)

	// Hard guarantee: never spend Sol when Luna clearly fits.
	if tier == models.TierSol && class.Score <= lunaMax {
		tier = models.TierLuna
		eff.Hits = append(eff.Hits, "sol_suppressed_luna_sufficient")
		eff.SuppressSol = true
	}

	candidates, chosen, circ := e.pickCandidate(tier, eff)
	reasons := append([]string{}, class.Reasons...)
	reasons = append(reasons, "mode="+string(mode), "policy=luna-first")
	reasons = append(reasons, eff.Hits...)

	return Decision{
		RequestedModel: model,
		Mode:           mode,
		Tier:           tier,
		UpstreamModel:  chosen,
		Score:          class.Score,
		Reasons:        reasons,
		Features:       class.Features,
		Policy:         eff,
		Candidates:     candidates,
		CircuitState:   circ,
		LunaMax:        lunaMax,
		TerraMax:       terraMax,
		AllowCascade:   eff.AllowCascade && e.cfg.Cascade.Enabled,
	}
}

func (e *Engine) pickTier(mode config.Mode, score, lunaMax, terraMax float64) models.Tier {
	switch mode {
	case config.ModeCost:
		switch {
		case score <= lunaMax+0.12:
			return models.TierLuna
		case score <= terraMax+0.08:
			return models.TierTerra
		default:
			return models.TierSol
		}
	case config.ModeIntelligence:
		switch {
		case score <= lunaMax*0.7:
			return models.TierLuna
		case score <= terraMax:
			return models.TierTerra
		default:
			return models.TierSol
		}
	default:
		switch {
		case score <= lunaMax:
			return models.TierLuna
		case score <= terraMax:
			return models.TierTerra
		default:
			return models.TierSol
		}
	}
}

// pickCandidate walks primary+fallbacks for the tier; if all open and escalate
// is allowed, tries the next tier (never Sol when suppressed).
func (e *Engine) pickCandidate(tier models.Tier, eff policy.Effect) (considered []string, chosen string, st health.State) {
	try := []models.Tier{tier}
	if next := tier.Next(); next != "" {
		if !(eff.SuppressSol && next == models.TierSol) {
			if eff.MaxTier == "" || next.Order() <= eff.MaxTier.Order() {
				try = append(try, next)
			}
		}
	}

	for _, t := range try {
		for _, m := range e.registry.Candidates(t) {
			considered = append(considered, m)
			if e.health.Allow(m) {
				return considered, m, e.health.StateOf(m)
			}
		}
	}
	// All open — return primary of original tier anyway (fail open for availability).
	primary := e.registry.ResolveTier(tier)
	considered = append(considered, primary+"#forced")
	return considered, primary, health.Open
}

// RecordOutcome updates adaptive thresholds and health after a completed route.
func (e *Engine) RecordOutcome(dec Decision, latencyMs float64, success bool, errMsg string) {
	lat0 := time.Duration(latencyMs * float64(time.Millisecond))
	if dec.Passthrough {
		if success {
			e.health.RecordSuccess(dec.UpstreamModel, lat0)
		} else {
			e.health.RecordFailure(dec.UpstreamModel, errMsg)
		}
		return
	}

	lat := time.Duration(latencyMs * float64(time.Millisecond))
	if success {
		e.health.RecordSuccess(dec.UpstreamModel, lat)
	} else {
		e.health.RecordFailure(dec.UpstreamModel, errMsg)
	}

	if !e.cfg.Router.Adaptive {
		return
	}

	e.routedN.Add(1)
	if dec.Tier == models.TierSol {
		e.solN.Add(1)
	}

	e.adaptMu.Lock()
	defer e.adaptMu.Unlock()
	if e.scoreSamples == 0 {
		e.scoreEWMA = dec.Score
	} else {
		e.scoreEWMA = 0.2*dec.Score + 0.8*e.scoreEWMA
	}
	e.scoreSamples++

	routed := e.routedN.Load()
	if routed < 20 {
		return
	}
	solShare := float64(e.solN.Load()) / float64(routed)
	target := e.cfg.Router.SolShareTarget
	baseLuna := e.cfg.Router.LunaMaxScore
	baseTerra := e.cfg.Router.TerraMaxScore

	// If Sol share spikes, raise luna_max slightly (send more to Luna).
	delta := 0.0
	if solShare > target+0.05 {
		delta = 0.03
	} else if solShare < target*0.4 && e.scoreEWMA > baseLuna {
		delta = -0.02 // slightly more Terra/Sol room when Sol is rare and scores high
	}
	e.lunaMax = clamp(baseLuna+delta, baseLuna-0.05, baseLuna+0.05)
	e.terraMax = clamp(baseTerra+delta*0.5, baseTerra-0.05, baseTerra+0.05)
	if e.terraMax <= e.lunaMax {
		e.terraMax = e.lunaMax + 0.15
	}
}

// Escalate returns a follow-up decision for cascade retry.
func (e *Engine) Escalate(prev Decision) (Decision, bool) {
	if !prev.AllowCascade || prev.Passthrough {
		return Decision{}, false
	}
	nextTier := prev.Tier.Next()
	if nextTier == "" {
		// try next candidate in same tier
		cands := e.registry.Candidates(prev.Tier)
		for i, m := range cands {
			if m == prev.UpstreamModel && i+1 < len(cands) {
				alt := cands[i+1]
				if !e.health.Allow(alt) {
					continue
				}
				out := prev
				out.UpstreamModel = alt
				out.Candidates = append(out.Candidates, alt)
				out.Reasons = append(out.Reasons, "cascade_same_tier")
				out.AllowCascade = false // one shot
				return out, true
			}
		}
		return Decision{}, false
	}
	if prev.Policy.SuppressSol && nextTier == models.TierSol {
		return Decision{}, false
	}
	if prev.Policy.ForceTier == models.TierLuna && !prev.Policy.AllowCascade {
		return Decision{}, false
	}

	cands, chosen, circ := e.pickCandidate(nextTier, prev.Policy)
	if chosen == "" || chosen == prev.UpstreamModel {
		return Decision{}, false
	}
	out := prev
	out.Tier = nextTier
	out.UpstreamModel = chosen
	out.Candidates = append(out.Candidates, cands...)
	out.CircuitState = circ
	out.Reasons = append(out.Reasons, "cascade:"+string(prev.Tier)+"→"+string(nextTier))
	out.AllowCascade = false
	return out, true
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

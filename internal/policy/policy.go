// Package policy applies Luna-first guardrails on top of the classifier score.
package policy

import (
	"github.com/desenyon/modelrouter/internal/config"
	"github.com/desenyon/modelrouter/internal/features"
	"github.com/desenyon/modelrouter/internal/models"
)

// Effect captures forced/capped tier constraints from rule evaluation.
type Effect struct {
	ForceTier    models.Tier `json:"force_tier,omitempty"`
	MinTier      models.Tier `json:"min_tier,omitempty"`
	MaxTier      models.Tier `json:"max_tier,omitempty"`
	SuppressSol  bool        `json:"suppress_sol"`
	Hits         []string    `json:"hits"`
	AllowCascade bool        `json:"allow_cascade"`
}

// Engine evaluates declarative routing rules.
type Engine struct {
	cfg config.PolicyConfig
}

// New builds a policy engine.
func New(cfg config.PolicyConfig) *Engine {
	return &Engine{cfg: cfg}
}

// Evaluate runs rules in order. Pin (if any) short-circuits.
func (e *Engine) Evaluate(mode config.Mode, score float64, v features.Vector, pin models.Tier) Effect {
	eff := Effect{AllowCascade: true, Hits: make([]string, 0, 6)}

	if pin != "" {
		eff.ForceTier = pin
		eff.Hits = append(eff.Hits, "client_pin:"+string(pin))
		eff.AllowCascade = pin != models.TierLuna || score > e.cfg.SuppressSolBelow
		return eff
	}

	// Easy short work → force Luna (never Sol, cascade disabled upward past Terra).
	if e.cfg.ForceLunaEasy &&
		v.EasyMarkers > 0 && v.HardMarkers == 0 && v.Tools == 0 &&
		v.Chars < 800 && !v.StructuredOut {
		eff.ForceTier = models.TierLuna
		eff.SuppressSol = true
		eff.AllowCascade = false
		eff.Hits = append(eff.Hits, "force_luna_easy")
		return eff
	}

	// Absolute Sol suppress when score says Luna is enough.
	suppressBelow := e.cfg.SuppressSolBelow
	if suppressBelow <= 0 {
		suppressBelow = 0.42
	}
	if score <= suppressBelow {
		eff.MaxTier = models.TierTerra
		eff.SuppressSol = true
		eff.Hits = append(eff.Hits, "suppress_sol_below")
	}

	minTools := e.cfg.MinTerraTools
	if minTools <= 0 {
		minTools = 3
	}
	if v.Tools >= minTools || v.StructuredOut {
		eff.MinTier = maxTier(eff.MinTier, models.TierTerra)
		eff.Hits = append(eff.Hits, "min_terra_tools_or_structured")
	}

	// Extreme hard work under intelligence (or very high score) may floor at Sol.
	if v.HardMarkers >= e.cfg.MinSolHardMarkers && v.EstTokens >= e.cfg.MinSolTokens {
		if mode == config.ModeIntelligence || score >= 0.85 {
			eff.MinTier = maxTier(eff.MinTier, models.TierSol)
			eff.Hits = append(eff.Hits, "min_sol_extreme")
		} else {
			eff.MinTier = maxTier(eff.MinTier, models.TierTerra)
			eff.Hits = append(eff.Hits, "min_terra_hard_context")
		}
	}

	return eff
}

// Apply merges a scored base tier with policy constraints.
func Apply(base models.Tier, eff Effect) models.Tier {
	if eff.ForceTier != "" {
		return eff.ForceTier
	}
	t := base
	if eff.MinTier != "" && t.Order() < eff.MinTier.Order() {
		t = eff.MinTier
	}
	if eff.MaxTier != "" && t.Order() > eff.MaxTier.Order() {
		t = eff.MaxTier
	}
	if eff.SuppressSol && t == models.TierSol {
		t = models.TierTerra
		if eff.MaxTier == models.TierLuna {
			t = models.TierLuna
		}
	}
	return t
}

func maxTier(a, b models.Tier) models.Tier {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if a.Order() >= b.Order() {
		return a
	}
	return b
}

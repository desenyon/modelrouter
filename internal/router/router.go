// Package router picks the cheapest tier that still fits the task.
// Policy: prefer Luna; escalate to Terra; use Sol only when the work demands it.
package router

import (
	"strings"

	"github.com/desenyon/modelrouter/internal/classifier"
	"github.com/desenyon/modelrouter/internal/config"
	"github.com/desenyon/modelrouter/internal/models"
)

// Decision is the full routing outcome for one request.
type Decision struct {
	RequestedModel string             `json:"requested_model"`
	Mode           config.Mode        `json:"mode"`
	Tier           models.Tier        `json:"tier"`
	UpstreamModel  string             `json:"upstream_model"`
	Score          float64            `json:"score"`
	Reasons        []string           `json:"reasons"`
	Signals        classifier.Signals `json:"signals"`
	Passthrough    bool               `json:"passthrough"`
}

// Engine applies Luna-first routing policy.
type Engine struct {
	cfg      config.Config
	registry models.Registry
}

// New builds a routing engine from config.
func New(cfg config.Config) *Engine {
	return &Engine{
		cfg:      cfg,
		registry: models.NewRegistry(cfg.Models.Luna, cfg.Models.Terra, cfg.Models.Sol),
	}
}

// Registry exposes the model registry.
func (e *Engine) Registry() models.Registry { return e.registry }

// Config returns a copy of the engine config.
func (e *Engine) Config() config.Config { return e.cfg }

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

	// Explicit concrete model → passthrough (no Luna/Sol policy).
	if !models.IsVirtual(model) {
		return Decision{
			RequestedModel: model,
			Mode:           mode,
			UpstreamModel:  model,
			Passthrough:    true,
			Reasons:        []string{"explicit upstream model"},
		}
	}

	if tier, ok := models.ForcedTier(model); ok {
		return Decision{
			RequestedModel: model,
			Mode:           mode,
			Tier:           tier,
			UpstreamModel:  e.registry.ResolveTier(tier),
			Reasons:        []string{"client pinned tier " + string(tier)},
		}
	}

	class := classifier.Classify(in.Req)
	tier := e.pickTier(mode, class.Score)
	reasons := append([]string{}, class.Reasons...)
	reasons = append(reasons, "mode="+string(mode), "policy=luna-first")

	// Hard guarantee: never spend Sol when Luna clearly fits.
	if tier == models.TierSol && class.Score <= e.cfg.Router.LunaMaxScore {
		tier = models.TierLuna
		reasons = append(reasons, "sol suppressed — luna sufficient")
	}

	return Decision{
		RequestedModel: model,
		Mode:           mode,
		Tier:           tier,
		UpstreamModel:  e.registry.ResolveTier(tier),
		Score:          class.Score,
		Reasons:        reasons,
		Signals:        class.Signals,
	}
}

func (e *Engine) pickTier(mode config.Mode, score float64) models.Tier {
	lunaMax := e.cfg.Router.LunaMaxScore
	terraMax := e.cfg.Router.TerraMaxScore

	switch mode {
	case config.ModeCost:
		// Aggressive Luna preference. Sol only for extreme scores.
		switch {
		case score <= lunaMax+0.12:
			return models.TierLuna
		case score <= terraMax+0.08:
			return models.TierTerra
		default:
			return models.TierSol
		}
	case config.ModeIntelligence:
		// Still Luna for trivial work — never Sol by default.
		switch {
		case score <= lunaMax*0.7:
			return models.TierLuna
		case score <= terraMax:
			return models.TierTerra
		default:
			return models.TierSol
		}
	default: // balance
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

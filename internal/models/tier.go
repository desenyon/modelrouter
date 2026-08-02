// Package models defines logical routing tiers and the live model registry.
package models

import (
	"strings"

	"github.com/desenyon/modelrouter/internal/config"
)

// Tier is a capability / cost band. Luna handles the majority of work;
// Sol is reserved for tasks that genuinely need frontier intelligence.
type Tier string

const (
	TierLuna  Tier = "luna"
	TierTerra Tier = "terra"
	TierSol   Tier = "sol"
)

// Order returns a comparable rank (lower = cheaper / lighter).
func (t Tier) Order() int {
	switch t {
	case TierLuna:
		return 0
	case TierTerra:
		return 1
	case TierSol:
		return 2
	default:
		return 1
	}
}

// Next returns the next higher tier, or empty at Sol.
func (t Tier) Next() Tier {
	switch t {
	case TierLuna:
		return TierTerra
	case TierTerra:
		return TierSol
	default:
		return ""
	}
}

// Registry maps tiers to candidate pools.
type Registry struct {
	Luna  config.TierPool
	Terra config.TierPool
	Sol   config.TierPool
}

// NewRegistry builds a registry from config pools.
func NewRegistry(m config.ModelsConfig) Registry {
	return Registry{Luna: m.Luna, Terra: m.Terra, Sol: m.Sol}
}

// Pool returns the candidate pool for a tier.
func (r Registry) Pool(t Tier) config.TierPool {
	switch t {
	case TierLuna:
		return r.Luna
	case TierTerra:
		return r.Terra
	case TierSol:
		return r.Sol
	default:
		return r.Terra
	}
}

// ResolveTier returns the primary upstream model for a tier.
func (r Registry) ResolveTier(t Tier) string {
	return r.Pool(t).Primary
}

// Candidates returns primary+fallbacks for a tier.
func (r Registry) Candidates(t Tier) []string {
	return r.Pool(t).Candidates()
}

// Virtual models that trigger intelligent routing.
var virtual = map[string]struct{}{
	"auto":              {},
	"router":            {},
	"modelrouter":       {},
	"auto-smart":        {},
	"luna":              {},
	"terra":             {},
	"sol":               {},
	"modelrouter/auto":  {},
	"modelrouter/luna":  {},
	"modelrouter/terra": {},
	"modelrouter/sol":   {},
}

// IsVirtual reports whether the requested model should be routed.
func IsVirtual(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	_, ok := virtual[m]
	return ok
}

// ForcedTier returns a pinned tier when the client asked for luna/terra/sol.
func ForcedTier(model string) (Tier, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	switch m {
	case "luna", "modelrouter/luna":
		return TierLuna, true
	case "terra", "modelrouter/terra":
		return TierTerra, true
	case "sol", "modelrouter/sol":
		return TierSol, true
	default:
		return "", false
	}
}

// Catalog lists the virtual models exposed on /v1/models.
func (r Registry) Catalog() []CatalogEntry {
	return []CatalogEntry{
		{ID: "auto", OwnedBy: "modelrouter", Description: "Intelligent router — Luna by default, Sol only when needed"},
		{ID: "router", OwnedBy: "modelrouter", Description: "Alias for auto"},
		{ID: "luna", OwnedBy: "modelrouter", Description: "Force the efficient Luna tier → " + r.Luna.Primary},
		{ID: "terra", OwnedBy: "modelrouter", Description: "Force the balanced Terra tier → " + r.Terra.Primary},
		{ID: "sol", OwnedBy: "modelrouter", Description: "Force the frontier Sol tier → " + r.Sol.Primary},
	}
}

// CatalogEntry is an OpenAI-style model list item with router metadata.
type CatalogEntry struct {
	ID          string
	OwnedBy     string
	Description string
}

// Package models defines logical routing tiers and the live model registry.
package models

import "strings"

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

// Registry maps virtual model names and tiers to upstream IDs.
type Registry struct {
	Luna  string
	Terra string
	Sol   string
}

// NewRegistry builds a registry from concrete upstream model IDs.
func NewRegistry(luna, terra, sol string) Registry {
	return Registry{Luna: luna, Terra: terra, Sol: sol}
}

// ResolveTier returns the upstream model for a tier.
func (r Registry) ResolveTier(t Tier) string {
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

// Virtual models that trigger intelligent routing.
var virtual = map[string]struct{}{
	"auto":          {},
	"router":        {},
	"modelrouter":   {},
	"auto-smart":    {},
	"luna":          {},
	"terra":         {},
	"sol":           {},
	"modelrouter/auto": {},
	"modelrouter/luna": {},
	"modelrouter/terra": {},
	"modelrouter/sol": {},
}

// IsVirtual reports whether the requested model should be routed.
func IsVirtual(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	_, ok := virtual[m]
	return ok
}

// ForcedTier returns a pinned tier when the client asked for luna/terra/sol
// directly. Empty means full auto routing.
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
		{ID: "luna", OwnedBy: "modelrouter", Description: "Force the efficient Luna tier → " + r.Luna},
		{ID: "terra", OwnedBy: "modelrouter", Description: "Force the balanced Terra tier → " + r.Terra},
		{ID: "sol", OwnedBy: "modelrouter", Description: "Force the frontier Sol tier → " + r.Sol},
	}
}

// CatalogEntry is an OpenAI-style model list item with router metadata.
type CatalogEntry struct {
	ID          string
	OwnedBy     string
	Description string
}

package canon

import (
	"fmt"
	"math"
	"strings"
)

// Validate checks provider-independent controls after body and header merging.
// Unknown top-level wire fields remain tolerated for SDK compatibility.
func (r *Request) Validate() error {
	if len(r.Messages) == 0 {
		return fmt.Errorf("messages must be a non-empty array")
	}
	if r.MaxTokens < 0 {
		return fmt.Errorf("max_tokens must not be negative")
	}
	for _, c := range []struct {
		name       string
		value, max float64
	}{
		{"router.max_cost_usd", r.Router.MaxCostUSD, math.MaxFloat64},
		{"router.max_latency_ms", r.Router.MaxLatencyMs, math.MaxFloat64},
		{"router.min_quality", r.Router.MinQuality, 1},
	} {
		if math.IsNaN(c.value) || math.IsInf(c.value, 0) || c.value < 0 || c.value > c.max {
			return fmt.Errorf("%s must be finite and in [0,%g]", c.name, c.max)
		}
	}
	for _, c := range []struct {
		name  string
		value *float64
		max   float64
	}{
		{"temperature", r.Temperature, 2}, {"top_p", r.TopP, 1},
	} {
		if c.value != nil && (math.IsNaN(*c.value) || math.IsInf(*c.value, 0) || *c.value < 0 || *c.value > c.max) {
			return fmt.Errorf("%s must be finite and in [0,%g]", c.name, c.max)
		}
	}
	switch r.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return fmt.Errorf("unsupported reasoning_effort %q", r.ReasoningEffort)
	}
	names := make(map[string]bool, len(r.Tools))
	for _, t := range r.Tools {
		if strings.TrimSpace(t.Name) == "" || names[t.Name] {
			return fmt.Errorf("tool names must be non-empty and unique")
		}
		names[t.Name] = true
	}
	if r.ToolChoice.Forced() && len(r.Tools) == 0 {
		return fmt.Errorf("forced tool_choice requires tools")
	}
	if r.ToolChoice.Mode == ToolChoiceNamed && !names[r.ToolChoice.Name] {
		return fmt.Errorf("tool_choice references unknown function %q", r.ToolChoice.Name)
	}
	return nil
}

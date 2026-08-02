// Package classifier turns a feature vector into a complexity score in [0,1].
// Low → Luna territory. High → Sol may be justified.
package classifier

import (
	"github.com/desenyon/modelrouter/internal/features"
)

// Request is accepted for CLI/gateway convenience; features are extracted inside.
type Request = features.Request

// Message aliases features.Message.
type Message = features.Message

// Result is a complexity score plus human-readable reasons and the raw vector.
type Result struct {
	Score    float64         `json:"score"`
	Reasons  []string        `json:"reasons"`
	Features features.Vector `json:"features"`
}

// Signals is kept as an alias of the feature vector for API compatibility.
type Signals = features.Vector

// Classify extracts features and blends axes into a single score.
func Classify(req Request) Result {
	v := features.Extract(req)
	return Score(v)
}

// Score blends a pre-extracted feature vector.
func Score(v features.Vector) Result {
	score := 0.16
	reasons := make([]string, 0, 10)

	// Context pressure
	switch {
	case v.EstTokens > 6000:
		score += 0.36
		reasons = append(reasons, "very large context")
	case v.EstTokens > 2000:
		score += 0.22
		reasons = append(reasons, "large context")
	case v.EstTokens > 600:
		score += 0.10
		reasons = append(reasons, "moderate context")
	case v.Chars < 280 && v.HardMarkers == 0:
		score -= 0.08
		reasons = append(reasons, "short prompt")
	}

	if v.Messages >= 12 {
		score += 0.12
		reasons = append(reasons, "long conversation")
	} else if v.Messages >= 6 {
		score += 0.06
		reasons = append(reasons, "multi-turn thread")
	}

	if v.Tools > 0 {
		score += 0.08 + min(0.14, float64(v.Tools)*0.03)
		reasons = append(reasons, "tool use requested")
	}
	if v.HasImages {
		score += 0.07
		reasons = append(reasons, "multimodal input")
	}
	if v.StructuredOut {
		score += 0.06
		reasons = append(reasons, "structured output")
	}
	if v.SystemHeavy {
		score += 0.05
		reasons = append(reasons, "heavy system prompt")
	}
	if v.CodeFences >= 3 {
		score += 0.08
		reasons = append(reasons, "dense code context")
	} else if v.CodeFences > 0 {
		score += 0.03
	}
	if v.JSONBlocks >= 2 {
		score += 0.04
		reasons = append(reasons, "structured payloads")
	}
	if v.UserTailChars > 4000 {
		score += 0.06
		reasons = append(reasons, "large last user turn")
	}

	if v.HardMarkers > 0 {
		score += min(0.45, 0.14+float64(v.HardMarkers)*0.09)
		reasons = append(reasons, "hard-task markers")
	}
	if v.EasyMarkers > 0 && v.HardMarkers == 0 {
		score -= min(0.20, float64(v.EasyMarkers)*0.06)
		reasons = append(reasons, "easy-task markers")
	}
	if v.AgentLike && v.HardMarkers == 0 && v.Chars < 4000 {
		score += 0.04
		reasons = append(reasons, "light agent pattern")
	}
	if v.AgentLike && (v.HardMarkers > 0 || v.Chars > 6000) {
		score += 0.09
		reasons = append(reasons, "heavy agent pattern")
	}

	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "default baseline")
	}
	return Result{Score: round3(score), Reasons: reasons, Features: v}
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func round3(v float64) float64 {
	return float64(int(v*1000+0.5)) / 1000
}

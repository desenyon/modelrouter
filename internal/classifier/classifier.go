// Package classifier scores request complexity so the router can pick a tier.
package classifier

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Request is the minimal signal set the classifier needs.
type Request struct {
	Messages   []Message
	Tools      int
	HasImages  bool
	StreamHint bool
}

// Message is a chat message fragment.
type Message struct {
	Role    string
	Content string
}

// Result is a complexity score in [0, 1] plus human-readable reasons.
type Result struct {
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons"`
	Signals Signals  `json:"signals"`
}

// Signals are the raw features used for scoring (useful for /v1/route previews).
type Signals struct {
	Chars       int  `json:"chars"`
	Messages    int  `json:"messages"`
	Tools       int  `json:"tools"`
	HasImages   bool `json:"has_images"`
	HardMarkers int  `json:"hard_markers"`
	EasyMarkers int  `json:"easy_markers"`
	AgentLike   bool `json:"agent_like"`
}

var hardRE = regexp.MustCompile(`(?i)\b(architect|refactor|migrate|distributed|consensus|prove|theorem|security audit|race condition|deadlock|performance regression|multi-?agent|long[- ]?horizon|from scratch|large codebase|production incident|root cause|formal verif)\b`)

var easyRE = regexp.MustCompile(`(?i)\b(summarize|tl;dr|rename|format|lint|typo|translate|what does|explain briefly|one[- ]?liner|boilerplate|css color|commit message|changelog|regex for|json schema)\b`)

var agentRE = regexp.MustCompile(`(?i)\b(step by step|plan then|use tools?|call the|function call|write tests?|implement|debug|investigate)\b`)

// Classify inspects a request and returns a complexity score.
// Low scores → Luna territory. High scores → Sol may be justified.
func Classify(req Request) Result {
	var b strings.Builder
	msgs := 0
	for _, m := range req.Messages {
		if m.Content == "" {
			continue
		}
		msgs++
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	text := b.String()
	chars := utf8.RuneCountInString(text)

	hard := len(hardRE.FindAllStringIndex(text, -1))
	easy := len(easyRE.FindAllStringIndex(text, -1))
	agentLike := agentRE.MatchString(text) || req.Tools > 0

	sig := Signals{
		Chars:       chars,
		Messages:    msgs,
		Tools:       req.Tools,
		HasImages:   req.HasImages,
		HardMarkers: hard,
		EasyMarkers: easy,
		AgentLike:   agentLike,
	}

	score := 0.18
	reasons := make([]string, 0, 8)

	switch {
	case chars > 24000:
		score += 0.38
		reasons = append(reasons, "very large context")
	case chars > 8000:
		score += 0.24
		reasons = append(reasons, "large context")
	case chars > 2500:
		score += 0.12
		reasons = append(reasons, "moderate context")
	case chars < 280 && hard == 0:
		score -= 0.08
		reasons = append(reasons, "short prompt")
	}

	if msgs >= 12 {
		score += 0.14
		reasons = append(reasons, "long conversation")
	} else if msgs >= 6 {
		score += 0.07
		reasons = append(reasons, "multi-turn thread")
	}

	if req.Tools > 0 {
		score += 0.10 + min(0.12, float64(req.Tools)*0.03)
		reasons = append(reasons, "tool use requested")
	}
	if req.HasImages {
		score += 0.08
		reasons = append(reasons, "multimodal input")
	}
	if hard > 0 {
		// Hard markers dominate — short prompts can still need Sol.
		score += min(0.45, 0.16+float64(hard)*0.10)
		reasons = append(reasons, "hard-task markers")
	}
	if easy > 0 && hard == 0 {
		score -= min(0.18, float64(easy)*0.06)
		reasons = append(reasons, "easy-task markers")
	}
	if agentLike && hard == 0 && chars < 4000 {
		score += 0.05
		reasons = append(reasons, "light agent pattern")
	}
	if agentLike && (hard > 0 || chars > 6000) {
		score += 0.10
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

	return Result{Score: round3(score), Reasons: reasons, Signals: sig}
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

// Package features extracts a multi-axis signal vector from a chat request
// in a single O(n) pass — no embeddings, no secondary model calls.
package features

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Request is the raw material for feature extraction.
type Request struct {
	Messages      []Message
	Tools         int
	HasImages     bool
	StructuredOut bool
	StreamHint    bool
}

// Message is one chat turn.
type Message struct {
	Role    string
	Content string
}

// Vector is the full feature set used by the classifier and policy engine.
type Vector struct {
	Chars         int  `json:"chars"`
	EstTokens     int  `json:"est_tokens"`
	Messages      int  `json:"messages"`
	Tools         int  `json:"tools"`
	HasImages     bool `json:"has_images"`
	CodeFences    int  `json:"code_fences"`
	JSONBlocks    int  `json:"json_blocks"`
	HardMarkers   int  `json:"hard_markers"`
	EasyMarkers   int  `json:"easy_markers"`
	AgentLike     bool `json:"agent_like"`
	SystemHeavy   bool `json:"system_heavy"`
	StructuredOut bool `json:"structured_out"`
	UserTailChars int  `json:"user_tail_chars"`
	StreamHint    bool `json:"stream_hint"`
}

var (
	hardRE = regexp.MustCompile(`(?i)\b(architect|refactor|migrate|distributed|consensus|prove|theorem|security audit|race condition|deadlock|performance regression|multi-?agent|long[- ]?horizon|from scratch|large codebase|production incident|root cause|formal verif|correctness|invariant)\b`)
	easyRE = regexp.MustCompile(`(?i)\b(summarize|tl;dr|rename|format|lint|typo|translate|what does|explain briefly|one[- ]?liner|boilerplate|css color|commit message|changelog|regex for|json schema)\b`)
	agentRE = regexp.MustCompile(`(?i)\b(step by step|plan then|use tools?|call the|function call|write tests?|implement|debug|investigate|orchestrat)\b`)
	fenceRE = regexp.MustCompile("(?m)^```")
	jsonRE  = regexp.MustCompile(`(?s)\{[^{}]{20,}\}|\[[^\[\]]{20,}\]`)
)

// Extract builds a feature vector from a request in one pass.
func Extract(req Request) Vector {
	var all strings.Builder
	var systemChars int
	msgs := 0
	userTail := 0

	for _, m := range req.Messages {
		if m.Content == "" {
			continue
		}
		msgs++
		all.WriteString(m.Content)
		all.WriteByte('\n')
		role := strings.ToLower(m.Role)
		n := utf8.RuneCountInString(m.Content)
		switch role {
		case "system", "developer":
			systemChars += n
		case "user":
			userTail = n
		}
	}

	text := all.String()
	chars := utf8.RuneCountInString(text)
	hard := len(hardRE.FindAllStringIndex(text, -1))
	easy := len(easyRE.FindAllStringIndex(text, -1))
	fences := len(fenceRE.FindAllStringIndex(text, -1))
	if fences > 0 {
		fences = (fences + 1) / 2 // open+close pairs ≈ fence count
	}
	jsons := len(jsonRE.FindAllStringIndex(text, -1))
	agentLike := agentRE.MatchString(text) || req.Tools > 0

	return Vector{
		Chars:         chars,
		EstTokens:     chars / 4,
		Messages:      msgs,
		Tools:         req.Tools,
		HasImages:     req.HasImages,
		CodeFences:    fences,
		JSONBlocks:    jsons,
		HardMarkers:   hard,
		EasyMarkers:   easy,
		AgentLike:     agentLike,
		SystemHeavy:   systemChars > 1500 || (chars > 0 && float64(systemChars)/float64(chars) > 0.45),
		StructuredOut: req.StructuredOut,
		UserTailChars: userTail,
		StreamHint:    req.StreamHint,
	}
}

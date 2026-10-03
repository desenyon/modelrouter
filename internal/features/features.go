// Package features extracts structural request signals in a single pass:
// token estimates, tool/schema complexity, code/math density, constraint
// counts, conversation shape. Semantic difficulty comes from the embedder in
// package predict; these features adjust it with things embeddings can't see.
package features

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/desenyon/modelrouter/internal/canon"
)

// Features is the structural signal vector for one request.
type Features struct {
	InputTokens     int     `json:"input_tokens"`
	LastUserTokens  int     `json:"last_user_tokens"`
	SystemTokens    int     `json:"system_tokens"`
	Messages        int     `json:"messages"`
	UserTurns       int     `json:"user_turns"`
	Tools           int     `json:"tools"`
	ToolSchemaBytes int     `json:"tool_schema_bytes,omitempty"`
	ForcedTool      bool    `json:"forced_tool,omitempty"`
	InToolLoop      bool    `json:"in_tool_loop,omitempty"`
	ToolResults     int     `json:"tool_results,omitempty"`
	Images          int     `json:"images,omitempty"`
	RemoteImages    bool    `json:"remote_images,omitempty"`
	Files           int     `json:"files,omitempty"`
	JSONMode        bool    `json:"json_mode,omitempty"`
	SchemaFields    int     `json:"schema_fields,omitempty"`
	CodeBlocks      int     `json:"code_blocks,omitempty"`
	CodeLines       int     `json:"code_lines,omitempty"`
	MathDensity     float64 `json:"math_density,omitempty"`
	Constraints     int     `json:"constraints,omitempty"`
	Questions       int     `json:"questions,omitempty"`
	LengthHint      int     `json:"length_hint_tokens,omitempty"`
	StackTrace      bool    `json:"stack_trace,omitempty"`
	Diff            bool    `json:"diff,omitempty"`
	Trivial         bool    `json:"trivial,omitempty"`
	MaxTokens       int     `json:"max_tokens,omitempty"`
	Prefill         bool    `json:"prefill,omitempty"`
	WantsSampling   bool    `json:"wants_sampling,omitempty"`
	Seed            bool    `json:"seed,omitempty"`
}

// Per-attachment token estimates (providers bill images/PDF pages by size;
// these are mid-range planning numbers).
const (
	imageTokens   = 1100
	filePageBytes = 3000 // ≈ bytes of base64 PDF per page
	pageTokens    = 1500
)

// Extract computes features for req.
func Extract(req *canon.Request) Features {
	f := Features{
		Messages:      len(req.Messages),
		Tools:         len(req.Tools),
		ForcedTool:    req.ToolChoice.Forced() && len(req.Tools) > 0,
		InToolLoop:    req.InToolLoop(),
		JSONMode:      req.ResponseFormat.WantsJSON(),
		MaxTokens:     req.MaxTokens,
		Prefill:       req.EndsWithAssistant(),
		WantsSampling: (req.Temperature != nil && *req.Temperature != 1) || (req.TopP != nil && *req.TopP != 1),
		Seed:          req.Seed != nil,
	}
	for _, t := range req.Tools {
		f.ToolSchemaBytes += len(t.Parameters) + len(t.Description)
		f.InputTokens += 12 + EstimateTokens(t.Name) + EstimateTokens(t.Description) + len(t.Parameters)/3
	}
	if req.ResponseFormat != nil && len(req.ResponseFormat.Schema) > 0 {
		f.SchemaFields = countSchemaFields(req.ResponseFormat.Schema)
		f.InputTokens += len(req.ResponseFormat.Schema) / 3
	}
	lastUser := -1
	for i, m := range req.Messages {
		if m.Role == canon.RoleUser && strings.TrimSpace(m.Text()) != "" {
			lastUser = i
		}
	}
	for i, m := range req.Messages {
		mt := 4 // per-message framing
		for _, p := range m.Parts {
			switch p.Type {
			case canon.PartText:
				mt += EstimateTokens(p.Text)
			case canon.PartImage:
				f.Images++
				if p.URL != "" {
					f.RemoteImages = true
				}
				mt += imageTokens
			case canon.PartFile, canon.PartAudio:
				f.Files++
				pages := len(p.Data)/filePageBytes + 1
				mt += pages * pageTokens
			}
		}
		for _, tc := range m.ToolCalls {
			mt += 8 + EstimateTokens(tc.Name) + EstimateTokens(tc.Arguments)
		}
		switch m.Role {
		case canon.RoleSystem, canon.RoleDeveloper:
			f.SystemTokens += mt
		case canon.RoleUser:
			f.UserTurns++
		case canon.RoleTool:
			f.ToolResults++
		}
		if i == lastUser {
			f.LastUserTokens = mt
		}
		f.InputTokens += mt
	}
	if lastUser >= 0 {
		scanText(req.Messages[lastUser].Text(), &f)
	}
	return f
}

// EstimateTokens approximates BPE token counts (o200k/Claude/Gemini class
// tokenizers) without a vocabulary: word pieces ≈ 1 token per ≤5 letters,
// digits in groups of 3, each symbol 1, CJK ≈ 1 per char.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	tokens := 0
	run, digits := 0, 0
	flush := func() {
		if run > 0 {
			tokens += 1 + (run-1)/5
			run = 0
		}
		if digits > 0 {
			tokens += (digits + 2) / 3
			digits = 0
		}
	}
	spaces := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			i++
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
				if digits > 0 {
					flush()
				}
				run++
			case c >= '0' && c <= '9':
				if run > 0 {
					flush()
				}
				digits++
			case c == ' ':
				flush()
				spaces++
			case c == '\n' || c == '\t' || c == '\r':
				flush()
				tokens++
			default:
				flush()
				tokens++
			}
			continue
		}
		r, sz := utf8.DecodeRuneInString(s[i:])
		i += sz
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r):
			flush()
			tokens++
		case unicode.IsLetter(r):
			run++ // accented / Cyrillic / Greek letters: count as word chars, slightly denser
			if run%3 == 0 {
				tokens++
			}
		default:
			flush()
			tokens += 2 // emoji, rare symbols
		}
	}
	flush()
	// Runs of indentation spaces compress well; charge a fraction.
	tokens += spaces / 8
	if tokens == 0 {
		tokens = 1
	}
	return tokens
}

var greetings = map[string]bool{
	"hi": true, "hello": true, "hey": true, "thanks": true, "thank you": true, "ok": true, "okay": true,
	"yo": true, "sup": true, "good morning": true, "good night": true, "cool": true, "great": true,
	"thx": true, "ty": true, "bye": true, "nice": true, "lol": true, "yes": true, "no": true,
}

func scanText(t string, f *Features) {
	trim := strings.TrimSpace(t)
	low := strings.ToLower(strings.Trim(trim, " !.?,:)"))
	if greetings[low] {
		f.Trivial = true
	}
	inFence := false
	mathSyms, letters := 0, 0
	for _, line := range strings.Split(t, "\n") {
		lt := strings.TrimSpace(line)
		if strings.HasPrefix(lt, "```") {
			if !inFence {
				f.CodeBlocks++
			}
			inFence = !inFence
			continue
		}
		if inFence {
			f.CodeLines++
			continue
		}
		if isConstraintLine(lt) {
			f.Constraints++
		}
		if strings.HasPrefix(lt, "Traceback (most recent call last)") || strings.HasPrefix(lt, "panic:") ||
			strings.HasPrefix(lt, "at ") && strings.Contains(lt, "(") && strings.Contains(lt, ".java:") ||
			strings.HasPrefix(lt, "error[E") || strings.HasPrefix(lt, "goroutine ") && strings.HasSuffix(lt, ":") {
			f.StackTrace = true
		}
		if strings.HasPrefix(lt, "diff --git") || strings.HasPrefix(lt, "@@ ") || strings.HasPrefix(lt, "+++ ") {
			f.Diff = true
		}
	}
	for _, r := range t {
		switch {
		case r == '?':
			f.Questions++
		case strings.ContainsRune("∑∫√≤≥≠≈∀∃∈∉⊂⊆∞∂∇πθλσμ±×÷^", r):
			mathSyms++
		case unicode.IsLetter(r):
			letters++
		}
	}
	mathSyms += 3 * (strings.Count(t, `\frac`) + strings.Count(t, `\int`) + strings.Count(t, `\sum`) + strings.Count(t, `\begin{`) + strings.Count(t, "$$"))
	if letters+mathSyms > 0 {
		f.MathDensity = float64(mathSyms) / float64(letters+mathSyms)
	}
	f.LengthHint = lengthHint(strings.ToLower(t))
}

func isConstraintLine(lt string) bool {
	if lt == "" {
		return false
	}
	if strings.HasPrefix(lt, "- ") || strings.HasPrefix(lt, "* ") || strings.HasPrefix(lt, "• ") {
		return true
	}
	// "1." / "2)" numbered items
	i := 0
	for i < len(lt) && i < 3 && lt[i] >= '0' && lt[i] <= '9' {
		i++
	}
	if i > 0 && i < len(lt) && (lt[i] == '.' || lt[i] == ')') {
		return true
	}
	l := strings.ToLower(lt)
	return strings.HasPrefix(l, "must ") || strings.HasPrefix(l, "do not ") || strings.HasPrefix(l, "don't ") ||
		strings.HasPrefix(l, "never ") || strings.HasPrefix(l, "always ") || strings.HasPrefix(l, "ensure ")
}

// lengthHint finds explicit output-length requests ("2000 words", "10 pages").
func lengthHint(l string) int {
	units := []struct {
		word   string
		tokens float64
	}{
		{"words", 1.35}, {"word", 1.35}, {"pages", 700}, {"page", 700}, {"paragraphs", 120},
		{"lines", 12}, {"sentences", 25}, {"tokens", 1}, {"items", 40}, {"examples", 60},
	}
	best := 0
	fields := strings.Fields(l)
	for i := 0; i+1 < len(fields); i++ {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.ReplaceAll(fields[i], ",", ""), "+"))
		if err != nil || n <= 0 || n > 100000 {
			continue
		}
		next := strings.Trim(fields[i+1], ".,;:!?)")
		for _, u := range units {
			if next == u.word {
				if v := int(float64(n) * u.tokens); v > best {
					best = v
				}
				break
			}
		}
	}
	return best
}

func countSchemaFields(raw json.RawMessage) int {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return 0
	}
	n := 0
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if p, ok := t["properties"].(map[string]any); ok {
				n += len(p)
			}
			for _, c := range t {
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(v)
	return n
}

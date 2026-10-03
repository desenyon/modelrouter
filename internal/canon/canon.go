// Package canon defines the provider-neutral request, response, and stream
// event model. Every ingress format decodes into canon.Request and every
// provider adapter consumes it, so N ingress formats × M providers costs N+M
// translators instead of N×M.
package canon

import (
	"encoding/json"
	"strings"
)

// Role values used in Message.Role.
const (
	RoleSystem    = "system"
	RoleDeveloper = "developer"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Part types.
const (
	PartText  = "text"
	PartImage = "image"
	PartFile  = "file"
	PartAudio = "audio"
)

// Request is one chat generation request in canonical form.
type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`

	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        ToolChoice      `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ResponseFormat    *ResponseFormat `json:"response_format,omitempty"`

	MaxTokens int `json:"max_tokens,omitempty"` // 0 = unset
	// MaxTokensInclusive is true when the limit already includes reasoning
	// tokens (OpenAI max_completion_tokens semantics); otherwise it bounds
	// visible output and the router adds a reasoning allowance on the wire.
	MaxTokensInclusive bool     `json:"max_tokens_inclusive,omitempty"`
	Temperature        *float64 `json:"temperature,omitempty"`
	TopP               *float64 `json:"top_p,omitempty"`
	Stop               []string `json:"stop,omitempty"`
	Seed               *int64   `json:"seed,omitempty"`

	// ReasoningEffort is the client's requested effort (normalized lower-case),
	// empty when the router should choose.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	Stream      bool   `json:"stream,omitempty"`
	StreamUsage bool   `json:"stream_usage,omitempty"`
	User        string `json:"user,omitempty"`

	Router RouterOptions `json:"router"`
}

// RouterOptions are per-request routing controls (body "router" extension or
// X-Modelrouter-* headers).
type RouterOptions struct {
	Mode         string   `json:"mode,omitempty"`           // cost|balance|quality|fast
	MaxCostUSD   float64  `json:"max_cost_usd,omitempty"`   // hard ceiling on expected cost
	MaxLatencyMs float64  `json:"max_latency_ms,omitempty"` // soft ceiling on expected latency
	MinQuality   float64  `json:"min_quality,omitempty"`    // override success-probability floor
	Allow        []string `json:"allow,omitempty"`          // model ids / providers / tiers allowed
	Deny         []string `json:"deny,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
	Explain      bool     `json:"explain,omitempty"`
	NoCache      bool     `json:"no_cache,omitempty"`
	ForceCache   bool     `json:"force_cache,omitempty"`
}

// Message is one conversation turn.
type Message struct {
	Role       string     `json:"role"`
	Parts      []Part     `json:"parts,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// Part is one content block.
type Part struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	URL       string `json:"url,omitempty"`        // http(s) URL for image/file
	MediaType string `json:"media_type,omitempty"` // for inline data
	Data      string `json:"data,omitempty"`       // base64 inline data
	Detail    string `json:"detail,omitempty"`
	Filename  string `json:"filename,omitempty"`
}

// Text concatenates all text parts.
func (m Message) Text() string {
	if len(m.Parts) == 1 && m.Parts[0].Type == PartText {
		return m.Parts[0].Text
	}
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == PartText {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Tool is a client function definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

// ToolChoice modes.
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"
	ToolChoiceNamed    = "named"
)

// ToolChoice controls whether/which tool the model must call.
type ToolChoice struct {
	Mode string `json:"mode,omitempty"` // "" means auto
	Name string `json:"name,omitempty"`
}

// Forced reports whether the client demands a tool call.
func (t ToolChoice) Forced() bool {
	return t.Mode == ToolChoiceRequired || t.Mode == ToolChoiceNamed
}

// ToolCall is a model-issued function call.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ResponseFormat types.
const (
	FormatText       = "text"
	FormatJSONObject = "json_object"
	FormatJSONSchema = "json_schema"
)

// ResponseFormat constrains the output shape.
type ResponseFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict bool            `json:"strict,omitempty"`
}

// WantsJSON reports whether the output must be JSON.
func (r *ResponseFormat) WantsJSON() bool {
	return r != nil && (r.Type == FormatJSONObject || r.Type == FormatJSONSchema)
}

// Finish reasons (OpenAI vocabulary).
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
)

// Usage is token accounting for one completion. InputTokens is the total
// prompt size including cached and cache-write tokens.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"` // includes reasoning tokens
	CachedTokens     int `json:"cached_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
}

// Add accumulates another usage record.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CachedTokens += o.CachedTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.ReasoningTokens += o.ReasoningTokens
}

// Response is an assembled completion.
type Response struct {
	ID           string     `json:"id"`
	Model        string     `json:"model"`
	Content      string     `json:"content"`
	Refusal      string     `json:"refusal,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason"`
	Usage        Usage      `json:"usage"`
}

// EventKind enumerates stream events.
type EventKind uint8

const (
	EvText EventKind = iota + 1
	EvReasoning
	EvRefusal
	EvToolStart // ToolIndex, ToolID, ToolName
	EvToolArgs  // ToolIndex, Text = argument JSON fragment
	EvFinish    // FinishReason
	EvUsage     // Usage
	EvMeta      // ID / Model from upstream
	EvKeepalive // upstream activity without visible output (thinking, pings)
)

// Event is one normalized stream event emitted by a provider adapter.
type Event struct {
	Kind         EventKind
	Text         string
	ToolIndex    int
	ToolID       string
	ToolName     string
	FinishReason string
	Usage        *Usage
	ID           string
	Model        string
}

// Visible reports whether the event carries client-visible output; the
// dispatcher commits to an attempt (no more failover) on the first visible event.
func (e Event) Visible() bool {
	switch e.Kind {
	case EvText, EvRefusal, EvToolStart, EvToolArgs:
		return true
	}
	return false
}

// Assembler folds stream events into a Response.
type Assembler struct {
	resp    Response
	content strings.Builder
	refusal strings.Builder
	args    []strings.Builder
	usage   Usage
	gotUse  bool
}

// Add applies one event.
func (a *Assembler) Add(ev Event) {
	switch ev.Kind {
	case EvMeta:
		if ev.ID != "" {
			a.resp.ID = ev.ID
		}
		if ev.Model != "" {
			a.resp.Model = ev.Model
		}
	case EvText:
		a.content.WriteString(ev.Text)
	case EvRefusal:
		a.refusal.WriteString(ev.Text)
	case EvToolStart:
		for len(a.resp.ToolCalls) <= ev.ToolIndex {
			a.resp.ToolCalls = append(a.resp.ToolCalls, ToolCall{})
			a.args = append(a.args, strings.Builder{})
		}
		tc := &a.resp.ToolCalls[ev.ToolIndex]
		if ev.ToolID != "" {
			tc.ID = ev.ToolID
		}
		if ev.ToolName != "" {
			tc.Name = ev.ToolName
		}
	case EvToolArgs:
		for len(a.args) <= ev.ToolIndex {
			a.resp.ToolCalls = append(a.resp.ToolCalls, ToolCall{})
			a.args = append(a.args, strings.Builder{})
		}
		a.args[ev.ToolIndex].WriteString(ev.Text)
	case EvFinish:
		a.resp.FinishReason = ev.FinishReason
	case EvUsage:
		if ev.Usage != nil {
			a.usage = *ev.Usage
			a.gotUse = true
		}
	}
}

// Response returns the assembled response.
func (a *Assembler) Response() Response {
	r := a.resp
	r.Content = a.content.String()
	r.Refusal = a.refusal.String()
	r.ToolCalls = append([]ToolCall(nil), a.resp.ToolCalls...)
	for i := range r.ToolCalls {
		r.ToolCalls[i].Arguments = a.args[i].String()
	}
	if r.FinishReason == "" {
		if len(r.ToolCalls) > 0 {
			r.FinishReason = FinishToolCalls
		} else {
			r.FinishReason = FinishStop
		}
	}
	r.Usage = a.usage
	return r
}

// HasUsage reports whether the upstream reported usage.
func (a *Assembler) HasUsage() bool { return a.gotUse }

// Events converts an assembled response back into a stream (used for cache
// replay and for providers that only return whole responses).
func (r Response) Events() []Event {
	evs := make([]Event, 0, 4+2*len(r.ToolCalls))
	evs = append(evs, Event{Kind: EvMeta, ID: r.ID, Model: r.Model})
	if r.Refusal != "" {
		evs = append(evs, Event{Kind: EvRefusal, Text: r.Refusal})
	}
	if r.Content != "" {
		evs = append(evs, Event{Kind: EvText, Text: r.Content})
	}
	for i, tc := range r.ToolCalls {
		evs = append(evs, Event{Kind: EvToolStart, ToolIndex: i, ToolID: tc.ID, ToolName: tc.Name})
		if tc.Arguments != "" {
			evs = append(evs, Event{Kind: EvToolArgs, ToolIndex: i, Text: tc.Arguments})
		}
	}
	u := r.Usage
	evs = append(evs, Event{Kind: EvFinish, FinishReason: r.FinishReason}, Event{Kind: EvUsage, Usage: &u})
	return evs
}

// LastUserText returns the text of the most recent user message that carries
// text (skipping tool results), which is the strongest routing signal.
func (r *Request) LastUserText() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		m := r.Messages[i]
		if m.Role == RoleUser {
			if t := m.Text(); strings.TrimSpace(t) != "" {
				return t
			}
		}
	}
	return ""
}

// SystemText concatenates system and developer instructions.
func (r *Request) SystemText() string {
	var b strings.Builder
	for _, m := range r.Messages {
		if m.Role == RoleSystem || m.Role == RoleDeveloper {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(m.Text())
		}
	}
	return b.String()
}

// HasImages reports whether any message carries image parts.
func (r *Request) HasImages() (any, remoteURL bool) {
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			if p.Type == PartImage {
				any = true
				if p.URL != "" {
					remoteURL = true
				}
			}
		}
	}
	return
}

// HasFiles reports whether any message carries non-image file parts (PDFs).
func (r *Request) HasFiles() bool {
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			if p.Type == PartFile {
				return true
			}
		}
	}
	return false
}

// EndsWithAssistant reports an assistant-prefill request.
func (r *Request) EndsWithAssistant() bool {
	n := len(r.Messages)
	return n > 0 && r.Messages[n-1].Role == RoleAssistant && len(r.Messages[n-1].ToolCalls) == 0
}

// InToolLoop reports whether the last message is a tool result (agent loop).
func (r *Request) InToolLoop() bool {
	n := len(r.Messages)
	return n > 0 && r.Messages[n-1].Role == RoleTool
}

// ToolNameByID resolves the function name for a tool_call_id from history.
func (r *Request) ToolNameByID(id string) string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		for _, tc := range r.Messages[i].ToolCalls {
			if tc.ID == id {
				return tc.Name
			}
		}
	}
	return ""
}

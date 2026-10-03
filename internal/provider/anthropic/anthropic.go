// Package anthropic is the native Claude adapter for the Messages API
// (/v1/messages). It speaks the wire format directly (rather than via the
// SDK) because a gateway must pass through arbitrary fields and stream raw
// SSE without waiting for SDK type updates.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/provider"
	"github.com/desenyon/modelrouter/internal/provider/sse"
)

const apiVersion = "2023-06-01"

// Adapter talks to api.anthropic.com.
type Adapter struct {
	baseURL         string
	apiKey          string
	client          *http.Client
	serverFallbacks bool
}

// New builds an adapter. serverFallbacks enables Anthropic's server-side
// refusal fallback ("fallbacks":"default") on models that support it.
func New(baseURL, apiKey string, client *http.Client, serverFallbacks bool) *Adapter {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	return &Adapter{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, client: client, serverFallbacks: serverFallbacks}
}

// Name implements provider.Provider.
func (a *Adapter) Name() string { return "anthropic" }

// fallbackModels accept `fallbacks: "default"` (beta server-side-fallback-2026-07-01).
var fallbackModels = map[string]bool{"claude-fable-5-1": true, "claude-opus-5-5": true, "claude-sonnet-5-5": true}

// Open implements provider.Provider.
func (a *Adapter) Open(ctx context.Context, call *provider.Call) (provider.Stream, error) {
	body := BuildRequest(call)
	var betas []string
	if a.serverFallbacks && fallbackModels[call.UpstreamID] {
		body["fallbacks"] = "default"
		betas = append(betas, "server-side-fallback-2026-07-01")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, &provider.Error{Provider: "anthropic", Model: call.UpstreamID, Kind: provider.KindBadRequest, Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, provider.Classify(err, "anthropic", call.UpstreamID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", apiVersion)
	if len(betas) > 0 {
		req.Header.Set("anthropic-beta", strings.Join(betas, ","))
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, provider.Classify(err, "anthropic", call.UpstreamID)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		e := provider.FromHTTP("anthropic", call.UpstreamID, resp)
		if resp.StatusCode == 529 {
			e.Kind = provider.KindServer // overloaded
		}
		return nil, e
	}
	return &stream{model: call.UpstreamID, resp: resp, r: sse.NewReader(resp.Body), blocks: map[int]*block{}}, nil
}

// BuildRequest translates a canonical call into a Messages API body.
func BuildRequest(call *provider.Call) map[string]any {
	r := call.Req
	caps := call.Caps()
	system, msgs := buildMessages(r, caps)
	body := map[string]any{
		"model":      call.UpstreamID,
		"max_tokens": call.MaxTokens,
		"messages":   msgs,
		"stream":     true,
	}
	if rf := r.ResponseFormat; rf != nil && rf.Type == canon.FormatJSONObject {
		system = appendSys(system, "Respond with a single valid JSON object and nothing else.")
	}
	if system != "" {
		body["system"] = system
	}
	outputConfig := map[string]any{}
	if rf := r.ResponseFormat; rf != nil && rf.Type == canon.FormatJSONSchema && len(rf.Schema) > 0 && caps.JSONSchema {
		outputConfig["format"] = map[string]any{"type": "json_schema", "schema": SanitizeSchema(rf.Schema)}
	}
	thinkingOn := false
	switch call.ThinkingStyle() {
	case catalog.ThinkAnthropicAdaptive:
		if e := mapEffort(call.Effort); e != "" {
			outputConfig["effort"] = e
		}
	case catalog.ThinkAnthropicBudget:
		if call.Effort != "" && call.Effort != "none" && call.MaxTokens > 2048 {
			budget := call.MaxTokens / 2
			if budget > 16384 {
				budget = 16384
			}
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
			thinkingOn = true
		}
	}
	if len(outputConfig) > 0 {
		body["output_config"] = outputConfig
	}
	if len(r.Tools) > 0 && caps.Tools {
		tools := make([]map[string]any, 0, len(r.Tools))
		for _, t := range r.Tools {
			schema := t.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tool := map[string]any{"name": t.Name, "input_schema": schema}
			if t.Description != "" {
				tool["description"] = t.Description
			}
			if t.Strict {
				tool["strict"] = true
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
		tc := map[string]any{"type": "auto"}
		switch r.ToolChoice.Mode {
		case canon.ToolChoiceNone:
			tc = map[string]any{"type": "none"}
		case canon.ToolChoiceRequired:
			if caps.ForcedToolChoice && !thinkingOn {
				tc = map[string]any{"type": "any"}
			}
		case canon.ToolChoiceNamed:
			if caps.ForcedToolChoice && !thinkingOn {
				tc = map[string]any{"type": "tool", "name": r.ToolChoice.Name}
			}
		}
		if r.ParallelToolCalls != nil && !*r.ParallelToolCalls && tc["type"] != "none" {
			tc["disable_parallel_tool_use"] = true
		}
		body["tool_choice"] = tc
	}
	if caps.Sampling && !thinkingOn {
		if r.Temperature != nil {
			t := *r.Temperature
			if t > 1 {
				t = 1 // Anthropic range is [0,1]
			}
			body["temperature"] = t
		}
		if r.TopP != nil {
			body["top_p"] = *r.TopP
		}
	}
	if len(r.Stop) > 0 {
		body["stop_sequences"] = r.Stop
	}
	if r.User != "" {
		body["metadata"] = map[string]any{"user_id": r.User}
	}
	if call.CacheHint {
		body["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	return body
}

func mapEffort(e string) string {
	switch e {
	case "":
		return ""
	case "none", "minimal":
		return "low"
	}
	return e
}

func appendSys(sys, s string) string {
	if sys == "" {
		return s
	}
	return sys + "\n\n" + s
}

type msg struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

func buildMessages(r *canon.Request, caps catalog.Caps) (string, []msg) {
	var system strings.Builder
	out := make([]msg, 0, len(r.Messages))
	push := func(role string, blocks ...map[string]any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, msg{Role: role, Content: blocks})
	}
	for _, m := range r.Messages {
		switch m.Role {
		case canon.RoleSystem, canon.RoleDeveloper:
			if t := m.Text(); t != "" {
				if system.Len() > 0 {
					system.WriteString("\n\n")
				}
				system.WriteString(t)
			}
		case canon.RoleUser:
			push("user", contentBlocks(m.Parts)...)
		case canon.RoleAssistant:
			var blocks []map[string]any
			if t := m.Text(); strings.TrimSpace(t) != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": t})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": provider.ParseArgs(tc.Arguments)})
			}
			push("assistant", blocks...)
		case canon.RoleTool:
			content := m.Text()
			if content == "" {
				content = "(empty)"
			}
			push("user", map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": content})
		}
	}
	// The conversation must open with a user turn.
	if len(out) == 0 || out[0].Role != "user" {
		out = append([]msg{{Role: "user", Content: []map[string]any{{"type": "text", "text": "(continue)"}}}}, out...)
	}
	// Models without prefill support reject a trailing assistant turn.
	if n := len(out); n > 1 && out[n-1].Role == "assistant" && !caps.Prefill && !hasToolUse(out[n-1]) {
		prefix := blocksText(out[n-1].Content)
		out = out[:n-1]
		out[len(out)-1].Content = append(out[len(out)-1].Content, map[string]any{"type": "text", "text": "Begin your response with exactly: " + prefix})
	}
	return system.String(), out
}

func hasToolUse(m msg) bool {
	for _, b := range m.Content {
		if b["type"] == "tool_use" {
			return true
		}
	}
	return false
}

func blocksText(bs []map[string]any) string {
	var sb strings.Builder
	for _, b := range bs {
		if t, ok := b["text"].(string); ok {
			sb.WriteString(t)
		}
	}
	return sb.String()
}

func contentBlocks(parts []canon.Part) []map[string]any {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case canon.PartText:
			if p.Text != "" {
				out = append(out, map[string]any{"type": "text", "text": p.Text})
			}
		case canon.PartImage:
			if p.URL != "" {
				out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": p.URL}})
			} else {
				out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": p.MediaType, "data": p.Data}})
			}
		case canon.PartFile:
			if p.URL != "" {
				out = append(out, map[string]any{"type": "document", "source": map[string]any{"type": "url", "url": p.URL}})
			} else {
				mt := p.MediaType
				if mt == "" {
					mt = "application/pdf"
				}
				out = append(out, map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": mt, "data": p.Data}})
			}
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"type": "text", "text": "(empty)"})
	}
	return out
}

// SanitizeSchema adapts a JSON Schema to Anthropic structured-output limits:
// every object gets additionalProperties:false, and unsupported numeric /
// string-length constraints are removed (the dispatcher still validates the
// output is JSON).
func SanitizeSchema(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for _, k := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
				"minLength", "maxLength", "minItems", "maxItems", "uniqueItems", "pattern"} {
				delete(t, k)
			}
			if t["type"] == "object" || t["properties"] != nil {
				t["additionalProperties"] = false
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
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

type block struct {
	kind    string
	toolIdx int
	gotArgs bool
}

type stream struct {
	model   string
	resp    *http.Response
	r       *sse.Reader
	pending []canon.Event
	blocks  map[int]*block
	nTools  int
	usage   canon.Usage
	done    bool
}

func (s *stream) Header() http.Header { return s.resp.Header }
func (s *stream) Close() error        { return s.resp.Body.Close() }

func (s *stream) Next() (canon.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.done {
			return canon.Event{}, io.EOF
		}
		e, err := s.r.Next()
		if err != nil {
			if err == io.EOF {
				return canon.Event{}, &provider.Error{Provider: "anthropic", Model: s.model, Kind: provider.KindTransport, Message: "stream ended before message_stop"}
			}
			return canon.Event{}, provider.Classify(err, "anthropic", s.model)
		}
		if err := s.handle(e.Data); err != nil {
			return canon.Event{}, err
		}
	}
}

type aUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type aEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID    string  `json:"id"`
		Model string  `json:"model"`
		Usage *aUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Text string `json:"text"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *aUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *stream) handle(data []byte) error {
	var e aEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil
	}
	switch e.Type {
	case "message_start":
		if e.Message != nil {
			s.pending = append(s.pending, canon.Event{Kind: canon.EvMeta, ID: e.Message.ID, Model: e.Message.Model})
			if u := e.Message.Usage; u != nil {
				s.usage.CachedTokens = u.CacheReadInputTokens
				s.usage.CacheWriteTokens = u.CacheCreationInputTokens
				s.usage.InputTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
				s.usage.OutputTokens = u.OutputTokens
			}
		}
	case "content_block_start":
		if cb := e.ContentBlock; cb != nil {
			b := &block{kind: cb.Type}
			s.blocks[e.Index] = b
			switch cb.Type {
			case "tool_use":
				b.toolIdx = s.nTools
				s.nTools++
				s.pending = append(s.pending, canon.Event{Kind: canon.EvToolStart, ToolIndex: b.toolIdx, ToolID: cb.ID, ToolName: cb.Name})
			case "text":
				if cb.Text != "" {
					s.pending = append(s.pending, canon.Event{Kind: canon.EvText, Text: cb.Text})
				}
			}
		}
	case "content_block_delta":
		b := s.blocks[e.Index]
		if e.Delta == nil || b == nil {
			return nil
		}
		switch e.Delta.Type {
		case "text_delta":
			if e.Delta.Text != "" {
				s.pending = append(s.pending, canon.Event{Kind: canon.EvText, Text: e.Delta.Text})
			}
		case "thinking_delta", "signature_delta":
			s.pending = append(s.pending, canon.Event{Kind: canon.EvKeepalive})
		case "input_json_delta":
			if e.Delta.PartialJSON != "" {
				b.gotArgs = true
				s.pending = append(s.pending, canon.Event{Kind: canon.EvToolArgs, ToolIndex: b.toolIdx, Text: e.Delta.PartialJSON})
			}
		}
	case "content_block_stop":
		if b := s.blocks[e.Index]; b != nil && b.kind == "tool_use" && !b.gotArgs {
			s.pending = append(s.pending, canon.Event{Kind: canon.EvToolArgs, ToolIndex: b.toolIdx, Text: "{}"})
		}
	case "message_delta":
		if e.Usage != nil {
			s.usage.OutputTokens = e.Usage.OutputTokens
		}
		if e.Delta != nil && e.Delta.StopReason != "" {
			s.pending = append(s.pending, canon.Event{Kind: canon.EvFinish, FinishReason: mapStop(e.Delta.StopReason)})
		}
	case "ping":
		s.pending = append(s.pending, canon.Event{Kind: canon.EvKeepalive})
	case "message_stop":
		u := s.usage
		s.pending = append(s.pending, canon.Event{Kind: canon.EvUsage, Usage: &u})
		s.done = true
	case "error":
		k := provider.KindServer
		msg := "stream error"
		if e.Error != nil {
			msg = e.Error.Type + ": " + e.Error.Message
			switch e.Error.Type {
			case "rate_limit_error":
				k = provider.KindRateLimit
			case "invalid_request_error":
				k = provider.KindBadRequest
			}
		}
		return &provider.Error{Provider: "anthropic", Model: s.model, Kind: k, Message: msg}
	}
	return nil
}

func mapStop(r string) string {
	switch r {
	case "max_tokens", "model_context_window_exceeded":
		return canon.FinishLength
	case "tool_use":
		return canon.FinishToolCalls
	case "refusal":
		return canon.FinishContentFilter
	}
	return canon.FinishStop
}

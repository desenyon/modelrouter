// Package openai is the native OpenAI adapter. It targets the Responses API
// (/v1/responses): GPT-6-class models don't support tool calling on Chat
// Completions, while Responses supports every current model and feature.
package openai

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

// Adapter talks to api.openai.com (or a compatible Responses endpoint).
type Adapter struct {
	name    string
	baseURL string
	apiKey  string
	org     string
	client  *http.Client
}

// New builds an adapter. baseURL defaults to https://api.openai.com/v1.
func New(name, baseURL, apiKey, org string, client *http.Client) *Adapter {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if name == "" {
		name = "openai"
	}
	return &Adapter{name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, org: org, client: client}
}

// Name implements provider.Provider.
func (a *Adapter) Name() string { return a.name }

// Open implements provider.Provider.
func (a *Adapter) Open(ctx context.Context, call *provider.Call) (provider.Stream, error) {
	body, err := json.Marshal(BuildRequest(call))
	if err != nil {
		return nil, &provider.Error{Provider: a.name, Model: call.UpstreamID, Kind: provider.KindBadRequest, Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, provider.Classify(err, a.name, call.UpstreamID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	if a.org != "" {
		req.Header.Set("OpenAI-Organization", a.org)
	}
	if call.RequestID != "" {
		req.Header.Set("X-Client-Request-Id", call.RequestID)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, provider.Classify(err, a.name, call.UpstreamID)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, provider.FromHTTP(a.name, call.UpstreamID, resp)
	}
	return &stream{a: a, model: call.UpstreamID, resp: resp, r: sse.NewReader(resp.Body), toolIdx: map[int]int{}}, nil
}

// BuildRequest translates a canonical call into a Responses API body.
func BuildRequest(call *provider.Call) map[string]any {
	r := call.Req
	caps := call.Caps()
	body := map[string]any{
		"model":  call.UpstreamID,
		"input":  buildInput(r),
		"stream": true,
		"store":  false,
	}
	if call.MaxTokens > 0 {
		body["max_output_tokens"] = call.MaxTokens
	}
	if len(r.Tools) > 0 && caps.Tools {
		tools := make([]map[string]any, 0, len(r.Tools))
		for _, t := range r.Tools {
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			// Responses defaults strict=true; chat clients' schemas are usually
			// not strict-compatible, so pass the client's choice explicitly.
			tools = append(tools, map[string]any{
				"type": "function", "name": t.Name, "description": t.Description,
				"parameters": params, "strict": t.Strict,
			})
		}
		body["tools"] = tools
		switch r.ToolChoice.Mode {
		case canon.ToolChoiceNone:
			body["tool_choice"] = "none"
		case canon.ToolChoiceRequired:
			body["tool_choice"] = "required"
		case canon.ToolChoiceNamed:
			body["tool_choice"] = map[string]any{"type": "function", "name": r.ToolChoice.Name}
		}
		if r.ParallelToolCalls != nil {
			body["parallel_tool_calls"] = *r.ParallelToolCalls
		}
	}
	if call.Effort != "" && call.ThinkingStyle() == catalog.ThinkOpenAI {
		effort := call.Effort
		if call.Model != nil && !call.Model.SupportsEffort(effort) {
			effort = nearestEffort(call.Model.Efforts, effort)
		}
		if effort != "" {
			body["reasoning"] = map[string]any{"effort": effort}
		}
	}
	if rf := r.ResponseFormat; rf != nil {
		switch rf.Type {
		case canon.FormatJSONSchema:
			name := rf.Name
			if name == "" {
				name = "response"
			}
			body["text"] = map[string]any{"format": map[string]any{
				"type": "json_schema", "name": name, "schema": rf.Schema, "strict": rf.Strict,
			}}
		case canon.FormatJSONObject:
			body["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
		}
	}
	if caps.Sampling {
		if r.Temperature != nil {
			body["temperature"] = *r.Temperature
		}
		if r.TopP != nil {
			body["top_p"] = *r.TopP
		}
	}
	if call.SessionKey != "" {
		body["prompt_cache_key"] = call.SessionKey
	}
	if r.User != "" {
		body["safety_identifier"] = r.User
	}
	return body
}

func buildInput(r *canon.Request) []any {
	items := make([]any, 0, len(r.Messages)+2)
	for _, m := range r.Messages {
		switch m.Role {
		case canon.RoleSystem, canon.RoleDeveloper:
			items = append(items, map[string]any{"role": m.Role, "content": m.Text()})
		case canon.RoleUser:
			items = append(items, map[string]any{"role": "user", "content": userParts(m.Parts)})
		case canon.RoleAssistant:
			if t := m.Text(); t != "" {
				items = append(items, map[string]any{"role": "assistant", "content": t})
			}
			for _, tc := range m.ToolCalls {
				args := tc.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				items = append(items, map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Name, "arguments": args})
			}
		case canon.RoleTool:
			items = append(items, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Text()})
		}
	}
	return items
}

func userParts(parts []canon.Part) []map[string]any {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case canon.PartText:
			out = append(out, map[string]any{"type": "input_text", "text": p.Text})
		case canon.PartImage:
			url := p.URL
			if url == "" {
				url = "data:" + p.MediaType + ";base64," + p.Data
			}
			img := map[string]any{"type": "input_image", "image_url": url}
			if p.Detail != "" {
				img["detail"] = p.Detail
			}
			out = append(out, img)
		case canon.PartFile:
			f := map[string]any{"type": "input_file"}
			if p.URL != "" {
				f["file_url"] = p.URL
			} else {
				f["file_data"] = "data:" + nz(p.MediaType, "application/pdf") + ";base64," + p.Data
				f["filename"] = nz(p.Filename, "document.pdf")
			}
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"type": "input_text", "text": ""})
	}
	return out
}

type stream struct {
	a       *Adapter
	model   string
	resp    *http.Response
	r       *sse.Reader
	pending []canon.Event
	toolIdx map[int]int // output_index → tool index
	nTools  int
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
				return canon.Event{}, &provider.Error{Provider: s.a.name, Model: s.model, Kind: provider.KindTransport, Message: "stream ended before response.completed"}
			}
			return canon.Event{}, provider.Classify(err, s.a.name, s.model)
		}
		if err := s.handle(e.Data); err != nil {
			return canon.Event{}, err
		}
	}
}

type usage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type respObj struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Usage *usage `json:"usage"`
}

type evt struct {
	Type        string   `json:"type"`
	Delta       string   `json:"delta"`
	OutputIndex int      `json:"output_index"`
	Response    *respObj `json:"response"`
	Item        *struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (s *stream) handle(data []byte) error {
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	var e evt
	if err := json.Unmarshal(data, &e); err != nil {
		return nil // tolerate unknown payloads
	}
	switch e.Type {
	case "response.created":
		if e.Response != nil {
			s.pending = append(s.pending, canon.Event{Kind: canon.EvMeta, ID: e.Response.ID, Model: e.Response.Model})
		}
	case "response.output_text.delta":
		if e.Delta != "" {
			s.pending = append(s.pending, canon.Event{Kind: canon.EvText, Text: e.Delta})
		}
	case "response.refusal.delta":
		if e.Delta != "" {
			s.pending = append(s.pending, canon.Event{Kind: canon.EvRefusal, Text: e.Delta})
		}
	case "response.output_item.added":
		if e.Item != nil && e.Item.Type == "function_call" {
			idx := s.nTools
			s.nTools++
			s.toolIdx[e.OutputIndex] = idx
			s.pending = append(s.pending, canon.Event{Kind: canon.EvToolStart, ToolIndex: idx, ToolID: e.Item.CallID, ToolName: e.Item.Name})
			if e.Item.Arguments != "" {
				s.pending = append(s.pending, canon.Event{Kind: canon.EvToolArgs, ToolIndex: idx, Text: e.Item.Arguments})
			}
		}
	case "response.function_call_arguments.delta":
		if idx, ok := s.toolIdx[e.OutputIndex]; ok && e.Delta != "" {
			s.pending = append(s.pending, canon.Event{Kind: canon.EvToolArgs, ToolIndex: idx, Text: e.Delta})
		}
	case "response.in_progress", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.output_item.done":
		s.pending = append(s.pending, canon.Event{Kind: canon.EvKeepalive})
	case "response.completed", "response.incomplete":
		s.finish(e.Response)
	case "response.failed":
		msg, code := "response failed", ""
		if e.Response != nil && e.Response.Error != nil {
			msg, code = e.Response.Error.Message, e.Response.Error.Code
		}
		return s.fail(code, msg)
	case "error":
		code, msg := e.Code, e.Message
		if e.Error != nil {
			code, msg = nz(e.Error.Code, e.Error.Type), e.Error.Message
		}
		return s.fail(code, msg)
	}
	return nil
}

func (s *stream) fail(code, msg string) error {
	k := provider.KindServer
	switch {
	case strings.Contains(code, "rate_limit"):
		k = provider.KindRateLimit
	case strings.Contains(code, "invalid"), strings.Contains(code, "context_length"):
		k = provider.KindBadRequest
	}
	return &provider.Error{Provider: s.a.name, Model: s.model, Kind: k, Message: strings.TrimSpace(code + " " + msg)}
}

func (s *stream) finish(r *respObj) {
	s.done = true
	reason := canon.FinishStop
	if s.nTools > 0 {
		reason = canon.FinishToolCalls
	}
	if r != nil {
		if r.IncompleteDetails != nil {
			switch r.IncompleteDetails.Reason {
			case "max_output_tokens":
				reason = canon.FinishLength
			case "content_filter":
				reason = canon.FinishContentFilter
			}
		}
		if r.Usage != nil {
			u := &canon.Usage{
				InputTokens:     r.Usage.InputTokens,
				OutputTokens:    r.Usage.OutputTokens,
				CachedTokens:    r.Usage.InputTokensDetails.CachedTokens,
				ReasoningTokens: r.Usage.OutputTokensDetails.ReasoningTokens,
			}
			s.pending = append(s.pending, canon.Event{Kind: canon.EvUsage, Usage: u})
		}
	}
	s.pending = append(s.pending, canon.Event{Kind: canon.EvFinish, FinishReason: reason})
}

func nearestEffort(levels []string, want string) string {
	order := map[string]int{"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}
	w, ok := order[want]
	if !ok || len(levels) == 0 {
		return ""
	}
	best, bestD := "", 99
	for _, l := range levels {
		d := order[l] - w
		if d < 0 {
			d = -d*2 + 1 // prefer rounding up
		}
		if d < bestD {
			best, bestD = l, d
		}
	}
	return best
}

func nz(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

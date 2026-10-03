// Package compat adapts any OpenAI Chat Completions–compatible endpoint
// (OpenRouter, Groq, Together, vLLM, Ollama, …) so extra providers can be
// added from config without code.
package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/provider"
	"github.com/desenyon/modelrouter/internal/provider/sse"
)

// Adapter talks to a /chat/completions endpoint.
type Adapter struct {
	name    string
	baseURL string
	apiKey  string
	headers map[string]string
	client  *http.Client
}

// New builds an adapter.
func New(name, baseURL, apiKey string, headers map[string]string, client *http.Client) *Adapter {
	return &Adapter{name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, headers: headers, client: client}
}

// Name implements provider.Provider.
func (a *Adapter) Name() string { return a.name }

// Open implements provider.Provider.
func (a *Adapter) Open(ctx context.Context, call *provider.Call) (provider.Stream, error) {
	raw, err := json.Marshal(BuildRequest(call))
	if err != nil {
		return nil, &provider.Error{Provider: a.name, Model: call.UpstreamID, Kind: provider.KindBadRequest, Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, provider.Classify(err, a.name, call.UpstreamID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	for k, v := range a.headers {
		req.Header.Set(k, v)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, provider.Classify(err, a.name, call.UpstreamID)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, provider.FromHTTP(a.name, call.UpstreamID, resp)
	}
	return &stream{name: a.name, model: call.UpstreamID, resp: resp, r: sse.NewReader(resp.Body), seen: map[int]bool{}}, nil
}

// BuildRequest renders a canonical call as a Chat Completions body.
func BuildRequest(call *provider.Call) map[string]any {
	r := call.Req
	caps := call.Caps()
	msgs := make([]map[string]any, 0, len(r.Messages))
	for _, m := range r.Messages {
		om := map[string]any{"role": m.Role}
		switch m.Role {
		case canon.RoleTool:
			om["tool_call_id"] = m.ToolCallID
			om["content"] = m.Text()
		case canon.RoleAssistant:
			if t := m.Text(); t != "" || len(m.ToolCalls) == 0 {
				om["content"] = t
			}
			if len(m.ToolCalls) > 0 {
				tcs := make([]map[string]any, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					tcs = append(tcs, map[string]any{"id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Name, "arguments": tc.Arguments}})
				}
				om["tool_calls"] = tcs
			}
		case canon.RoleUser:
			if len(m.Parts) == 1 && m.Parts[0].Type == canon.PartText {
				om["content"] = m.Parts[0].Text
			} else {
				ps := make([]map[string]any, 0, len(m.Parts))
				for _, p := range m.Parts {
					switch p.Type {
					case canon.PartText:
						ps = append(ps, map[string]any{"type": "text", "text": p.Text})
					case canon.PartImage:
						u := p.URL
						if u == "" {
							u = "data:" + p.MediaType + ";base64," + p.Data
						}
						img := map[string]any{"url": u}
						if p.Detail != "" {
							img["detail"] = p.Detail
						}
						ps = append(ps, map[string]any{"type": "image_url", "image_url": img})
					}
				}
				om["content"] = ps
			}
		default:
			om["content"] = m.Text()
		}
		if m.Name != "" && m.Role != canon.RoleTool {
			om["name"] = m.Name
		}
		msgs = append(msgs, om)
	}
	body := map[string]any{
		"model": call.UpstreamID, "messages": msgs, "stream": true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if call.MaxTokens > 0 {
		body["max_tokens"] = call.MaxTokens
	}
	if len(r.Tools) > 0 && caps.Tools {
		tools := make([]map[string]any, 0, len(r.Tools))
		for _, t := range r.Tools {
			fn := map[string]any{"name": t.Name, "description": t.Description}
			if len(t.Parameters) > 0 {
				fn["parameters"] = t.Parameters
			}
			if t.Strict {
				fn["strict"] = true
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		body["tools"] = tools
		switch r.ToolChoice.Mode {
		case canon.ToolChoiceNone, canon.ToolChoiceRequired:
			body["tool_choice"] = r.ToolChoice.Mode
		case canon.ToolChoiceNamed:
			body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": r.ToolChoice.Name}}
		}
		if r.ParallelToolCalls != nil {
			body["parallel_tool_calls"] = *r.ParallelToolCalls
		}
	}
	if rf := r.ResponseFormat; rf != nil {
		switch rf.Type {
		case canon.FormatJSONSchema:
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": nz(rf.Name, "response"), "schema": rf.Schema, "strict": rf.Strict}}
		case canon.FormatJSONObject:
			body["response_format"] = map[string]any{"type": "json_object"}
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
	if len(r.Stop) > 0 {
		body["stop"] = r.Stop
	}
	if r.Seed != nil {
		body["seed"] = *r.Seed
	}
	if call.Effort != "" && call.Model != nil && len(call.Model.Efforts) > 0 {
		body["reasoning_effort"] = call.Effort
	}
	if r.User != "" {
		body["user"] = r.User
	}
	return body
}

type stream struct {
	name    string
	model   string
	resp    *http.Response
	r       *sse.Reader
	pending []canon.Event
	seen    map[int]bool
	meta    bool
	finish  string
	usage   *canon.Usage
	done    bool
}

func (s *stream) Header() http.Header { return s.resp.Header }
func (s *stream) Close() error        { return s.resp.Body.Close() }

type chunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			Refusal   string `json:"refusal"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

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
		if err == io.EOF || (err == nil && bytes.Equal(bytes.TrimSpace(e.Data), []byte("[DONE]"))) {
			s.end()
			continue
		}
		if err != nil {
			return canon.Event{}, provider.Classify(err, s.name, s.model)
		}
		var c chunk
		if json.Unmarshal(e.Data, &c) != nil {
			continue
		}
		if c.Error != nil {
			return canon.Event{}, &provider.Error{Provider: s.name, Model: s.model, Kind: provider.KindServer, Message: c.Error.Message}
		}
		if !s.meta && c.ID != "" {
			s.meta = true
			s.pending = append(s.pending, canon.Event{Kind: canon.EvMeta, ID: c.ID, Model: c.Model})
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != "" {
				s.pending = append(s.pending, canon.Event{Kind: canon.EvText, Text: ch.Delta.Content})
			}
			if ch.Delta.Refusal != "" {
				s.pending = append(s.pending, canon.Event{Kind: canon.EvRefusal, Text: ch.Delta.Refusal})
			}
			for _, tc := range ch.Delta.ToolCalls {
				if !s.seen[tc.Index] {
					s.seen[tc.Index] = true
					s.pending = append(s.pending, canon.Event{Kind: canon.EvToolStart, ToolIndex: tc.Index, ToolID: tc.ID, ToolName: tc.Function.Name})
				}
				if tc.Function.Arguments != "" {
					s.pending = append(s.pending, canon.Event{Kind: canon.EvToolArgs, ToolIndex: tc.Index, Text: tc.Function.Arguments})
				}
			}
			if ch.FinishReason != "" {
				s.finish = ch.FinishReason
			}
			break
		}
		if u := c.Usage; u != nil {
			s.usage = &canon.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
			if u.PromptTokensDetails != nil {
				s.usage.CachedTokens = u.PromptTokensDetails.CachedTokens
			}
			if u.CompletionTokensDetails != nil {
				s.usage.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
			}
		}
	}
}

func (s *stream) end() {
	s.done = true
	if s.usage != nil {
		s.pending = append(s.pending, canon.Event{Kind: canon.EvUsage, Usage: s.usage})
	}
	f := s.finish
	if f == "" || f == "function_call" {
		if len(s.seen) > 0 {
			f = canon.FinishToolCalls
		} else {
			f = canon.FinishStop
		}
	}
	s.pending = append(s.pending, canon.Event{Kind: canon.EvFinish, FinishReason: f})
}

func nz(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

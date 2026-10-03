// Package openai implements the OpenAI Chat Completions wire format as the
// gateway's client-facing API: request decoding into canon.Request and
// response / SSE chunk encoding from canonical events.
package openai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/provider"
)

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Tools               []chatTool      `json:"tools"`
	Functions           []chatFunction  `json:"functions"` // legacy
	ToolChoice          json.RawMessage `json:"tool_choice"`
	FunctionCall        json.RawMessage `json:"function_call"` // legacy
	ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
	ResponseFormat      *responseFormat `json:"response_format"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"`
	Seed                *int64          `json:"seed"`
	N                   int             `json:"n"`
	Stream              bool            `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	ReasoningEffort string `json:"reasoning_effort"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	User        string               `json:"user"`
	OptimizeFor string               `json:"optimize_for"` // legacy alias for router.mode
	Router      *canon.RouterOptions `json:"router"`
}

type chatMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Name         string          `json:"name"`
	ToolCalls    []chatToolCall  `json:"tool_calls"`
	ToolCallID   string          `json:"tool_call_id"`
	FunctionCall *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function_call"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type responseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict bool            `json:"strict"`
	} `json:"json_schema"`
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	} `json:"image_url"`
	File *struct {
		FileData string `json:"file_data"`
		Filename string `json:"filename"`
		FileID   string `json:"file_id"`
	} `json:"file"`
	InputAudio *struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	} `json:"input_audio"`
}

// MaxBodyBytes bounds request bodies.
const MaxBodyBytes = 64 << 20

// Decode parses a Chat Completions request body.
func Decode(r io.Reader) (*canon.Request, error) {
	var cr chatRequest
	dec := json.NewDecoder(io.LimitReader(r, MaxBodyBytes))
	if err := dec.Decode(&cr); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if len(cr.Messages) == 0 {
		return nil, fmt.Errorf("messages must be a non-empty array")
	}
	if cr.N > 1 {
		return nil, fmt.Errorf("n > 1 is not supported by the router; send separate requests")
	}
	req := &canon.Request{
		Model: cr.Model, ParallelToolCalls: cr.ParallelToolCalls,
		Temperature: cr.Temperature, TopP: cr.TopP, Seed: cr.Seed,
		Stream: cr.Stream, User: cr.User,
	}
	if cr.StreamOptions != nil {
		req.StreamUsage = cr.StreamOptions.IncludeUsage
	}
	switch {
	case cr.MaxCompletionTokens > 0:
		req.MaxTokens, req.MaxTokensInclusive = cr.MaxCompletionTokens, true
	case cr.MaxTokens > 0:
		req.MaxTokens = cr.MaxTokens
	}
	req.ReasoningEffort = strings.ToLower(cr.ReasoningEffort)
	if req.ReasoningEffort == "" && cr.Reasoning != nil {
		req.ReasoningEffort = strings.ToLower(cr.Reasoning.Effort)
	}
	if cr.Router != nil {
		req.Router = *cr.Router
	}
	if req.Router.Mode == "" && cr.OptimizeFor != "" {
		req.Router.Mode = cr.OptimizeFor
	}
	stop, err := decodeStop(cr.Stop)
	if err != nil {
		return nil, err
	}
	req.Stop = stop
	for _, t := range cr.Tools {
		if t.Type != "" && t.Type != "function" {
			continue // hosted tools are provider-specific; not routable
		}
		req.Tools = append(req.Tools, canon.Tool{Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters, Strict: t.Function.Strict})
	}
	for _, f := range cr.Functions {
		req.Tools = append(req.Tools, canon.Tool{Name: f.Name, Description: f.Description, Parameters: f.Parameters, Strict: f.Strict})
	}
	tc := cr.ToolChoice
	if len(tc) == 0 {
		tc = cr.FunctionCall
	}
	req.ToolChoice, err = decodeToolChoice(tc)
	if err != nil {
		return nil, err
	}
	if rf := cr.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_schema":
			if rf.JSONSchema == nil {
				return nil, fmt.Errorf("response_format.json_schema is required for type json_schema")
			}
			req.ResponseFormat = &canon.ResponseFormat{Type: canon.FormatJSONSchema, Name: rf.JSONSchema.Name, Schema: rf.JSONSchema.Schema, Strict: rf.JSONSchema.Strict}
		case "json_object":
			req.ResponseFormat = &canon.ResponseFormat{Type: canon.FormatJSONObject}
		}
	}
	for i, m := range cr.Messages {
		cm, err := decodeMessage(m)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		req.Messages = append(req.Messages, cm)
	}
	return req, nil
}

func decodeStop(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, nil
		}
		return []string{s}, nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("stop must be a string or array of strings")
	}
	return arr, nil
}

func decodeToolChoice(raw json.RawMessage) (canon.ToolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return canon.ToolChoice{}, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "":
			return canon.ToolChoice{Mode: canon.ToolChoiceAuto}, nil
		case "none":
			return canon.ToolChoice{Mode: canon.ToolChoiceNone}, nil
		case "required", "any":
			return canon.ToolChoice{Mode: canon.ToolChoiceRequired}, nil
		}
		return canon.ToolChoice{}, fmt.Errorf("unknown tool_choice %q", s)
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return canon.ToolChoice{}, fmt.Errorf("invalid tool_choice")
	}
	name := obj.Name
	if obj.Function != nil {
		name = obj.Function.Name
	}
	if name == "" {
		return canon.ToolChoice{}, fmt.Errorf("tool_choice object needs function.name")
	}
	return canon.ToolChoice{Mode: canon.ToolChoiceNamed, Name: name}, nil
}

func decodeMessage(m chatMessage) (canon.Message, error) {
	role := m.Role
	if role == "function" {
		role = canon.RoleTool
	}
	switch role {
	case canon.RoleSystem, canon.RoleDeveloper, canon.RoleUser, canon.RoleAssistant, canon.RoleTool:
	default:
		return canon.Message{}, fmt.Errorf("unknown role %q", m.Role)
	}
	cm := canon.Message{Role: role, Name: m.Name, ToolCallID: m.ToolCallID}
	if m.Role == "function" && cm.ToolCallID == "" {
		cm.ToolCallID = m.Name
	}
	parts, err := decodeContent(m.Content)
	if err != nil {
		return cm, err
	}
	cm.Parts = parts
	for _, tc := range m.ToolCalls {
		cm.ToolCalls = append(cm.ToolCalls, canon.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	if m.FunctionCall != nil {
		cm.ToolCalls = append(cm.ToolCalls, canon.ToolCall{ID: m.FunctionCall.Name, Name: m.FunctionCall.Name, Arguments: m.FunctionCall.Arguments})
	}
	return cm, nil
}

func decodeContent(raw json.RawMessage) ([]canon.Part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []canon.Part{{Type: canon.PartText, Text: s}}, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content must be a string or array of parts")
	}
	out := make([]canon.Part, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			out = append(out, canon.Part{Type: canon.PartText, Text: p.Text})
		case "image_url":
			if p.ImageURL == nil {
				return nil, fmt.Errorf("image_url part missing image_url")
			}
			out = append(out, imagePart(p.ImageURL.URL, p.ImageURL.Detail))
		case "file":
			if p.File == nil {
				return nil, fmt.Errorf("file part missing file")
			}
			if p.File.FileID != "" {
				return nil, fmt.Errorf("file_id references are provider-specific; send file_data instead")
			}
			mt, data, ok := provider.DataURL(p.File.FileData)
			if !ok {
				mt, data = "application/pdf", p.File.FileData
			}
			out = append(out, canon.Part{Type: canon.PartFile, MediaType: mt, Data: data, Filename: p.File.Filename})
		case "input_audio":
			if p.InputAudio != nil {
				out = append(out, canon.Part{Type: canon.PartAudio, MediaType: "audio/" + p.InputAudio.Format, Data: p.InputAudio.Data})
			}
		case "refusal":
			// assistant history refusal parts carry no routable content
		default:
			return nil, fmt.Errorf("unsupported content part type %q", p.Type)
		}
	}
	return out, nil
}

func imagePart(url, detail string) canon.Part {
	if mt, data, ok := provider.DataURL(url); ok {
		return canon.Part{Type: canon.PartImage, MediaType: mt, Data: data, Detail: detail}
	}
	return canon.Part{Type: canon.PartImage, URL: url, Detail: detail}
}

type outToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type outUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func usage(u canon.Usage) *outUsage {
	o := &outUsage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: u.InputTokens + u.OutputTokens}
	o.PromptTokensDetails.CachedTokens = u.CachedTokens
	o.CompletionTokensDetails.ReasoningTokens = u.ReasoningTokens
	return o
}

// Encode renders a non-streaming chat.completion response.
func Encode(id, model string, resp canon.Response, extra map[string]any) ([]byte, error) {
	msg := map[string]any{"role": "assistant", "content": nil}
	if resp.Content != "" || (len(resp.ToolCalls) == 0 && resp.Refusal == "") {
		msg["content"] = resp.Content
	}
	if resp.Refusal != "" {
		msg["refusal"] = resp.Refusal
	}
	if len(resp.ToolCalls) > 0 {
		tcs := make([]outToolCall, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			tcs[i].ID, tcs[i].Type = tc.ID, "function"
			tcs[i].Function.Name, tcs[i].Function.Arguments = tc.Name, tc.Arguments
		}
		msg["tool_calls"] = tcs
	}
	out := map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": resp.FinishReason, "logprobs": nil}},
		"usage":   usage(resp.Usage),
	}
	for k, v := range extra {
		out[k] = v
	}
	return json.Marshal(out)
}

// StreamWriter renders canonical events as chat.completion.chunk SSE.
type StreamWriter struct {
	w            *bufio.Writer
	flusher      http.Flusher
	id, model    string
	created      int64
	includeUsage bool
	started      bool
	usage        *canon.Usage
	finish       string
	toolSeen     map[int]bool
}

// NewStreamWriter wraps an http.ResponseWriter (headers must be set by caller).
func NewStreamWriter(w http.ResponseWriter, id, model string, includeUsage bool) *StreamWriter {
	f, _ := w.(http.Flusher)
	return &StreamWriter{w: bufio.NewWriterSize(w, 8<<10), flusher: f, id: id, model: model, created: time.Now().Unix(), includeUsage: includeUsage, toolSeen: map[int]bool{}}
}

func (s *StreamWriter) chunk(delta map[string]any, finish any) error {
	c := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish, "logprobs": nil}},
	}
	return s.write(c)
}

func (s *StreamWriter) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.w.WriteString("data: ")
	s.w.Write(b)
	s.w.WriteString("\n\n")
	return nil
}

func (s *StreamWriter) flush() error {
	if err := s.w.Flush(); err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

// Event writes one canonical event.
func (s *StreamWriter) Event(ev canon.Event) error {
	if !s.started && ev.Visible() {
		s.started = true
		if err := s.chunk(map[string]any{"role": "assistant", "content": ""}, nil); err != nil {
			return err
		}
	}
	switch ev.Kind {
	case canon.EvText:
		if err := s.chunk(map[string]any{"content": ev.Text}, nil); err != nil {
			return err
		}
	case canon.EvRefusal:
		if err := s.chunk(map[string]any{"refusal": ev.Text}, nil); err != nil {
			return err
		}
	case canon.EvToolStart:
		idx := ev.ToolIndex
		tc := outToolCall{Index: &idx, ID: ev.ToolID, Type: "function"}
		tc.Function.Name = ev.ToolName
		s.toolSeen[idx] = true
		if err := s.chunk(map[string]any{"tool_calls": []outToolCall{tc}}, nil); err != nil {
			return err
		}
	case canon.EvToolArgs:
		idx := ev.ToolIndex
		tc := outToolCall{Index: &idx}
		tc.Function.Arguments = ev.Text
		if err := s.chunk(map[string]any{"tool_calls": []outToolCall{tc}}, nil); err != nil {
			return err
		}
	case canon.EvFinish:
		s.finish = ev.FinishReason
		return nil
	case canon.EvUsage:
		s.usage = ev.Usage
		return nil
	default:
		return nil
	}
	return s.flush()
}

// Close writes the finish chunk, optional usage chunk, and [DONE].
func (s *StreamWriter) Close() error {
	if !s.started {
		s.chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	}
	f := s.finish
	if f == "" {
		f = canon.FinishStop
	}
	s.chunk(map[string]any{}, f)
	if s.includeUsage && s.usage != nil {
		s.write(map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model, "choices": []any{}, "usage": usage(*s.usage)})
	}
	s.w.WriteString("data: [DONE]\n\n")
	return s.flush()
}

// Error writes an in-stream error event (used after bytes were committed).
func (s *StreamWriter) Error(msg, typ string) error {
	s.write(map[string]any{"error": map[string]any{"message": msg, "type": typ}})
	s.w.WriteString("data: [DONE]\n\n")
	return s.flush()
}

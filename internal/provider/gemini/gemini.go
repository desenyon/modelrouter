// Package gemini is the native Google Gemini adapter for
// models.streamGenerateContent (Gemini API, generativelanguage.googleapis.com).
package gemini

import (
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/provider"
	"github.com/desenyon/modelrouter/internal/provider/sse"
)

// SkipSignature is Google's documented placeholder for function calls whose
// thought signature is unknown (e.g. history produced by another model).
const SkipSignature = "skip_thought_signature_validator"

// Adapter talks to the Gemini API.
type Adapter struct {
	baseURL string
	apiKey  string
	client  *http.Client
	sigs    *sigStore
}

// New builds an adapter. baseURL defaults to https://generativelanguage.googleapis.com.
func New(baseURL, apiKey string, client *http.Client) *Adapter {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	return &Adapter{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, client: client, sigs: newSigStore(100_000)}
}

// Name implements provider.Provider.
func (a *Adapter) Name() string { return "gemini" }

// Open implements provider.Provider.
func (a *Adapter) Open(ctx context.Context, call *provider.Call) (provider.Stream, error) {
	raw, err := json.Marshal(a.BuildRequest(call))
	if err != nil {
		return nil, &provider.Error{Provider: "gemini", Model: call.UpstreamID, Kind: provider.KindBadRequest, Message: err.Error()}
	}
	u := a.baseURL + "/v1beta/models/" + url.PathEscape(call.UpstreamID) + ":streamGenerateContent?alt=sse"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return nil, provider.Classify(err, "gemini", call.UpstreamID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", a.apiKey)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, provider.Classify(err, "gemini", call.UpstreamID)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, provider.FromHTTP("gemini", call.UpstreamID, resp)
	}
	return &stream{a: a, model: call.UpstreamID, resp: resp, r: sse.NewReader(resp.Body)}, nil
}

// BuildRequest translates a canonical call into a generateContent body.
func (a *Adapter) BuildRequest(call *provider.Call) map[string]any {
	r := call.Req
	caps := call.Caps()
	system, contents := a.buildContents(r)
	body := map[string]any{"contents": contents}
	if system != "" {
		body["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": system}}}
	}
	gc := map[string]any{}
	if call.MaxTokens > 0 {
		gc["maxOutputTokens"] = call.MaxTokens
	}
	if caps.Sampling {
		if r.Temperature != nil {
			gc["temperature"] = *r.Temperature
		}
		if r.TopP != nil {
			gc["topP"] = *r.TopP
		}
	}
	if len(r.Stop) > 0 {
		gc["stopSequences"] = r.Stop
	}
	if r.Seed != nil && caps.Seed {
		gc["seed"] = *r.Seed
	}
	if rf := r.ResponseFormat; rf.WantsJSON() {
		gc["responseMimeType"] = "application/json"
		if rf.Type == canon.FormatJSONSchema && len(rf.Schema) > 0 {
			gc["responseJsonSchema"] = rf.Schema
		}
	}
	if tc := thinkingConfig(call); tc != nil {
		gc["thinkingConfig"] = tc
	}
	if len(gc) > 0 {
		body["generationConfig"] = gc
	}
	if len(r.Tools) > 0 && caps.Tools {
		decls := make([]map[string]any, 0, len(r.Tools))
		for _, t := range r.Tools {
			d := map[string]any{"name": t.Name}
			if t.Description != "" {
				d["description"] = t.Description
			}
			if len(t.Parameters) > 0 {
				d["parametersJsonSchema"] = t.Parameters
			}
			decls = append(decls, d)
		}
		body["tools"] = []map[string]any{{"functionDeclarations": decls}}
		fc := map[string]any{"mode": "AUTO"}
		switch r.ToolChoice.Mode {
		case canon.ToolChoiceNone:
			fc["mode"] = "NONE"
		case canon.ToolChoiceRequired:
			fc["mode"] = "ANY"
		case canon.ToolChoiceNamed:
			fc["mode"] = "ANY"
			fc["allowedFunctionNames"] = []string{r.ToolChoice.Name}
		}
		body["toolConfig"] = map[string]any{"functionCallingConfig": fc}
	}
	return body
}

func thinkingConfig(call *provider.Call) map[string]any {
	e := call.Effort
	if e == "" {
		return nil
	}
	switch call.ThinkingStyle() {
	case catalog.ThinkGeminiLevel:
		lvl := e
		switch e {
		case "none", "minimal":
			lvl = "low"
		case "xhigh", "max":
			lvl = "high"
		}
		if call.Model != nil && !call.Model.SupportsEffort(lvl) {
			if lvl == "medium" {
				lvl = "high"
			} else {
				lvl = "low"
			}
		}
		return map[string]any{"thinkingLevel": lvl}
	case catalog.ThinkGeminiBudget:
		budget := map[string]int{"none": 0, "minimal": 512, "low": 1024, "medium": 4096, "high": 16384, "xhigh": 24576, "max": 32768}[e]
		if budget == 0 && call.Model != nil && !call.Model.SupportsEffort("none") {
			budget = 128 // e.g. 2.5 Pro cannot disable thinking
		}
		return map[string]any{"thinkingBudget": budget}
	}
	return nil
}

func (a *Adapter) buildContents(r *canon.Request) (string, []map[string]any) {
	var system strings.Builder
	var contents []map[string]any
	push := func(role string, parts ...map[string]any) {
		if len(parts) == 0 {
			return
		}
		if n := len(contents); n > 0 && contents[n-1]["role"] == role {
			contents[n-1]["parts"] = append(contents[n-1]["parts"].([]map[string]any), parts...)
			return
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
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
			push("user", parts(m.Parts)...)
		case canon.RoleAssistant:
			var ps []map[string]any
			if t := m.Text(); t != "" {
				ps = append(ps, map[string]any{"text": t})
			}
			for i, tc := range m.ToolCalls {
				p := map[string]any{"functionCall": map[string]any{"name": tc.Name, "args": provider.ParseArgs(tc.Arguments)}}
				// Gemini 3 validates the signature on the first call of a turn.
				if sig, ok := a.sigs.get(tc.ID); ok {
					p["thoughtSignature"] = sig
				} else if i == 0 {
					p["thoughtSignature"] = SkipSignature
				}
				ps = append(ps, p)
			}
			push("model", ps...)
		case canon.RoleTool:
			name := m.Name
			if name == "" {
				name = r.ToolNameByID(m.ToolCallID)
			}
			if name == "" {
				name = "tool"
			}
			push("user", map[string]any{"functionResponse": map[string]any{"name": name, "response": toolResponse(m.Text())}})
		}
	}
	if len(contents) == 0 {
		contents = append(contents, map[string]any{"role": "user", "parts": []map[string]any{{"text": ""}}})
	}
	return system.String(), contents
}

func toolResponse(s string) any {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) == nil {
		return obj
	}
	return map[string]any{"result": s}
}

func parts(ps []canon.Part) []map[string]any {
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		switch p.Type {
		case canon.PartText:
			out = append(out, map[string]any{"text": p.Text})
		case canon.PartImage, canon.PartFile, canon.PartAudio:
			if p.Data != "" {
				mt := p.MediaType
				if mt == "" && p.Type == canon.PartFile {
					mt = "application/pdf"
				}
				out = append(out, map[string]any{"inlineData": map[string]any{"mimeType": mt, "data": p.Data}})
			} else if p.URL != "" {
				out = append(out, map[string]any{"fileData": map[string]any{"fileUri": p.URL, "mimeType": guessMime(p.URL, p.Type)}})
			}
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"text": ""})
	}
	return out
}

func guessMime(u, typ string) string {
	l := strings.ToLower(u)
	switch {
	case strings.HasSuffix(l, ".png"):
		return "image/png"
	case strings.HasSuffix(l, ".webp"):
		return "image/webp"
	case strings.HasSuffix(l, ".gif"):
		return "image/gif"
	case strings.HasSuffix(l, ".pdf"):
		return "application/pdf"
	}
	if typ == canon.PartFile {
		return "application/pdf"
	}
	return "image/jpeg"
}

type stream struct {
	a       *Adapter
	model   string
	resp    *http.Response
	r       *sse.Reader
	pending []canon.Event
	nTools  int
	meta    bool
	finish  string
	usage   *canon.Usage
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
		if err == io.EOF {
			s.end()
			continue
		}
		if err != nil {
			return canon.Event{}, provider.Classify(err, "gemini", s.model)
		}
		if err := s.handle(e.Data); err != nil {
			return canon.Event{}, err
		}
	}
}

type gResp struct {
	ResponseID   string `json:"responseId"`
	ModelVersion string `json:"modelVersion"`
	Candidates   []struct {
		Content struct {
			Parts []struct {
				Text             string `json:"text"`
				Thought          bool   `json:"thought"`
				ThoughtSignature string `json:"thoughtSignature"`
				FunctionCall     *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func (s *stream) handle(data []byte) error {
	var g gResp
	if err := json.Unmarshal(data, &g); err != nil {
		return nil
	}
	if g.Error != nil {
		k := provider.KindServer
		switch {
		case g.Error.Code == 429 || g.Error.Status == "RESOURCE_EXHAUSTED":
			k = provider.KindRateLimit
		case g.Error.Code == 400 || g.Error.Status == "INVALID_ARGUMENT":
			k = provider.KindBadRequest
		}
		return &provider.Error{Provider: "gemini", Model: s.model, Kind: k, Status: g.Error.Code, Message: g.Error.Message}
	}
	if !s.meta && (g.ResponseID != "" || g.ModelVersion != "") {
		s.meta = true
		s.pending = append(s.pending, canon.Event{Kind: canon.EvMeta, ID: g.ResponseID, Model: g.ModelVersion})
	}
	if g.PromptFeedback != nil && g.PromptFeedback.BlockReason != "" {
		s.finish = canon.FinishContentFilter
	}
	for _, c := range g.Candidates {
		for _, p := range c.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				id := "call_" + randHex(12)
				if p.ThoughtSignature != "" {
					s.a.sigs.put(id, p.ThoughtSignature)
				}
				idx := s.nTools
				s.nTools++
				args := string(p.FunctionCall.Args)
				if args == "" || args == "null" {
					args = "{}"
				}
				s.pending = append(s.pending,
					canon.Event{Kind: canon.EvToolStart, ToolIndex: idx, ToolID: id, ToolName: p.FunctionCall.Name},
					canon.Event{Kind: canon.EvToolArgs, ToolIndex: idx, Text: args})
			case p.Thought:
				s.pending = append(s.pending, canon.Event{Kind: canon.EvKeepalive})
			case p.Text != "":
				s.pending = append(s.pending, canon.Event{Kind: canon.EvText, Text: p.Text})
			}
		}
		if c.FinishReason != "" && c.FinishReason != "FINISH_REASON_UNSPECIFIED" {
			s.finish = mapFinish(c.FinishReason)
		}
		break // candidateCount is always 1
	}
	if um := g.UsageMetadata; um != nil {
		s.usage = &canon.Usage{
			InputTokens:     um.PromptTokenCount,
			CachedTokens:    um.CachedContentTokenCount,
			OutputTokens:    um.CandidatesTokenCount + um.ThoughtsTokenCount,
			ReasoningTokens: um.ThoughtsTokenCount,
		}
	}
	return nil
}

func (s *stream) end() {
	s.done = true
	reason := s.finish
	if reason == "" || (reason == canon.FinishStop && s.nTools > 0) {
		if s.nTools > 0 {
			reason = canon.FinishToolCalls
		} else {
			reason = canon.FinishStop
		}
	}
	if s.usage != nil {
		s.pending = append(s.pending, canon.Event{Kind: canon.EvUsage, Usage: s.usage})
	}
	s.pending = append(s.pending, canon.Event{Kind: canon.EvFinish, FinishReason: reason})
}

func mapFinish(r string) string {
	switch r {
	case "STOP":
		return canon.FinishStop
	case "MAX_TOKENS":
		return canon.FinishLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return canon.FinishContentFilter
	}
	return canon.FinishStop
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sigStore remembers thought signatures by tool-call id so multi-turn
// function calling on Gemini 3 round-trips through OpenAI-format clients.
type sigStore struct {
	mu  sync.Mutex
	cap int
	ll  *list.List
	m   map[string]*list.Element
}

type sigEntry struct{ id, sig string }

func newSigStore(capacity int) *sigStore {
	return &sigStore{cap: capacity, ll: list.New(), m: make(map[string]*list.Element)}
}

func (s *sigStore) put(id, sig string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.m[id]; ok {
		el.Value.(*sigEntry).sig = sig
		s.ll.MoveToFront(el)
		return
	}
	s.m[id] = s.ll.PushFront(&sigEntry{id, sig})
	for s.ll.Len() > s.cap {
		old := s.ll.Back()
		s.ll.Remove(old)
		delete(s.m, old.Value.(*sigEntry).id)
	}
}

func (s *sigStore) get(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.m[id]; ok {
		return el.Value.(*sigEntry).sig, true
	}
	return "", false
}

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/config"
	"github.com/desenyon/modelrouter/internal/embed"
	"github.com/desenyon/modelrouter/internal/provider"
)

type echoProv struct {
	name  string
	calls atomic.Int32
}

func (p *echoProv) Name() string { return p.name }
func (p *echoProv) Open(_ context.Context, c *provider.Call) (provider.Stream, error) {
	p.calls.Add(1)
	text := "answer from " + c.UpstreamID
	if c.Req.ResponseFormat.WantsJSON() {
		text = `{"model":"` + c.UpstreamID + `"}`
	}
	evs := []canon.Event{{Kind: canon.EvMeta, ID: "up1", Model: c.UpstreamID}, {Kind: canon.EvText, Text: text[:5]}, {Kind: canon.EvText, Text: text[5:]}}
	if len(c.Req.Tools) > 0 {
		evs = append(evs, canon.Event{Kind: canon.EvToolStart, ToolIndex: 0, ToolID: "call_x", ToolName: c.Req.Tools[0].Name}, canon.Event{Kind: canon.EvToolArgs, ToolIndex: 0, Text: `{"a":1}`})
	}
	evs = append(evs, canon.Event{Kind: canon.EvFinish, FinishReason: "stop"}, canon.Event{Kind: canon.EvUsage, Usage: &canon.Usage{InputTokens: 12, OutputTokens: 7}})
	return &listStream{evs: evs}, nil
}

type listStream struct {
	evs []canon.Event
	i   int
}

func (s *listStream) Header() http.Header { return http.Header{} }
func (s *listStream) Close() error        { return nil }
func (s *listStream) Next() (canon.Event, error) {
	if s.i >= len(s.evs) {
		return canon.Event{}, io.EOF
	}
	s.i++
	return s.evs[s.i-1], nil
}

func newTestServer(t *testing.T, mutate func(*config.Config)) (*httptest.Server, map[string]*echoProv) {
	t.Helper()
	dir := os.Getenv("MODELROUTER_EMBEDDER_DIR")
	if dir == "" {
		dir = embed.DefaultDir()
	}
	if err := embed.Present(dir); err != nil {
		if os.Getenv("MODELROUTER_REQUIRE_EMBEDDER") == "1" {
			t.Fatalf("required embedder not available: %v", err)
		}
		t.Skipf("embedder not available: %v", err)
	}
	em, err := embed.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Learning.StatePath = filepath.Join(t.TempDir(), "state.json")
	if mutate != nil {
		mutate(&cfg)
	}
	provs := map[string]*echoProv{"openai": {name: "openai"}, "anthropic": {name: "anthropic"}, "gemini": {name: "gemini"}}
	pm := map[string]provider.Provider{}
	for k, v := range provs {
		pm[k] = v
	}
	s, err := New(context.Background(), cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Embedder: em, Providers: pm})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(s.Close)
	t.Cleanup(ts.Close)
	return ts, provs
}

func post(t *testing.T, url, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestChatNonStream(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := post(t, ts.URL+"/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || !strings.HasPrefix(out.ID, "chatcmpl-") || out.Choices[0].Message.Content != "answer from "+out.Model || out.Usage.TotalTokens != 19 {
		t.Fatalf("bad response: %+v", out)
	}
	if resp.Header.Get("X-Modelrouter-Tier") != "luna" {
		t.Fatalf("'hi' should route to luna, got %s (%s)", resp.Header.Get("X-Modelrouter-Tier"), resp.Header.Get("X-Modelrouter-Model"))
	}
	for _, h := range []string{"X-Modelrouter-Model", "X-Modelrouter-Provider", "X-Modelrouter-Difficulty", "X-Modelrouter-P-Success", "X-Modelrouter-Cost", "X-Modelrouter-Request-Id"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
}

func TestChatStreamFormat(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := post(t, ts.URL+"/v1/chat/completions", `{"model":"auto","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"call the tool"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`, nil)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var chunks []map[string]any
	done := false
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad chunk %q", data)
		}
		chunks = append(chunks, c)
	}
	if !done || len(chunks) < 5 {
		t.Fatalf("stream incomplete: done=%v chunks=%d", done, len(chunks))
	}
	first := chunks[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if first["role"] != "assistant" || chunks[0]["object"] != "chat.completion.chunk" {
		t.Fatalf("first chunk must carry role: %v", chunks[0])
	}
	var text strings.Builder
	sawTool, sawFinish := false, false
	for _, c := range chunks {
		ch := c["choices"].([]any)
		if len(ch) == 0 {
			if c["usage"] == nil {
				t.Fatal("usage chunk missing usage")
			}
			continue
		}
		d := ch[0].(map[string]any)["delta"].(map[string]any)
		if s, ok := d["content"].(string); ok {
			text.WriteString(s)
		}
		if tcs, ok := d["tool_calls"].([]any); ok && tcs[0].(map[string]any)["id"] == "call_x" {
			sawTool = true
		}
		if ch[0].(map[string]any)["finish_reason"] != nil {
			sawFinish = true
		}
	}
	if !strings.HasPrefix(text.String(), "answer from ") || !sawTool || !sawFinish {
		t.Fatalf("text=%q tool=%v finish=%v", text.String(), sawTool, sawFinish)
	}
}

func TestDeterministicRequestsAreCached(t *testing.T) {
	ts, provs := newTestServer(t, nil)
	body := `{"model":"auto","temperature":0,"messages":[{"role":"user","content":"what is 2+2"}]}`
	r1 := post(t, ts.URL+"/v1/chat/completions", body, nil)
	r1.Body.Close()
	r2 := post(t, ts.URL+"/v1/chat/completions", body, nil)
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r1.Header.Get("X-Modelrouter-Cache") != "MISS" || r2.Header.Get("X-Modelrouter-Cache") != "HIT" {
		t.Fatalf("cache headers: %s then %s", r1.Header.Get("X-Modelrouter-Cache"), r2.Header.Get("X-Modelrouter-Cache"))
	}
	total := 0
	for _, p := range provs {
		total += int(p.calls.Load())
	}
	if total != 1 || !strings.Contains(string(b2), "answer from") {
		t.Fatalf("expected exactly one upstream call, got %d", total)
	}
	r3 := post(t, ts.URL+"/v1/chat/completions", body, map[string]string{"Cache-Control": "no-cache"})
	r3.Body.Close()
	if r3.Header.Get("X-Modelrouter-Cache") == "HIT" {
		t.Fatal("Cache-Control: no-cache must bypass the cache")
	}
}

func TestExplicitModelAndTierPins(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	r := post(t, ts.URL+"/v1/chat/completions", `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`, nil)
	r.Body.Close()
	if r.Header.Get("X-Modelrouter-Model") != "anthropic/claude-opus-5-5" {
		t.Fatalf("explicit model not honored: %s", r.Header.Get("X-Modelrouter-Model"))
	}
	r = post(t, ts.URL+"/v1/chat/completions", `{"model":"astra","messages":[{"role":"user","content":"hi"}]}`, nil)
	r.Body.Close()
	if r.Header.Get("X-Modelrouter-Tier") != "astra" {
		t.Fatalf("tier pin not honored: %s", r.Header.Get("X-Modelrouter-Tier"))
	}
	r = post(t, ts.URL+"/v1/chat/completions", `{"model":"gemini/auto","messages":[{"role":"user","content":"hi"}]}`, nil)
	r.Body.Close()
	if r.Header.Get("X-Modelrouter-Provider") != "gemini" {
		t.Fatalf("provider pin not honored: %s", r.Header.Get("X-Modelrouter-Provider"))
	}
	r = post(t, ts.URL+"/v1/chat/completions", `{"model":"gpt-9-imaginary","messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || r.Header.Get("X-Modelrouter-Provider") != "openai" || !strings.Contains(string(b), "gpt-9-imaginary") {
		t.Fatalf("uncatalogued gpt-* id should pass through to openai: %d %s", r.StatusCode, b)
	}
}

func TestFeedbackEndpoint(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	r := post(t, ts.URL+"/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"write a limerick about routers"}]}`, nil)
	r.Body.Close()
	id := r.Header.Get("X-Modelrouter-Request-Id")
	fb := post(t, ts.URL+"/v1/feedback", `{"request_id":"chatcmpl-`+id+`","rating":"bad"}`, nil)
	fb.Body.Close()
	if fb.StatusCode != 200 {
		t.Fatalf("feedback status %d", fb.StatusCode)
	}
	fb = post(t, ts.URL+"/v1/feedback", `{"request_id":"nope","score":1}`, nil)
	fb.Body.Close()
	if fb.StatusCode != 404 {
		t.Fatalf("unknown id should 404, got %d", fb.StatusCode)
	}
}

func TestAuthAndModels(t *testing.T) {
	ts, _ := newTestServer(t, func(c *config.Config) { c.APIKeys = []string{"secret"} })
	r := post(t, ts.URL+"/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`, nil)
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatalf("want 401, got %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	ids := map[string]bool{}
	for _, d := range list.Data {
		ids[d.ID] = true
	}
	for _, want := range []string{"auto", "auto:quality", "luna", "astra", "anthropic/claude-sonnet-5-5", "openai/gpt-6-luna", "gemini/gemini-3.8-flash"} {
		if !ids[want] {
			t.Errorf("/v1/models missing %s", want)
		}
	}
}

func TestBadRequests(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	for _, body := range []string{`{`, `{"model":"auto","messages":[]}`, `{"model":"auto","n":3,"messages":[{"role":"user","content":"x"}]}`, `{"model":"auto","messages":[{"role":"wizard","content":"x"}]}`} {
		r := post(t, ts.URL+"/v1/chat/completions", body, nil)
		r.Body.Close()
		if r.StatusCode != 400 {
			t.Errorf("%s → %d, want 400", body, r.StatusCode)
		}
	}
}

func TestMetricsExposition(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	r := post(t, ts.URL+"/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`, nil)
	r.Body.Close()
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"modelrouter_requests_total{", "modelrouter_routing_seconds_bucket", "modelrouter_budget_lambda 1"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

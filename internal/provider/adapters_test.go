package provider_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/provider"
	"github.com/desenyon/modelrouter/internal/provider/anthropic"
	"github.com/desenyon/modelrouter/internal/provider/compat"
	"github.com/desenyon/modelrouter/internal/provider/gemini"
	"github.com/desenyon/modelrouter/internal/provider/openai"
)

type captured struct {
	path   string
	header http.Header
	body   map[string]any
}

func server(t *testing.T, status int, hdr map[string]string, sse string, cap *captured) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if cap != nil {
			cap.path = r.URL.String()
			cap.header = r.Header.Clone()
			_ = json.Unmarshal(b, &cap.body)
		}
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		if status != 200 {
			w.WriteHeader(status)
			io.WriteString(w, sse)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
}

func toolReq() *canon.Request {
	f := false
	return &canon.Request{
		Messages: []canon.Message{
			{Role: "system", Parts: []canon.Part{{Type: "text", Text: "be terse"}}},
			{Role: "user", Parts: []canon.Part{{Type: "text", Text: "weather in Paris?"}, {Type: "image", MediaType: "image/png", Data: "AAAA"}}},
			{Role: "assistant", ToolCalls: []canon.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"Paris"}`}}},
			{Role: "tool", ToolCallID: "call_1", Parts: []canon.Part{{Type: "text", Text: `{"temp":21}`}}},
			{Role: "user", Parts: []canon.Part{{Type: "text", Text: "and London?"}}},
		},
		Tools:             []canon.Tool{{Name: "get_weather", Description: "weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string","minLength":1}},"required":["city"]}`)}},
		ToolChoice:        canon.ToolChoice{Mode: canon.ToolChoiceRequired},
		ParallelToolCalls: &f,
		Stop:              []string{"END"},
	}
}

func drain(t *testing.T, st provider.Stream) canon.Response {
	t.Helper()
	var a canon.Assembler
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		a.Add(ev)
	}
	return a.Response()
}

func model(id string) *catalog.Model {
	cat, _ := catalog.New(catalog.Builtin())
	m, ok := cat.Lookup(id)
	if !ok {
		panic(id)
	}
	return m
}

// ─── OpenAI Responses API ───

const openaiSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-6-luna"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_9","name":"get_weather","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"city\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"London\"}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":100},"output_tokens":40,"output_tokens_details":{"reasoning_tokens":25}}}}

`

func TestOpenAIResponsesTranslation(t *testing.T) {
	var cap captured
	srv := server(t, 200, nil, openaiSSE, &cap)
	defer srv.Close()
	a := openai.New("openai", srv.URL, "sk-test", "", srv.Client())
	st, err := a.Open(context.Background(), &provider.Call{Req: toolReq(), Model: model("gpt-6-luna"), UpstreamID: "gpt-6-luna", Effort: "low", MaxTokens: 900, SessionKey: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	r := drain(t, st)
	if cap.path != "/responses" || cap.header.Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("bad request target: %s %v", cap.path, cap.header)
	}
	b := cap.body
	if b["model"] != "gpt-6-luna" || b["stream"] != true || b["store"] != false || b["max_output_tokens"] != float64(900) || b["prompt_cache_key"] != "s1" {
		t.Fatalf("bad body: %v", b)
	}
	if b["reasoning"].(map[string]any)["effort"] != "low" || b["tool_choice"] != "required" || b["parallel_tool_calls"] != false {
		t.Fatalf("bad controls: %v", b)
	}
	tool := b["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "get_weather" || tool["strict"] != false {
		t.Fatalf("bad tool: %v", tool)
	}
	items := b["input"].([]any)
	kinds := []string{}
	for _, it := range items {
		m := it.(map[string]any)
		if ty, ok := m["type"].(string); ok {
			kinds = append(kinds, ty)
		} else {
			kinds = append(kinds, m["role"].(string))
		}
	}
	if strings.Join(kinds, ",") != "system,user,function_call,function_call_output,user" {
		t.Fatalf("bad input items: %v", kinds)
	}
	img := items[1].(map[string]any)["content"].([]any)[1].(map[string]any)
	if img["type"] != "input_image" || img["image_url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("bad image: %v", img)
	}
	if _, has := b["temperature"]; has {
		t.Fatal("GPT-6 rejects sampling params; must not be sent")
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].ID != "call_9" || r.ToolCalls[0].Arguments != `{"city":"London"}` || r.FinishReason != "tool_calls" {
		t.Fatalf("bad parse: %+v", r)
	}
	if r.Usage.InputTokens != 120 || r.Usage.CachedTokens != 100 || r.Usage.ReasoningTokens != 25 || r.ID != "resp_1" {
		t.Fatalf("bad usage: %+v", r.Usage)
	}
}

func TestOpenAIRateLimitClassified(t *testing.T) {
	srv := server(t, 429, map[string]string{"Retry-After": "7"}, `{"error":{"message":"slow down","type":"rate_limit_error"}}`, nil)
	defer srv.Close()
	a := openai.New("openai", srv.URL, "k", "", srv.Client())
	_, err := a.Open(context.Background(), &provider.Call{Req: toolReq(), Model: model("gpt-6-luna"), UpstreamID: "gpt-6-luna", MaxTokens: 10})
	pe := provider.AsError(err, "", "")
	if pe.Kind != provider.KindRateLimit || pe.RetryAfter != 7*time.Second || !strings.Contains(pe.Message, "slow down") {
		t.Fatalf("got %+v", pe)
	}
}

// ─── Anthropic Messages API ───

const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-haiku-4-5","usage":{"input_tokens":20,"cache_read_input_tokens":80,"cache_creation_input_tokens":5,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Checking."}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\": \"London\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: ping
data: {"type":"ping"}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":33}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicTranslation(t *testing.T) {
	var cap captured
	srv := server(t, 200, nil, anthropicSSE, &cap)
	defer srv.Close()
	a := anthropic.New(srv.URL, "ak", srv.Client(), true)
	st, err := a.Open(context.Background(), &provider.Call{Req: toolReq(), Model: model("claude-haiku-4-5"), UpstreamID: "claude-haiku-4-5", Effort: "none", MaxTokens: 1000, CacheHint: true})
	if err != nil {
		t.Fatal(err)
	}
	r := drain(t, st)
	h := cap.header
	if cap.path != "/v1/messages" || h.Get("x-api-key") != "ak" || h.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("bad target/headers: %s %v", cap.path, h)
	}
	if h.Get("anthropic-beta") != "" {
		t.Fatal("haiku does not get server-side fallbacks")
	}
	b := cap.body
	if b["system"] != "be terse" || b["max_tokens"] != float64(1000) || b["cache_control"] == nil {
		t.Fatalf("bad top-level: %v", b)
	}
	msgs := b["messages"].([]any)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	// user, assistant(tool_use), user(tool_result + text merged)
	if strings.Join(roles, ",") != "user,assistant,user" {
		t.Fatalf("roles must alternate with tool_result merged: %v", roles)
	}
	last := msgs[2].(map[string]any)["content"].([]any)
	if last[0].(map[string]any)["type"] != "tool_result" || last[1].(map[string]any)["text"] != "and London?" {
		t.Fatalf("bad merge: %v", last)
	}
	tu := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["input"].(map[string]any)["city"] != "Paris" {
		t.Fatalf("bad tool_use: %v", tu)
	}
	tc := b["tool_choice"].(map[string]any)
	if tc["type"] != "any" || tc["disable_parallel_tool_use"] != true {
		t.Fatalf("haiku supports forced tool choice: %v", tc)
	}
	if b["stop_sequences"].([]any)[0] != "END" {
		t.Fatal("stop sequences dropped")
	}
	if r.Content != "Checking." || len(r.ToolCalls) != 1 || r.ToolCalls[0].Arguments != `{"city": "London"}` || r.FinishReason != "tool_calls" {
		t.Fatalf("bad parse: %+v", r)
	}
	if r.Usage.InputTokens != 105 || r.Usage.CachedTokens != 80 || r.Usage.CacheWriteTokens != 5 || r.Usage.OutputTokens != 33 {
		t.Fatalf("bad usage: %+v", r.Usage)
	}
}

func TestAnthropicOpusRules(t *testing.T) {
	var cap captured
	srv := server(t, 200, nil, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", &cap)
	defer srv.Close()
	req := toolReq()
	temp := 0.2
	req.Temperature = &temp
	req.ResponseFormat = &canon.ResponseFormat{Type: canon.FormatJSONSchema, Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer","minimum":0}}}`)}
	a := anthropic.New(srv.URL, "ak", srv.Client(), true)
	st, err := a.Open(context.Background(), &provider.Call{Req: req, Model: model("claude-opus-5-5"), UpstreamID: "claude-opus-5-5", Effort: "high", MaxTokens: 4000})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, st)
	b := cap.body
	if b["tool_choice"].(map[string]any)["type"] != "auto" {
		t.Fatal("Opus 5.5 rejects forced tool_choice; must downgrade to auto")
	}
	if _, has := b["temperature"]; has {
		t.Fatal("Opus 5.5 rejects sampling params")
	}
	if _, has := b["thinking"]; has {
		t.Fatal("thinking must be omitted on Opus 5.5 (always adaptive)")
	}
	oc := b["output_config"].(map[string]any)
	if oc["effort"] != "high" {
		t.Fatalf("effort missing: %v", oc)
	}
	schema := oc["format"].(map[string]any)["schema"].(map[string]any)
	if schema["additionalProperties"] != false || schema["properties"].(map[string]any)["n"].(map[string]any)["minimum"] != nil {
		t.Fatalf("schema not sanitized: %v", schema)
	}
	if b["fallbacks"] != "default" || cap.header.Get("anthropic-beta") != "server-side-fallback-2026-07-01" {
		t.Fatal("server-side refusal fallback should be enabled for Opus 5.5")
	}
}

func TestAnthropicOverloadedIsServerError(t *testing.T) {
	srv := server(t, 529, nil, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, nil)
	defer srv.Close()
	a := anthropic.New(srv.URL, "ak", srv.Client(), false)
	_, err := a.Open(context.Background(), &provider.Call{Req: toolReq(), Model: model("claude-sonnet-5-5"), UpstreamID: "claude-sonnet-5-5", MaxTokens: 10})
	if pe := provider.AsError(err, "", ""); pe.Kind != provider.KindServer {
		t.Fatalf("529 should be retryable server error, got %v", pe.Kind)
	}
}

// ─── Gemini generateContent ───

const geminiSSE = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"thinking…","thought":true}]}}],"responseId":"g1","modelVersion":"gemini-3.8-flash"}

data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"London"}},"thoughtSignature":"SIG123"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":10,"cachedContentTokenCount":30,"thoughtsTokenCount":40}}

`

func TestGeminiTranslationAndSignatureRoundTrip(t *testing.T) {
	var cap captured
	srv := server(t, 200, nil, geminiSSE, &cap)
	defer srv.Close()
	a := gemini.New(srv.URL, "gk", srv.Client())
	m := model("gemini-3.8-flash")
	st, err := a.Open(context.Background(), &provider.Call{Req: toolReq(), Model: m, UpstreamID: m.UpstreamID, Effort: "medium", MaxTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	r := drain(t, st)
	if !strings.HasPrefix(cap.path, "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse") || cap.header.Get("x-goog-api-key") != "gk" {
		t.Fatalf("bad target: %s", cap.path)
	}
	b := cap.body
	gc := b["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(2000) || gc["thinkingConfig"].(map[string]any)["thinkingLevel"] != "medium" {
		t.Fatalf("bad generationConfig: %v", gc)
	}
	if b["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"] != "ANY" {
		t.Fatal("required tool choice → ANY")
	}
	contents := b["contents"].([]any)
	roles := []string{}
	for _, c := range contents {
		roles = append(roles, c.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "user,model,user" {
		t.Fatalf("bad roles: %v", roles)
	}
	fc := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if fc["thoughtSignature"] != gemini.SkipSignature {
		t.Fatalf("history function call from another model needs the skip signature: %v", fc)
	}
	fr := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "get_weather" || fr["response"].(map[string]any)["temp"] != float64(21) {
		t.Fatalf("bad functionResponse: %v", fr)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Arguments != `{"city":"London"}` || r.FinishReason != "tool_calls" {
		t.Fatalf("bad parse: %+v", r)
	}
	if r.Usage.InputTokens != 50 || r.Usage.CachedTokens != 30 || r.Usage.OutputTokens != 50 || r.Usage.ReasoningTokens != 40 {
		t.Fatalf("bad usage: %+v", r.Usage)
	}
	// Second turn: the signature Gemini returned must be replayed for that call id.
	req2 := toolReq()
	req2.Messages[2].ToolCalls[0].ID = r.ToolCalls[0].ID
	req2.Messages[3].ToolCallID = r.ToolCalls[0].ID
	body := a.BuildRequest(&provider.Call{Req: req2, Model: m, UpstreamID: m.UpstreamID, MaxTokens: 10})
	parts := body["contents"].([]map[string]any)[1]["parts"].([]map[string]any)
	if parts[0]["thoughtSignature"] != "SIG123" {
		t.Fatalf("signature not round-tripped: %v", parts[0])
	}
}

func TestGeminiRetryDelayParsed(t *testing.T) {
	srv := server(t, 429, nil, `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"12s"}]}}`, nil)
	defer srv.Close()
	a := gemini.New(srv.URL, "gk", srv.Client())
	_, err := a.Open(context.Background(), &provider.Call{Req: toolReq(), Model: model("gemini-3.8-flash"), UpstreamID: "gemini-3.8-flash", MaxTokens: 10})
	pe := provider.AsError(err, "", "")
	if pe.Kind != provider.KindRateLimit || pe.RetryAfter != 12*time.Second {
		t.Fatalf("got %+v", pe)
	}
}

// ─── OpenAI-compatible Chat Completions ───

const compatSSE = `data: {"id":"c1","model":"llama","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":"lo"}}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"c1","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2}}

data: [DONE]

`

func TestCompatTranslation(t *testing.T) {
	var cap captured
	srv := server(t, 200, nil, compatSSE, &cap)
	defer srv.Close()
	a := compat.New("local", srv.URL, "", map[string]string{"X-Title": "mr"}, srv.Client())
	st, err := a.Open(context.Background(), &provider.Call{Req: toolReq(), UpstreamID: "llama", MaxTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	r := drain(t, st)
	if cap.path != "/chat/completions" || cap.header.Get("X-Title") != "mr" || cap.body["stream_options"] == nil {
		t.Fatalf("bad request: %s %v", cap.path, cap.body)
	}
	if r.Content != "Hello" || r.Usage.InputTokens != 9 || r.FinishReason != "stop" {
		t.Fatalf("bad parse: %+v", r)
	}
}

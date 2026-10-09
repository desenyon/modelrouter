package gateway

import (
	"context"
	"encoding/json"
	"github.com/desenyon/modelrouter/internal/telemetry"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiopenai "github.com/desenyon/modelrouter/internal/api/openai"
	"github.com/desenyon/modelrouter/internal/config"
	"github.com/desenyon/modelrouter/internal/embed"
	"github.com/desenyon/modelrouter/internal/provider"
)

// syntheticEmbedder exercises the real predictor/router/gateway without network
// downloads. Its vectors are deliberately not used to assert model quality.
type syntheticEmbedder struct{}

func (syntheticEmbedder) Name() string { return "gateway-test-only" }
func (syntheticEmbedder) Dim() int     { return 8 }
func (e syntheticEmbedder) EncodeInto(s string, out []float32) int {
	return e.EncodeStats(s, out).Tokens
}
func (syntheticEmbedder) EncodeStats(s string, out []float32) embed.Stats {
	clear(out)
	for i, b := range []byte(s) {
		out[i%len(out)] += float32(b) / 255
	}
	embed.Normalize(out)
	return embed.Stats{Tokens: len(strings.Fields(s)), MeanNorm: 1, MaxNorm: 1}
}

func offlineServer(t *testing.T) (*Server, *echoProv) {
	t.Helper()
	cfg := config.Default()
	no := false
	cfg.Embedder.AutoDownload = &no
	cfg.Embedder.Dir = t.TempDir() // empty: accidental real embedder use must fail
	cfg.Learning.StatePath = filepath.Join(t.TempDir(), "learned.json")
	p := &echoProv{name: "openai"}
	s, err := New(context.Background(), cfg, Options{Embedder: syntheticEmbedder{}, Providers: map[string]provider.Provider{"openai": p}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, p
}

func request(s *Server, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestOfflineCachedRequestsAreFinalized(t *testing.T) {
	s, p := offlineServer(t)
	body := `{"model":"gpt-6-luna","temperature":0,"messages":[{"role":"user","content":"hi"}],"router":{"explain":true,"session_id":"test"}}`
	first := request(s, "/v1/chat/completions", body, nil)
	second := request(s, "/v1/chat/completions", body, nil)
	if first.Code != 200 || second.Code != 200 {
		t.Fatalf("first=%d %s second=%d %s", first.Code, first.Body, second.Code, second.Body)
	}
	if p.calls.Load() != 1 || second.Header().Get("X-Modelrouter-Cache") != "HIT" {
		t.Fatal("response was not cached")
	}
	if second.Header().Get("X-Modelrouter-Cost") != "0" {
		t.Fatal("cache charged upstream spend")
	}
	id := second.Header().Get("X-Modelrouter-Request-Id")
	fb := request(s, "/v1/feedback", `{"request_id":"`+id+`","rating":"good"}`, nil)
	if fb.Code != 200 {
		t.Fatalf("cached request missing from feedback: %d %s", fb.Code, fb.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(second.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["modelrouter"] == nil {
		t.Fatal("cached explain missing")
	}
	mw := httptest.NewRecorder()
	s.Handler().ServeHTTP(mw, httptest.NewRequest("GET", "/metrics", nil))
	metrics := mw.Body.String()
	if !strings.Contains(metrics, `mode="balance",outcome="cache_hit"} 1`) {
		t.Fatalf("cache request missing from metrics: %s", metrics)
	}
	if !strings.Contains(metrics, `kind="output"} 7`) {
		t.Fatal("cache replay duplicated upstream tokens")
	}
}

func TestOfflineHeadersValidatedBeforeDispatch(t *testing.T) {
	s, p := offlineServer(t)
	for _, header := range []map[string]string{
		{"X-Modelrouter-Max-Cost": "NaN"}, {"X-Modelrouter-Max-Cost": "typo"},
		{"X-Modelrouter-Min-Quality": "2"}, {"X-Modelrouter-Max-Latency": "-1"},
	} {
		for _, path := range []string{"/v1/chat/completions", "/v1/route"} {
			w := request(s, path, `{"messages":[{"role":"user","content":"hi"}]}`, header)
			if w.Code != 400 {
				t.Errorf("invalid headers accepted on %s: %v (%d)", path, header, w.Code)
			}
		}
	}
	if p.calls.Load() != 0 {
		t.Fatal("invalid request reached upstream")
	}
}

func TestOfflineUnknownModelWithCostCapRejected(t *testing.T) {
	s, p := offlineServer(t)
	w := request(s, "/v1/chat/completions", `{"model":"gpt-unknown","messages":[{"role":"user","content":"hi"}],"router":{"max_cost_usd":0.01}}`, nil)
	if w.Code != 400 || p.calls.Load() != 0 {
		t.Fatalf("unknown price bypassed cost cap: %d %s", w.Code, w.Body)
	}
}

func TestOfflineCachedRequestIDWithoutDebugHeaders(t *testing.T) {
	s, _ := offlineServer(t)
	no := false
	s.cfg.Router.ShowHeaders = &no
	body := `{"model":"gpt-6-luna","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	request(s, "/v1/chat/completions", body, nil)
	w := request(s, "/v1/chat/completions", body, nil)
	if w.Header().Get("X-Modelrouter-Request-Id") == "" {
		t.Fatal("cache hit lost feedback request id")
	}
}

func TestOfflineCanonicalValidation(t *testing.T) {
	s, _ := offlineServer(t)
	r, _ := apiopenai.Decode(strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	r.Router.MaxCostUSD = math.Inf(1)
	if _, err := s.Router.Route(r); err == nil {
		t.Fatal("non-finite direct request accepted")
	}
}

func TestOfflineHTTPTimeout(t *testing.T) {
	s, _ := offlineServer(t)
	s.cfg.Dispatch.TotalTimeout = time.Nanosecond
	w := request(s, "/v1/chat/completions", `{"model":"gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != 504 {
		t.Fatalf("deadline status=%d body=%s", w.Code, w.Body)
	}
}

// blockProvider exposes the upstream lifetime without making network requests.
type blockProvider struct {
	echo             echoProv
	started, release chan struct{}
}

func (p *blockProvider) Name() string { return "openai" }
func (p *blockProvider) Open(ctx context.Context, c *provider.Call) (provider.Stream, error) {
	close(p.started)
	select {
	case <-p.release:
		return p.echo.Open(ctx, c)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type joinContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *joinContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

func TestOfflineSingleflightFollowerCancellation(t *testing.T) {
	s, _ := offlineServer(t)
	s.cfg.Dispatch.TotalTimeout = 0
	p := &blockProvider{started: make(chan struct{}), release: make(chan struct{})}
	s.providers["openai"] = p
	body := `{"model":"gpt-6-luna","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	leader := make(chan *httptest.ResponseRecorder, 1)
	go func() { leader <- request(s, "/v1/chat/completions", body, nil) }()
	<-p.started
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jc := &joinContext{Context: ctx, joined: make(chan struct{})}
	follower := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)).WithContext(jc)
		s.Handler().ServeHTTP(w, r)
		follower <- w
	}()
	<-jc.joined
	cancel()
	select {
	case w := <-follower:
		if w.Code != 499 {
			t.Errorf("canceled follower status=%d", w.Code)
		}
	case <-time.After(time.Second):
		t.Error("canceled follower remained blocked")
	}
	close(p.release)
	if w := <-leader; w.Code != 200 {
		t.Fatalf("follower canceled leader: %d %s", w.Code, w.Body)
	}
	if p.echo.calls.Load() != 1 {
		t.Fatal("follower dispatched a second upstream")
	}
}

func TestOfflineCacheDoesNotRefreshUpstreamWarmth(t *testing.T) {
	s, _ := offlineServer(t)
	body := `{"model":"gpt-6-luna","temperature":0,"messages":[{"role":"user","content":"hi"}],"router":{"session_id":"warm"}}`
	request(s, "/v1/chat/completions", body, nil)
	req, _ := apiopenai.Decode(strings.NewReader(body))
	dec, err := s.Router.Route(req)
	if err != nil {
		t.Fatal(err)
	}
	old, ok := s.Router.Sessions.Get(dec.SessionKey)
	if !ok {
		t.Fatal("missing upstream session")
	}
	request(s, "/v1/chat/completions", body, nil)
	after, _ := s.Router.Sessions.Get(dec.SessionKey)
	if after.At != old.At || after.Turns != old.Turns {
		t.Fatal("cache hit invented a warm upstream prefix")
	}
}

func TestOfflineCacheRequestLoggedOnce(t *testing.T) {
	s, _ := offlineServer(t)
	path := filepath.Join(t.TempDir(), "decisions.jsonl")
	var err error
	s.dlog, err = telemetry.OpenDecisionLog(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt-6-luna","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	request(s, "/v1/chat/completions", body, nil)
	request(s, "/v1/chat/completions", body, nil)
	s.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one log record per request; got %d", len(lines))
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["cache"] != "hit" || rec["cost_usd"] != float64(0) {
		t.Fatalf("incorrect cache record: %+v", rec)
	}
}

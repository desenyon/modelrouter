package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/control"
	"github.com/desenyon/modelrouter/internal/health"
	"github.com/desenyon/modelrouter/internal/optimize"
	"github.com/desenyon/modelrouter/internal/provider"
)

// script describes how a fake model behaves.
type script struct {
	openErr   *provider.Error
	delay     time.Duration // before first event
	text      []string
	failAfter int // fail mid-stream after N events (0 = never)
	calls     atomic.Int32
}

type fakeProv struct {
	name    string
	scripts map[string]*script
}

func (f *fakeProv) Name() string { return f.name }

func (f *fakeProv) Open(ctx context.Context, call *provider.Call) (provider.Stream, error) {
	s := f.scripts[call.UpstreamID]
	s.calls.Add(1)
	if s.openErr != nil {
		e := *s.openErr
		return nil, &e
	}
	var evs []canon.Event
	evs = append(evs, canon.Event{Kind: canon.EvMeta, ID: "x", Model: call.UpstreamID})
	for _, t := range s.text {
		evs = append(evs, canon.Event{Kind: canon.EvText, Text: t})
	}
	evs = append(evs, canon.Event{Kind: canon.EvFinish, FinishReason: "stop"}, canon.Event{Kind: canon.EvUsage, Usage: &canon.Usage{InputTokens: 10, OutputTokens: 5}})
	return &fakeStream{ctx: ctx, evs: evs, delay: s.delay, failAfter: s.failAfter}, nil
}

type fakeStream struct {
	ctx       context.Context
	evs       []canon.Event
	i         int
	delay     time.Duration
	failAfter int
}

func (s *fakeStream) Header() http.Header { return http.Header{} }
func (s *fakeStream) Close() error        { return nil }
func (s *fakeStream) Next() (canon.Event, error) {
	if s.i == 0 && s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-s.ctx.Done():
			return canon.Event{}, s.ctx.Err()
		}
	}
	if s.failAfter > 0 && s.i >= s.failAfter {
		return canon.Event{}, errors.New("connection reset by peer")
	}
	if s.i >= len(s.evs) {
		return canon.Event{}, io.EOF
	}
	s.i++
	return s.evs[s.i-1], nil
}

func model(id, prov string) *catalog.Model {
	return &catalog.Model{ID: prov + "/" + id, Provider: prov, UpstreamID: id, Tier: catalog.Terra, MaxOutput: 1000, TTFTms: 10, TPS: 100}
}

func plan(ms ...*catalog.Model) *optimize.Plan {
	p := &optimize.Plan{}
	for i, m := range ms {
		c := &optimize.Candidate{Model: m, ModelID: m.ID, P: 0.9 - float64(i)*0.01, MaxTokens: 100}
		if i == 0 {
			p.Chosen = c
		} else {
			p.Fallbacks = append(p.Fallbacks, c)
		}
	}
	return p
}

func req() *canon.Request {
	return &canon.Request{Messages: []canon.Message{{Role: "user", Parts: []canon.Part{{Type: "text", Text: "hi"}}}}}
}

func newD(provs ...*fakeProv) (*Dispatcher, *health.Tracker) {
	m := map[string]provider.Provider{}
	for _, p := range provs {
		m[p.name] = p
	}
	h := health.New(health.DefaultConfig())
	return New(m, h, control.NewRetryBudget(0.5), Config{MaxAttempts: 3, FirstTokenTimeout: 200 * time.Millisecond, IdleTimeout: time.Second, QualityRetry: true}), h
}

func TestFailoverOnRateLimitToOtherProvider(t *testing.T) {
	a := &fakeProv{name: "a", scripts: map[string]*script{"m1": {openErr: &provider.Error{Kind: provider.KindRateLimit, Status: 429, RetryAfter: time.Second}}}}
	b := &fakeProv{name: "b", scripts: map[string]*script{"m2": {text: []string{"hello"}}}}
	d, h := newD(a, b)
	res := d.Run(context.Background(), Job{Req: req(), Plan: plan(model("m1", "a"), model("m2", "b"))})
	if res.Err != nil || res.Final.ModelID != "b/m2" || res.Resp.Content != "hello" {
		t.Fatalf("expected failover to b/m2, got err=%v final=%v", res.Err, res.Final)
	}
	if ok, why := h.Available("a", "a/m1"); ok || why != "rate_limited" {
		t.Fatal("429 should put the model in cooldown")
	}
	if len(res.Attempts) != 2 || res.Attempts[0].Outcome != "rate_limit" {
		t.Fatalf("attempts: %+v", res.Attempts)
	}
}

func TestFirstTokenTimeoutFailsOver(t *testing.T) {
	a := &fakeProv{name: "a", scripts: map[string]*script{"slow": {delay: 5 * time.Second, text: []string{"late"}}}}
	b := &fakeProv{name: "b", scripts: map[string]*script{"fast": {text: []string{"ok"}}}}
	d, _ := newD(a, b)
	start := time.Now()
	res := d.Run(context.Background(), Job{Req: req(), Plan: plan(model("slow", "a"), model("fast", "b"))})
	if res.Err != nil || res.Resp.Content != "ok" {
		t.Fatalf("expected timeout failover: %+v", res)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("first-token timeout did not fire")
	}
	if res.Attempts[0].Outcome != "timeout" {
		t.Fatalf("first attempt should be timeout, got %s", res.Attempts[0].Outcome)
	}
}

type recSink struct {
	mu        sync.Mutex
	committed string
	text      strings.Builder
	commits   int
}

func (s *recSink) Commit(c *optimize.Candidate, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.committed = c.ModelID
	s.commits++
	return nil
}
func (s *recSink) Event(ev canon.Event) error {
	if ev.Kind == canon.EvText {
		s.text.WriteString(ev.Text)
	}
	return nil
}

func TestStreamPeekFailsOverBeforeCommit(t *testing.T) {
	// Fails before any visible token → must fail over invisibly.
	a := &fakeProv{name: "a", scripts: map[string]*script{"m1": {text: []string{"x"}, failAfter: 1}}}
	b := &fakeProv{name: "b", scripts: map[string]*script{"m2": {text: []string{"he", "llo"}}}}
	d, _ := newD(a, b)
	sink := &recSink{}
	res := d.Run(context.Background(), Job{Req: req(), Plan: plan(model("m1", "a"), model("m2", "b")), Stream: true, Sink: sink})
	if res.Err != nil || sink.commits != 1 || sink.committed != "b/m2" || sink.text.String() != "hello" {
		t.Fatalf("commits=%d committed=%s text=%q err=%v", sink.commits, sink.committed, sink.text.String(), res.Err)
	}
}

func TestMidStreamFailureAfterCommitIsReported(t *testing.T) {
	a := &fakeProv{name: "a", scripts: map[string]*script{"m1": {text: []string{"a", "b", "c"}, failAfter: 3}}}
	b := &fakeProv{name: "b", scripts: map[string]*script{"m2": {text: []string{"z"}}}}
	d, _ := newD(a, b)
	sink := &recSink{}
	res := d.Run(context.Background(), Job{Req: req(), Plan: plan(model("m1", "a"), model("m2", "b")), Stream: true, Sink: sink})
	if !res.Committed || res.Err == nil || sink.committed != "a/m1" {
		t.Fatalf("expected committed mid-stream error, got %+v", res)
	}
	if b.scripts["m2"].calls.Load() != 0 {
		t.Fatal("must not fail over after bytes reached the client")
	}
}

func TestInvalidJSONEscalates(t *testing.T) {
	a := &fakeProv{name: "a", scripts: map[string]*script{"m1": {text: []string{"not json"}}}}
	b := &fakeProv{name: "b", scripts: map[string]*script{"m2": {text: []string{"```json\n{\"ok\":true}\n```"}}}}
	d, _ := newD(a, b)
	r := req()
	r.ResponseFormat = &canon.ResponseFormat{Type: canon.FormatJSONObject}
	res := d.Run(context.Background(), Job{Req: r, Plan: plan(model("m1", "a"), model("m2", "b"))})
	if res.Err != nil || res.Final.ModelID != "b/m2" || res.Resp.Content != `{"ok":true}` {
		t.Fatalf("expected escalation + fence repair, got %q from %v err=%v", res.Resp.Content, res.Final, res.Err)
	}
	if res.Quality["a/m1"] != "invalid_json" {
		t.Fatalf("quality signal missing: %v", res.Quality)
	}
}

func TestBadRequestSkipsSameProvider(t *testing.T) {
	bad := &provider.Error{Kind: provider.KindBadRequest, Status: 400}
	a := &fakeProv{name: "a", scripts: map[string]*script{"m1": {openErr: bad}, "m2": {text: []string{"same provider"}}}}
	b := &fakeProv{name: "b", scripts: map[string]*script{"m3": {text: []string{"other"}}}}
	d, _ := newD(a, b)
	res := d.Run(context.Background(), Job{Req: req(), Plan: plan(model("m1", "a"), model("m2", "a"), model("m3", "b"))})
	if res.Resp.Content != "other" || a.scripts["m2"].calls.Load() != 0 {
		t.Fatalf("400 should skip remaining arms on the same provider: got %q", res.Resp.Content)
	}
}

func TestClientCancelDoesNotHurtHealth(t *testing.T) {
	a := &fakeProv{name: "a", scripts: map[string]*script{"m1": {delay: time.Second, text: []string{"x"}}}}
	d, h := newD(a)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	for i := 0; i < 5; i++ {
		d.Run(ctx, Job{Req: req(), Plan: plan(model("m1", "a"))})
	}
	if ok, _ := h.Available("a", "a/m1"); !ok {
		t.Fatal("client cancellations must not open the breaker")
	}
}

func TestHedgeWinsWithFasterProvider(t *testing.T) {
	a := &fakeProv{name: "a", scripts: map[string]*script{"slow": {delay: 150 * time.Millisecond, text: []string{"slow"}}}}
	b := &fakeProv{name: "b", scripts: map[string]*script{"fast": {text: []string{"fast"}}}}
	d, _ := newD(a, b)
	d.cfg.Hedge, d.cfg.HedgeMinDelay, d.cfg.FirstTokenTimeout = true, 30*time.Millisecond, 5*time.Second
	res := d.Run(context.Background(), Job{Req: req(), Plan: plan(model("slow", "a"), model("fast", "b"))})
	if res.Err != nil || res.Resp.Content != "fast" {
		t.Fatalf("hedge should win: %q err=%v attempts=%+v", res.Resp.Content, res.Err, res.Attempts)
	}
}

func TestWireMaxTokensAddsReasoningAllowance(t *testing.T) {
	m := model("m", "a")
	m.MaxOutput = 64000
	c := &optimize.Candidate{Model: m, ReasonToks: 4000, MaxTokens: 500}
	r := req()
	r.MaxTokens = 500
	if got := wireMaxTokens(r, c); got != 500+6000 {
		t.Fatalf("visible max_tokens should get allowance, got %d", got)
	}
	r.MaxTokensInclusive = true
	if got := wireMaxTokens(r, c); got != 500 {
		t.Fatalf("inclusive limit must pass through, got %d", got)
	}
}

func TestPrimaryMustAcquireHealthPermit(t *testing.T) {
	a := &fakeProv{name: "a", scripts: map[string]*script{"m1": {text: []string{"blocked"}}}}
	b := &fakeProv{name: "b", scripts: map[string]*script{"m2": {text: []string{"healthy"}}}}
	d, h := newD(a, b)
	h.Record("a", "a/m1", health.Outcome{RateLimited: true, RetryAfter: time.Minute})
	res := d.Run(context.Background(), Job{Req: req(), Plan: plan(model("m1", "a"), model("m2", "b"))})
	if res.Err != nil || res.Resp.Content != "healthy" || a.scripts["m1"].calls.Load() != 0 {
		t.Fatalf("blocked primary was called: %+v", res)
	}
}

func TestTotalDeadlineIsTimeoutWithoutFurtherAttempts(t *testing.T) {
	a := &fakeProv{name: "a", scripts: map[string]*script{"slow": {delay: time.Second, text: []string{"late"}}}}
	b := &fakeProv{name: "b", scripts: map[string]*script{"fast": {text: []string{"ok"}}}}
	d, _ := newD(a, b)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	res := d.Run(ctx, Job{Req: req(), Plan: plan(model("slow", "a"), model("fast", "b"))})
	if res.Err == nil || res.Err.Kind != provider.KindTimeout {
		t.Fatalf("deadline classified as %v", res.Err)
	}
	if b.scripts["fast"].calls.Load() != 0 {
		t.Fatal("upstream called after request deadline")
	}
}

func TestHedgingHonorsMaxAttempts(t *testing.T) {
	a := &fakeProv{name: "a", scripts: map[string]*script{"slow": {delay: 80 * time.Millisecond, text: []string{"slow"}}}}
	b := &fakeProv{name: "b", scripts: map[string]*script{"fast": {text: []string{"fast"}}}}
	d, _ := newD(a, b)
	d.cfg.Hedge, d.cfg.HedgeMinDelay, d.cfg.MaxAttempts = true, time.Millisecond, 1
	res := d.Run(context.Background(), Job{Req: req(), Plan: plan(model("slow", "a"), model("fast", "b"))})
	if res.Err != nil || b.scripts["fast"].calls.Load() != 0 || len(res.Attempts) != 1 {
		t.Fatalf("max_attempts=1 launched hedge: %+v", res)
	}
}

func TestHedgeLoserReleasesHalfOpenProbe(t *testing.T) {
	a := &fakeProv{name: "a", scripts: map[string]*script{"slow": {delay: time.Second, text: []string{"late"}}}}
	b := &fakeProv{name: "b", scripts: map[string]*script{"fast": {text: []string{"ok"}}}}
	d, h := newD(a, b)
	now := time.Now()
	h.SetClock(func() time.Time { return now })
	for range 3 {
		h.Record("a", "a/slow", health.Outcome{HealthFail: true})
	}
	now = now.Add(time.Minute)
	d.cfg.Hedge, d.cfg.HedgeMinDelay = true, time.Millisecond
	res := d.Run(context.Background(), Job{Req: req(), Plan: plan(model("slow", "a"), model("fast", "b"))})
	if res.Err != nil || res.Resp.Content != "ok" {
		t.Fatalf("hedge failed: %+v", res)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if h.Acquire("a", "a/slow") {
			h.Record("a", "a/slow", health.Outcome{})
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("canceled half-open hedge kept probe forever")
}

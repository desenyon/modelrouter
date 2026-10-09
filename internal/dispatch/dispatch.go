// Package dispatch executes a routing plan against upstream providers:
// ordered failover across independent failure domains, stream peeking (no
// bytes reach the client until the first visible token, so failover stays
// possible), effort-scaled first-token and idle timeouts, optional hedged
// requests, output-quality checks with escalation, and a retry budget.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/control"
	"github.com/desenyon/modelrouter/internal/features"
	"github.com/desenyon/modelrouter/internal/health"
	"github.com/desenyon/modelrouter/internal/optimize"
	"github.com/desenyon/modelrouter/internal/provider"
)

// Config tunes dispatch.
type Config struct {
	MaxAttempts       int
	FirstTokenTimeout time.Duration
	IdleTimeout       time.Duration
	Hedge             bool
	HedgeMinDelay     time.Duration
	QualityRetry      bool
}

// Dispatcher is safe for concurrent use.
type Dispatcher struct {
	providers map[string]provider.Provider
	health    *health.Tracker
	retry     *control.RetryBudget
	cfg       Config
	now       func() time.Time
}

// New builds a dispatcher.
func New(providers map[string]provider.Provider, h *health.Tracker, rb *control.RetryBudget, cfg Config) *Dispatcher {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.FirstTokenTimeout <= 0 {
		cfg.FirstTokenTimeout = 30 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 120 * time.Second
	}
	if cfg.HedgeMinDelay <= 0 {
		cfg.HedgeMinDelay = 2 * time.Second
	}
	return &Dispatcher{providers: providers, health: h, retry: rb, cfg: cfg, now: time.Now}
}

// Sink receives a committed stream. Commit is called exactly once, before the
// first event, with the candidate that won.
type Sink interface {
	Commit(c *optimize.Candidate, attempts int) error
	Event(ev canon.Event) error
}

// Job is one dispatch.
type Job struct {
	Req        *canon.Request
	Plan       *optimize.Plan
	Stream     bool
	Sink       Sink // required when Stream
	RequestID  string
	SessionKey string
	CacheHint  bool
}

// Attempt records one upstream try.
type Attempt struct {
	ModelID  string      `json:"model"`
	Provider string      `json:"provider"`
	Effort   string      `json:"effort,omitempty"`
	TTFTms   float64     `json:"ttft_ms,omitempty"`
	DurMs    float64     `json:"duration_ms"`
	Outcome  string      `json:"outcome"` // ok | error kind | quality:<signal>
	Status   int         `json:"status,omitempty"`
	Message  string      `json:"message,omitempty"`
	Hedge    bool        `json:"hedge,omitempty"`
	CostUSD  float64     `json:"cost_usd,omitempty"`
	Usage    canon.Usage `json:"usage"`
}

// Result is the dispatch outcome.
type Result struct {
	Cache     string // hit/shared: replayed completion, no new upstream usage
	Resp      canon.Response
	Final     *optimize.Candidate
	Attempts  []Attempt
	Committed bool // stream bytes were sent to the client
	Err       *provider.Error
	Quality   map[string]string // modelID → quality signal for failed outputs
	CostUSD   float64           // summed over all attempts
}

type live struct {
	cand    *optimize.Candidate
	st      provider.Stream
	buf     []canon.Event
	done    bool // stream hit EOF while buffering
	start   time.Time
	ttft    time.Duration
	ctx     context.Context
	cancel  context.CancelCauseFunc
	release func() // cancels the per-attempt parent context (hedging)
	hedge   bool
}

// Run executes the job.
func (d *Dispatcher) Run(ctx context.Context, job Job) Result {
	res := Result{Quality: map[string]string{}}
	arms := append([]*optimize.Candidate{job.Plan.Chosen}, job.Plan.Fallbacks...)
	d.retry.Deposit()
	tried := map[string]bool{}
	var lastErr *provider.Error
	badRequestProviders := map[string]bool{}
	attempts := 0
	for i := 0; i < len(arms) && attempts < d.cfg.MaxAttempts; i++ {
		cand := arms[i]
		if ctx.Err() != nil {
			res.Err = d.classify(ctx.Err(), ctx, cand)
			return res
		}
		if tried[cand.ModelID] {
			continue
		}
		if lastErr != nil {
			// After a bad request, only a different provider can plausibly succeed.
			if lastErr.Kind == provider.KindBadRequest && badRequestProviders[cand.Model.Provider] {
				continue
			}
			if !d.retry.Take() {
				break
			}
		}
		if !d.health.Acquire(cand.Model.Provider, cand.ModelID) {
			continue
		}
		tried[cand.ModelID] = true
		attempts++

		var l *live
		var err *provider.Error
		recorded := false
		if attempts == 1 && d.cfg.Hedge && attempts < d.cfg.MaxAttempts {
			if h := hedgeCandidate(arms[i+1:], cand); h != nil {
				var launched bool
				l, err, launched = d.hedged(ctx, job, cand, h, &res)
				recorded = err != nil
				if launched {
					tried[h.ModelID] = true
					attempts++
				}
			}
		}
		if l == nil && err == nil {
			l, err = d.open(ctx, job, cand)
		}
		if err != nil {
			if !recorded {
				d.record(&res, cand, err, false)
			}
			lastErr = err
			if err.Kind == provider.KindBadRequest {
				badRequestProviders[cand.Model.Provider] = true
			}
			if err.Kind == provider.KindCanceled {
				break
			}
			continue
		}
		// A live stream with its first visible event (or a finished empty
		// response) is buffered; nothing has reached the client yet.
		if job.Stream {
			res.Committed = true
			resp, perr := d.pump(job, l, attempts)
			d.finishAttempt(&res, l, resp, perr)
			res.Resp, res.Final, res.Err = resp, l.cand, perr
			return res
		}
		resp, perr := d.pump(job, l, attempts)
		if perr != nil {
			d.finishAttempt(&res, l, resp, perr)
			lastErr = perr
			if perr.Kind == provider.KindCanceled {
				break
			}
			continue
		}
		if sig := Quality(job.Req, &resp); sig != "" {
			res.Quality[l.cand.ModelID] = sig
			d.finishAttemptQuality(&res, l, resp, sig)
			res.Resp, res.Final = resp, l.cand // kept as last resort
			if d.cfg.QualityRetry && attempts < d.cfg.MaxAttempts {
				arms = escalate(arms, i, l.cand, sig)
				lastErr = &provider.Error{Kind: provider.KindBadOutput, Message: sig}
				continue
			}
			return res
		}
		d.finishAttempt(&res, l, resp, nil)
		res.Resp, res.Final, res.Err = resp, l.cand, nil
		return res
	}
	if res.Final != nil && !job.Stream {
		return res // a quality-flagged answer beats an error
	}
	if lastErr == nil {
		lastErr = &provider.Error{Kind: provider.KindServer, Message: "no healthy upstream available"}
	}
	res.Err = lastErr
	return res
}

// open starts an attempt and buffers until the first visible event.
func (d *Dispatcher) open(ctx context.Context, job Job, cand *optimize.Candidate) (*live, *provider.Error) {
	prov, ok := d.providers[cand.Model.Provider]
	if !ok {
		return nil, &provider.Error{Provider: cand.Model.Provider, Model: cand.ModelID, Kind: provider.KindAuth, Message: "provider not configured"}
	}
	actx, cancel := context.WithCancelCause(ctx)
	l := &live{cand: cand, start: d.now(), ctx: actx, cancel: cancel}
	timer := time.AfterFunc(d.firstTokenTimeout(cand), func() { cancel(provider.ErrFirstTokenTimeout) })
	defer timer.Stop()
	call := &provider.Call{
		Req: job.Req, Model: cand.Model, UpstreamID: cand.Model.UpstreamID, Effort: cand.Effort,
		MaxTokens: wireMaxTokens(job.Req, cand), CacheHint: job.CacheHint, SessionKey: job.SessionKey, RequestID: job.RequestID,
	}
	st, err := prov.Open(actx, call)
	if err != nil {
		pe := d.classify(err, actx, cand)
		cancel(nil)
		return nil, pe
	}
	l.st = st
	d.observeHeaders(cand, st.Header())
	for {
		ev, err := st.Next()
		if err == io.EOF {
			l.done = true
			l.ttft = d.now().Sub(l.start)
			return l, nil
		}
		if err != nil {
			pe := d.classify(err, actx, cand)
			st.Close()
			cancel(nil)
			return nil, pe
		}
		l.buf = append(l.buf, ev)
		if ev.Visible() {
			l.ttft = d.now().Sub(l.start)
			return l, nil
		}
	}
}

// pump drains a live stream, forwarding to the sink when streaming.
func (d *Dispatcher) pump(job Job, l *live, attempts int) (canon.Response, *provider.Error) {
	defer l.st.Close()
	defer l.cancel(nil)
	if l.release != nil {
		defer l.release()
	}
	var asm canon.Assembler
	if job.Stream {
		if err := job.Sink.Commit(l.cand, attempts); err != nil {
			return asm.Response(), &provider.Error{Kind: provider.KindCanceled, Message: err.Error()}
		}
	}
	emit := func(ev canon.Event) *provider.Error {
		if ev.Kind == canon.EvKeepalive {
			return nil
		}
		asm.Add(ev)
		if job.Stream {
			if err := job.Sink.Event(ev); err != nil {
				return &provider.Error{Kind: provider.KindCanceled, Message: "client write: " + err.Error(), Err: provider.ErrClientGone}
			}
		}
		return nil
	}
	for _, ev := range l.buf {
		if e := emit(ev); e != nil {
			return asm.Response(), e
		}
	}
	if !l.done {
		idle := time.AfterFunc(d.cfg.IdleTimeout, func() { l.cancel(provider.ErrIdleTimeout) })
		defer idle.Stop()
		for {
			ev, err := l.st.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return asm.Response(), d.classify(err, l.ctx, l.cand)
			}
			idle.Reset(d.cfg.IdleTimeout)
			if e := emit(ev); e != nil {
				return asm.Response(), e
			}
		}
	}
	resp := asm.Response()
	if !asm.HasUsage() {
		resp.Usage = estimateUsage(l.cand, &resp)
	}
	if resp.Model == "" {
		resp.Model = l.cand.Model.UpstreamID
	}
	return resp, nil
}

// hedged races the primary against a different-provider arm launched after
// a delay; the first to produce a visible token wins and the other is
// canceled. Returns launched=true if the second arm was started. Errors are
// already recorded.
func (d *Dispatcher) hedged(ctx context.Context, job Job, primary, second *optimize.Candidate, res *Result) (*live, *provider.Error, bool) {
	type out struct {
		l   *live
		err *provider.Error
		c   *optimize.Candidate
	}
	ch := make(chan out, 2)
	pctx, pcancel := context.WithCancel(ctx)
	sctx, scancel := context.WithCancel(ctx)
	go func() { l, err := d.open(pctx, job, primary); ch <- out{l, err, primary} }()
	delay := d.cfg.HedgeMinDelay
	if p := time.Duration(2*primary.Model.TTFTms) * time.Millisecond; p > delay {
		delay = p
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	outstanding, launched := 1, false
	var lastErr *provider.Error
	for outstanding > 0 {
		select {
		case <-timer.C:
			if d.retry.Take() && d.health.Acquire(second.Model.Provider, second.ModelID) {
				launched = true
				outstanding++
				go func() { l, err := d.open(sctx, job, second); ch <- out{l, err, second} }()
			}
		case o := <-ch:
			outstanding--
			if o.err == nil {
				if o.c == primary {
					scancel()
					o.l.release = pcancel
				} else {
					pcancel()
					o.l.release = scancel
					o.l.hedge = true
				}
				if outstanding > 0 {
					go func() {
						r := <-ch
						if r.l != nil {
							r.l.st.Close()
							r.l.cancel(nil)
						}
						// Losing a race is neither success nor provider failure.
						// Release any half-open probe only after the attempt stops.
						d.health.Record(r.c.Model.Provider, r.c.ModelID, health.Outcome{})
					}()
				}
				if launched && outstanding > 0 {
					loser := second
					if o.c == second {
						loser = primary
					}
					res.Attempts = append(res.Attempts, Attempt{ModelID: loser.ModelID, Provider: loser.Model.Provider, Effort: loser.Effort, Outcome: "hedge_lost", Hedge: loser == second})
				}
				return o.l, nil, launched
			}
			d.record(res, o.c, o.err, o.c == second)
			lastErr = o.err
			if o.c == primary && !launched {
				scancel()
				pcancel()
				return nil, o.err, false
			}
		}
	}
	pcancel()
	scancel()
	return nil, lastErr, launched
}

func hedgeCandidate(rest []*optimize.Candidate, primary *optimize.Candidate) *optimize.Candidate {
	for _, c := range rest {
		if c.Model.Provider != primary.Model.Provider {
			return c
		}
	}
	return nil
}

// escalate moves the strongest remaining arm (by P) to position i+1 after a
// quality failure; refusals prefer a different provider.
func escalate(arms []*optimize.Candidate, i int, failed *optimize.Candidate, sig string) []*optimize.Candidate {
	best := -1
	for j := i + 1; j < len(arms); j++ {
		c := arms[j]
		if sig == "refusal" && c.Model.Provider == failed.Model.Provider {
			continue
		}
		if best < 0 || c.P > arms[best].P {
			best = j
		}
	}
	if best > i+1 {
		arms[i+1], arms[best] = arms[best], arms[i+1]
	}
	return arms
}

func (d *Dispatcher) firstTokenTimeout(c *optimize.Candidate) time.Duration {
	scale := map[string]float64{"": 2, "none": 1, "minimal": 1, "low": 1.5, "medium": 2, "high": 4, "xhigh": 6, "max": 8}[c.Effort]
	if scale == 0 {
		scale = 2
	}
	t := time.Duration(float64(d.cfg.FirstTokenTimeout) * scale)
	t += time.Duration(c.InTokens/20000) * time.Second // long-prompt prefill
	return t
}

// wireMaxTokens converts the client's limit into the upstream's (which
// counts reasoning tokens on every current provider).
func wireMaxTokens(r *canon.Request, c *optimize.Candidate) int {
	limit := c.Model.MaxOutput
	if r.MaxTokens <= 0 {
		return c.MaxTokens
	}
	n := r.MaxTokens
	if !r.MaxTokensInclusive && c.ReasonToks > 0 {
		allowance := c.ReasonToks * 3 / 2
		if allowance < 1024 {
			allowance = 1024
		}
		if allowance > 32000 {
			allowance = 32000
		}
		n += allowance
	}
	if limit > 0 && n > limit {
		n = limit
	}
	return n
}

// classify maps an attempt error using the attempt context's cancel cause
// (call before canceling the attempt context).
func (d *Dispatcher) classify(err error, actx context.Context, c *optimize.Candidate) *provider.Error {
	pe := provider.AsError(err, c.Model.Provider, c.ModelID)
	switch cause := context.Cause(actx); {
	case cause == nil:
	case errors.Is(cause, provider.ErrFirstTokenTimeout):
		pe = &provider.Error{Provider: c.Model.Provider, Model: c.ModelID, Kind: provider.KindTimeout, Message: "no output within first-token timeout"}
	case errors.Is(cause, provider.ErrIdleTimeout):
		pe = &provider.Error{Provider: c.Model.Provider, Model: c.ModelID, Kind: provider.KindTimeout, Message: "stream idle timeout"}
	case errors.Is(cause, context.DeadlineExceeded):
		pe = &provider.Error{Provider: c.Model.Provider, Model: c.ModelID, Kind: provider.KindTimeout, Message: "request deadline exceeded", Err: cause}
	case errors.Is(cause, context.Canceled):
		// Client disconnect or a lost hedge.
		pe = &provider.Error{Provider: c.Model.Provider, Model: c.ModelID, Kind: provider.KindCanceled, Message: cause.Error(), Err: provider.ErrClientGone}
	}
	return pe
}

func (d *Dispatcher) observeHeaders(c *optimize.Candidate, h interface{ Get(string) string }) {
	if h == nil {
		return
	}
	parse := func(keys ...string) int64 {
		for _, k := range keys {
			if v := h.Get(k); v != "" {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					return n
				}
			}
		}
		return -1
	}
	d.health.ObserveHeadroom(c.ModelID,
		parse("x-ratelimit-remaining-requests", "anthropic-ratelimit-requests-remaining"),
		parse("x-ratelimit-remaining-tokens", "anthropic-ratelimit-input-tokens-remaining", "anthropic-ratelimit-tokens-remaining"))
}

func (d *Dispatcher) record(res *Result, c *optimize.Candidate, err *provider.Error, hedge bool) {
	d.health.Record(c.Model.Provider, c.ModelID, health.Outcome{
		HealthFail: err.Kind.HealthFailure(), RateLimited: err.Kind == provider.KindRateLimit, RetryAfter: err.RetryAfter,
		NotFound: err.Kind == provider.KindNotFound, AuthFailed: err.Kind == provider.KindAuth, Err: err.Error(),
	})
	res.Attempts = append(res.Attempts, Attempt{ModelID: c.ModelID, Provider: c.Model.Provider, Effort: c.Effort,
		Outcome: err.Kind.String(), Status: err.Status, Message: trunc(err.Message, 300), Hedge: hedge})
}

func (d *Dispatcher) finishAttempt(res *Result, l *live, resp canon.Response, perr *provider.Error) {
	dur := d.now().Sub(l.start)
	cost := l.cand.Model.PriceAt(d.now()).Cost(resp.Usage.InputTokens, resp.Usage.CachedTokens, resp.Usage.CacheWriteTokens, resp.Usage.OutputTokens)
	res.CostUSD += cost
	a := Attempt{ModelID: l.cand.ModelID, Provider: l.cand.Model.Provider, Effort: l.cand.Effort, TTFTms: ms(l.ttft), DurMs: ms(dur),
		Outcome: "ok", Hedge: l.hedge, CostUSD: cost, Usage: resp.Usage}
	o := health.Outcome{OK: perr == nil, TTFT: l.ttft, Duration: dur, OutputTokens: resp.Usage.OutputTokens}
	if perr != nil {
		a.Outcome, a.Status, a.Message = perr.Kind.String(), perr.Status, trunc(perr.Message, 300)
		o.HealthFail = perr.Kind.HealthFailure()
		o.Err = perr.Error()
	}
	d.health.Record(l.cand.Model.Provider, l.cand.ModelID, o)
	res.Attempts = append(res.Attempts, a)
}

func (d *Dispatcher) finishAttemptQuality(res *Result, l *live, resp canon.Response, sig string) {
	d.finishAttempt(res, l, resp, nil)
	res.Attempts[len(res.Attempts)-1].Outcome = "quality:" + sig
}

// Quality returns a signal name if a completed (non-stream) response is
// unusable, repairing trivially fixable JSON in place.
func Quality(r *canon.Request, resp *canon.Response) string {
	if resp.FinishReason == canon.FinishContentFilter || (resp.Refusal != "" && resp.Content == "") {
		return "refusal"
	}
	if strings.TrimSpace(resp.Content) == "" && len(resp.ToolCalls) == 0 && resp.FinishReason != canon.FinishLength {
		return "empty"
	}
	for i := range resp.ToolCalls {
		a := strings.TrimSpace(resp.ToolCalls[i].Arguments)
		if a == "" {
			resp.ToolCalls[i].Arguments = "{}"
			continue
		}
		if !json.Valid([]byte(a)) {
			return "invalid_tool_args"
		}
	}
	if r.ResponseFormat.WantsJSON() && len(resp.ToolCalls) == 0 && resp.FinishReason != canon.FinishLength {
		c := strings.TrimSpace(resp.Content)
		if !json.Valid([]byte(c)) {
			if fixed := stripFences(c); json.Valid([]byte(fixed)) {
				resp.Content = fixed
			} else {
				return "invalid_json"
			}
		}
	}
	return ""
}

func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	if i, j := strings.IndexAny(s, "{["), strings.LastIndexAny(s, "}]"); i >= 0 && j > i {
		return s[i : j+1]
	}
	return strings.TrimSpace(s)
}

func estimateUsage(c *optimize.Candidate, r *canon.Response) canon.Usage {
	out := features.EstimateTokens(r.Content) + features.EstimateTokens(r.Refusal)
	for _, tc := range r.ToolCalls {
		out += features.EstimateTokens(tc.Arguments) + 8
	}
	return canon.Usage{InputTokens: c.InTokens, OutputTokens: out + c.ReasonToks, ReasoningTokens: c.ReasonToks}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

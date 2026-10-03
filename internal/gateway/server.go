// Package gateway is the HTTP server: OpenAI-compatible chat completions with
// routing, plus route preview, feedback, model listing, health, metrics and
// admin endpoints.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	apiopenai "github.com/desenyon/modelrouter/internal/api/openai"
	"github.com/desenyon/modelrouter/internal/cache"
	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/config"
	"github.com/desenyon/modelrouter/internal/control"
	"github.com/desenyon/modelrouter/internal/dispatch"
	"github.com/desenyon/modelrouter/internal/embed"
	"github.com/desenyon/modelrouter/internal/health"
	"github.com/desenyon/modelrouter/internal/learn"
	"github.com/desenyon/modelrouter/internal/optimize"
	"github.com/desenyon/modelrouter/internal/predict"
	"github.com/desenyon/modelrouter/internal/provider"
	"github.com/desenyon/modelrouter/internal/provider/anthropic"
	"github.com/desenyon/modelrouter/internal/provider/compat"
	"github.com/desenyon/modelrouter/internal/provider/gemini"
	pvopenai "github.com/desenyon/modelrouter/internal/provider/openai"
	"github.com/desenyon/modelrouter/internal/router"
	"github.com/desenyon/modelrouter/internal/session"
	"github.com/desenyon/modelrouter/internal/telemetry"
)

// Version is reported by the server and CLI.
const Version = "3.0.0"

// Server is the gateway.
type Server struct {
	cfg       config.Config
	log       *slog.Logger
	Router    *router.Router
	disp      *dispatch.Dispatcher
	providers map[string]provider.Provider
	cache     *cache.Store
	learner   *learn.Learner
	budget    *control.Budget
	health    *health.Tracker
	embedder  *embed.Model
	dlog      *telemetry.DecisionLog
	reg       *telemetry.Registry
	m         metrics
	mux       *http.ServeMux
	started   time.Time
}

type metrics struct {
	requests *telemetry.CounterVec
	cost     *telemetry.CounterVec
	tokens   *telemetry.CounterVec
	attempts *telemetry.CounterVec
	cache    *telemetry.CounterVec
	routing  *telemetry.HistogramVec
	ttft     *telemetry.HistogramVec
	latency  *telemetry.HistogramVec
	diff     *telemetry.HistogramVec
}

// Options inject dependencies (tests); zero values build real ones.
type Options struct {
	Logger    *slog.Logger
	Embedder  *embed.Model
	Providers map[string]provider.Provider
}

// New assembles the gateway. It loads (and if allowed downloads) the
// embedder, which the router requires.
func New(ctx context.Context, cfg config.Config, opt Options) (*Server, error) {
	log := opt.Logger
	if log == nil {
		log = slog.Default()
	}
	cat, err := cfg.BuildCatalog()
	if err != nil {
		return nil, err
	}
	em := opt.Embedder
	if em == nil {
		em, err = embed.Ensure(ctx, cfg.Embedder.Dir, cfg.AutoDownload(), func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) })
		if err != nil {
			return nil, fmt.Errorf("embedder (required): %w", err)
		}
	}
	pred, err := predict.New(em, predict.DefaultConfig())
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: log, embedder: em, started: time.Now(), mux: http.NewServeMux()}
	s.health = health.New(health.DefaultConfig())
	s.budget = control.NewBudget(cfg.Budget.USDPerHour)
	s.providers = opt.Providers
	if s.providers == nil {
		s.providers = buildProviders(cfg)
	}
	if !cfg.Learning.Disabled {
		s.learner = learn.New(cat, pred, learn.Config{LearningRate: cfg.Learning.LearningRate, StatePath: cfg.Learning.StatePath})
		if err := s.learner.Load(em.Name()); err != nil {
			log.Warn("could not load learned state", "err", err)
		}
	}
	defMode, _ := optimize.ParseMode(cfg.Router.DefaultMode)
	s.Router = &router.Router{
		Catalog: cat, Predictor: pred, Health: s.health, Sessions: session.New(200_000, 30*time.Minute),
		Learner: s.learner, Budget: s.budget, Objectives: cfg.Objectives(), DefaultMode: defMode,
		Ready:   func(p string) bool { _, ok := s.providers[p]; return ok },
		Explore: cfg.Router.ExploreRate, SwitchUSD: cfg.Router.SwitchPenalty,
	}
	d := cfg.Dispatch
	s.disp = dispatch.New(s.providers, s.health, control.NewRetryBudget(d.RetryRatio), dispatch.Config{
		MaxAttempts: d.MaxAttempts, FirstTokenTimeout: d.FirstTokenTimeout, IdleTimeout: d.IdleTimeout,
		Hedge: d.Hedge, HedgeMinDelay: d.HedgeMinDelay, QualityRetry: cfg.QualityRetry(),
	})
	if cfg.CacheEnabled() {
		s.cache = cache.New(cfg.Cache.MaxBytes, cfg.Cache.TTL)
	}
	if cfg.Telemetry.DecisionLog != "" {
		if s.dlog, err = telemetry.OpenDecisionLog(cfg.Telemetry.DecisionLog, cfg.Telemetry.Sample); err != nil {
			return nil, err
		}
	}
	s.initMetrics()
	s.routes()
	return s, nil
}

func buildProviders(cfg config.Config) map[string]provider.Provider {
	hc := provider.NewHTTPClient(cfg.Dispatch.ConnectTimeout)
	m := map[string]provider.Provider{}
	p := cfg.Providers
	if p.OpenAI.APIKey != "" && !p.OpenAI.Disabled {
		m["openai"] = pvopenai.New("openai", p.OpenAI.BaseURL, p.OpenAI.APIKey, p.OpenAI.Organization, hc)
	}
	if p.Anthropic.APIKey != "" && !p.Anthropic.Disabled {
		m["anthropic"] = anthropic.New(p.Anthropic.BaseURL, p.Anthropic.APIKey, hc, cfg.AnthropicServerFallbacks())
	}
	if p.Gemini.APIKey != "" && !p.Gemini.Disabled {
		m["gemini"] = gemini.New(p.Gemini.BaseURL, p.Gemini.APIKey, hc)
	}
	for _, e := range p.Extra {
		m[e.Name] = compat.New(e.Name, e.BaseURL, e.APIKey, e.Headers, hc)
	}
	return m
}

// Providers lists configured provider names.
func (s *Server) Providers() []string {
	out := make([]string, 0, len(s.providers))
	for k := range s.providers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Server) initMetrics() {
	r := telemetry.NewRegistry()
	s.reg = r
	s.m = metrics{
		requests: r.NewCounterVec("modelrouter_requests_total", "Routed requests by final model and outcome.", "model", "provider", "tier", "mode", "outcome"),
		cost:     r.NewCounterVec("modelrouter_cost_usd_total", "Upstream spend in USD (all attempts).", "model"),
		tokens:   r.NewCounterVec("modelrouter_tokens_total", "Tokens by model and kind.", "model", "kind"),
		attempts: r.NewCounterVec("modelrouter_attempts_total", "Upstream attempts by model and outcome.", "model", "outcome"),
		cache:    r.NewCounterVec("modelrouter_cache_total", "Response cache lookups.", "result"),
		routing:  r.NewHistogramVec("modelrouter_routing_seconds", "Routing decision latency.", []float64{25e-6, 50e-6, 100e-6, 250e-6, 500e-6, 1e-3, 5e-3}),
		ttft:     r.NewHistogramVec("modelrouter_ttft_seconds", "Upstream time to first visible token.", []float64{.1, .25, .5, 1, 2, 5, 10, 30, 60}, "provider"),
		latency:  r.NewHistogramVec("modelrouter_request_seconds", "End-to-end request latency.", []float64{.25, .5, 1, 2, 5, 10, 30, 60, 120}, "mode"),
		diff:     r.NewHistogramVec("modelrouter_difficulty", "Predicted request difficulty.", []float64{.1, .2, .3, .4, .5, .6, .7, .8, .9, 1}),
	}
	r.NewGaugeFunc("modelrouter_budget_lambda", "Spend controller cost multiplier.", func() map[string]float64 {
		return map[string]float64{"": s.budget.Lambda()}
	})
	r.NewGaugeFunc("modelrouter_breaker_open", "1 if the model's circuit is open.", func() map[string]float64 {
		out := map[string]float64{}
		for _, h := range s.health.SnapshotAll() {
			v := 0.0
			if h.State == health.Open {
				v = 1
			}
			out[h.Model] = v
		}
		return out
	}, "model")
	r.NewGaugeFunc("modelrouter_learned_exemplars", "Exemplars learned from feedback.", func() map[string]float64 {
		_, n := s.Router.Predictor.Size()
		return map[string]float64{"": float64(n)}
	})
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/chat/completions", s.auth(s.handleChat))
	s.mux.HandleFunc("POST /chat/completions", s.auth(s.handleChat))
	s.mux.HandleFunc("POST /v1/route", s.auth(s.handleRoute))
	s.mux.HandleFunc("POST /v1/feedback", s.auth(s.handleFeedback))
	s.mux.HandleFunc("GET /v1/models", s.auth(s.handleModels))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]any{"status": "ok"}) })
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /admin/state", s.admin(s.handleState))
	s.mux.HandleFunc("GET /{$}", s.handleRoot)
}

// Background runs periodic maintenance until ctx ends, then persists state.
func (s *Server) Background(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	flushEvery := s.cfg.Learning.FlushInterval
	if flushEvery <= 0 {
		flushEvery = 30 * time.Second
	}
	lastFlush, lastSave := time.Now(), time.Now()
	for {
		select {
		case <-ctx.Done():
			s.Close()
			return
		case <-tick.C:
			s.budget.Tick()
			if s.learner != nil && time.Since(lastFlush) >= flushEvery {
				if n := s.learner.Flush(); n > 0 {
					s.log.Info("learned exemplars added", "n", n)
				}
				lastFlush = time.Now()
			}
			if s.learner != nil && time.Since(lastSave) >= 5*time.Minute {
				if err := s.learner.Save(s.embedder.Name()); err != nil {
					s.log.Warn("save learned state", "err", err)
				}
				lastSave = time.Now()
			}
		}
	}
}

// Close persists learned state and flushes logs.
func (s *Server) Close() {
	if s.learner != nil {
		if err := s.learner.Save(s.embedder.Name()); err != nil {
			s.log.Warn("save learned state", "err", err)
		}
	}
	if s.dlog != nil {
		_ = s.dlog.Close()
	}
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.APIKeys) == 0 {
			next(w, r)
			return
		}
		tok := bearer(r)
		for _, k := range s.cfg.APIKeys {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(k)) == 1 {
				next(w, r)
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "invalid API key", "authentication_error")
	}
}

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminKey != "" {
			if subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(s.cfg.AdminKey)) != 1 {
				writeError(w, http.StatusUnauthorized, "invalid admin key", "authentication_error")
				return
			}
			next(w, r)
			return
		}
		s.auth(next)(w, r)
	}
}

func bearer(r *http.Request) string {
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	return r.Header.Get("api-key")
}

// applyHeaders lets clients set routing options via headers.
func applyHeaders(r *http.Request, req *canon.Request) {
	h := r.Header
	if v := h.Get("X-Modelrouter-Mode"); v != "" {
		req.Router.Mode = v
	} else if v := h.Get("X-Optimize-For"); v != "" && req.Router.Mode == "" {
		req.Router.Mode = v
	}
	if v := h.Get("X-Modelrouter-Session"); v != "" {
		req.Router.SessionID = v
	}
	if v := h.Get("X-Modelrouter-Max-Cost"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			req.Router.MaxCostUSD = f
		}
	}
	if v := h.Get("X-Modelrouter-Min-Quality"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			req.Router.MinQuality = f
		}
	}
	if v := h.Get("X-Modelrouter-Allow"); v != "" {
		req.Router.Allow = append(req.Router.Allow, strings.Split(v, ",")...)
	}
	if v := h.Get("X-Modelrouter-Deny"); v != "" {
		req.Router.Deny = append(req.Router.Deny, strings.Split(v, ",")...)
	}
	if h.Get("X-Modelrouter-Explain") == "1" || h.Get("X-Modelrouter-Explain") == "true" {
		req.Router.Explain = true
	}
	cc := strings.ToLower(h.Get("Cache-Control"))
	if strings.Contains(cc, "no-cache") || strings.Contains(cc, "no-store") {
		req.Router.NoCache = true
	}
	if h.Get("X-Modelrouter-Cache") == "force" {
		req.Router.ForceCache = true
	}
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	req, err := apiopenai.Decode(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	applyHeaders(r, req)
	dec, err := s.Router.Route(req)
	if err != nil {
		code := http.StatusBadRequest
		if strings.HasPrefix(err.Error(), "no model can serve") {
			code = http.StatusServiceUnavailable
		}
		writeError(w, code, err.Error(), "routing_error")
		return
	}
	s.m.routing.Observe(dec.RoutingTime.Seconds())
	s.m.diff.Observe(dec.Plan.Prediction.Difficulty)
	reqID := newID()

	// Response cache (deterministic requests only).
	var ckey string
	if s.cache != nil && cache.Cacheable(req) {
		ckey = cache.Key(req, strings.ToLower(req.Model)+"|"+string(dec.Mode))
		if ent, ok := s.cache.Get(ckey); ok {
			s.m.cache.Inc("hit")
			s.serveCached(w, req, dec, reqID, ent)
			return
		}
		s.m.cache.Inc("miss")
	}

	ctx := r.Context()
	if t := s.cfg.Dispatch.TotalTimeout; t > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
	}
	cacheHint := dec.SessionKey != "" && (dec.Session != nil || dec.Plan.Features.UserTurns > 1 || dec.Plan.Features.InToolLoop)
	job := dispatch.Job{Req: req, Plan: dec.Plan, Stream: req.Stream, RequestID: reqID, SessionKey: dec.SessionKey, CacheHint: cacheHint}

	var res dispatch.Result
	if req.Stream {
		sink := &streamSink{s: s, w: w, req: req, dec: dec, reqID: reqID}
		job.Sink = sink
		res = s.disp.Run(ctx, job)
		if res.Committed {
			if res.Err != nil {
				sink.sw.Error(res.Err.Error(), "upstream_error")
			} else {
				sink.sw.Close()
			}
		} else if res.Err != nil {
			s.writeUpstreamError(w, res.Err)
		}
	} else if ckey != "" {
		var shared, ok bool
		var ent cache.Entry
		ent, ok, shared = s.cache.Do(ckey, func() (cache.Entry, bool) {
			res = s.disp.Run(ctx, job)
			ok := res.Err == nil && res.Final != nil && len(res.Quality) == 0
			e := cache.Entry{Resp: res.Resp, At: time.Now(), CostUSD: res.CostUSD}
			if res.Final != nil {
				e.ModelID, e.Tier, e.Effort = res.Final.ModelID, string(res.Final.Tier), res.Final.Effort
			}
			return e, ok
		})
		switch {
		case shared && ok:
			s.m.cache.Inc("shared")
			s.serveCached(w, req, dec, reqID, ent)
			return
		case shared:
			// The leader failed; don't replay its failure, try independently.
			res = s.disp.Run(ctx, job)
		}
		s.respond(w, req, dec, reqID, res)
	} else {
		res = s.disp.Run(ctx, job)
		s.respond(w, req, dec, reqID, res)
	}
	s.after(req, dec, reqID, res, start)
}

func (s *Server) respond(w http.ResponseWriter, req *canon.Request, dec *router.Decision, reqID string, res dispatch.Result) {
	if res.Final == nil {
		s.writeUpstreamError(w, res.Err)
		return
	}
	s.setHeaders(w.Header(), dec, res.Final, reqID, len(res.Attempts), "MISS")
	w.Header().Set("X-Modelrouter-Cost", strconv.FormatFloat(res.CostUSD, 'f', 6, 64))
	var extra map[string]any
	if req.Router.Explain {
		extra = map[string]any{"modelrouter": explain(dec, &res)}
	}
	body, err := apiopenai.Encode("chatcmpl-"+reqID, res.Final.Model.UpstreamID, res.Resp, extra)
	if err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) serveCached(w http.ResponseWriter, req *canon.Request, dec *router.Decision, reqID string, ent cache.Entry) {
	m, ok := s.Router.Catalog.Lookup(ent.ModelID)
	h := w.Header()
	if s.cfg.ShowHeaders() {
		h.Set("X-Modelrouter-Request-Id", reqID)
		h.Set("X-Modelrouter-Model", ent.ModelID)
		h.Set("X-Modelrouter-Tier", ent.Tier)
		h.Set("X-Modelrouter-Mode", string(dec.Mode))
		h.Set("X-Modelrouter-Cache", "HIT")
		h.Set("X-Modelrouter-Cost", "0")
	}
	model := ent.ModelID
	if ok {
		model = m.UpstreamID
	}
	if req.Stream {
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		sw := apiopenai.NewStreamWriter(w, "chatcmpl-"+reqID, model, req.StreamUsage)
		for _, ev := range ent.Resp.Events() {
			_ = sw.Event(ev)
		}
		_ = sw.Close()
		return
	}
	body, _ := apiopenai.Encode("chatcmpl-"+reqID, model, ent.Resp, nil)
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

type streamSink struct {
	s     *Server
	w     http.ResponseWriter
	req   *canon.Request
	dec   *router.Decision
	reqID string
	sw    *apiopenai.StreamWriter
}

func (k *streamSink) Commit(c *optimize.Candidate, attempts int) error {
	h := k.w.Header()
	k.s.setHeaders(h, k.dec, c, k.reqID, attempts, "MISS")
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	k.w.WriteHeader(http.StatusOK)
	k.sw = apiopenai.NewStreamWriter(k.w, "chatcmpl-"+k.reqID, c.Model.UpstreamID, k.req.StreamUsage)
	return nil
}

func (k *streamSink) Event(ev canon.Event) error { return k.sw.Event(ev) }

func (s *Server) setHeaders(h http.Header, dec *router.Decision, c *optimize.Candidate, reqID string, attempts int, cacheState string) {
	h.Set("X-Modelrouter-Request-Id", reqID)
	if !s.cfg.ShowHeaders() {
		return
	}
	h.Set("X-Modelrouter-Model", c.ModelID)
	h.Set("X-Modelrouter-Provider", c.Model.Provider)
	h.Set("X-Modelrouter-Tier", string(c.Model.Tier))
	if c.Effort != "" {
		h.Set("X-Modelrouter-Effort", c.Effort)
	}
	h.Set("X-Modelrouter-Mode", string(dec.Mode))
	h.Set("X-Modelrouter-Difficulty", fmt.Sprintf("%.3f", dec.Plan.Prediction.Difficulty))
	h.Set("X-Modelrouter-P-Success", fmt.Sprintf("%.3f", c.P))
	h.Set("X-Modelrouter-Est-Cost", strconv.FormatFloat(c.CostUSD, 'f', 6, 64))
	h.Set("X-Modelrouter-Attempts", strconv.Itoa(attempts))
	h.Set("X-Modelrouter-Routing-Us", strconv.FormatInt(dec.RoutingTime.Microseconds(), 10))
	if s.cache != nil {
		h.Set("X-Modelrouter-Cache", cacheState)
	}
}

func (s *Server) writeUpstreamError(w http.ResponseWriter, e *provider.Error) {
	if e == nil {
		writeError(w, http.StatusBadGateway, "no upstream response", "upstream_error")
		return
	}
	code, typ := http.StatusBadGateway, "upstream_error"
	switch e.Kind {
	case provider.KindBadRequest:
		code, typ = http.StatusBadRequest, "invalid_request_error"
	case provider.KindRateLimit:
		code, typ = http.StatusTooManyRequests, "rate_limit_error"
		if e.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(e.RetryAfter.Seconds()+0.999)))
		}
	case provider.KindTimeout:
		code = http.StatusGatewayTimeout
	case provider.KindCanceled:
		code, typ = 499, "client_closed_request"
	case provider.KindNotFound:
		code, typ = http.StatusNotFound, "model_not_found"
	}
	writeError(w, code, e.Error(), typ)
}

// after records metrics, learning, affinity, budget and the decision log.
func (s *Server) after(req *canon.Request, dec *router.Decision, reqID string, res dispatch.Result, start time.Time) {
	outcome := "ok"
	if res.Err != nil {
		outcome = res.Err.Kind.String()
	}
	for _, a := range res.Attempts {
		s.m.attempts.Inc(a.ModelID, a.Outcome)
		if a.CostUSD > 0 {
			s.m.cost.Add(a.CostUSD, a.ModelID)
		}
		if a.TTFTms > 0 && a.Outcome != "hedge_lost" {
			s.m.ttft.Observe(a.TTFTms/1000, a.Provider)
		}
	}
	s.budget.Record(res.CostUSD)
	s.m.latency.Observe(time.Since(start).Seconds(), string(dec.Mode))
	if f := res.Final; f != nil {
		u := res.Resp.Usage
		s.m.requests.Inc(f.ModelID, f.Model.Provider, string(f.Model.Tier), string(dec.Mode), outcome)
		s.m.tokens.Add(float64(u.InputTokens), f.ModelID, "input")
		s.m.tokens.Add(float64(u.OutputTokens), f.ModelID, "output")
		s.m.tokens.Add(float64(u.CachedTokens), f.ModelID, "cached")
		s.m.tokens.Add(float64(u.ReasoningTokens), f.ModelID, "reasoning")
		if res.Err == nil {
			s.Router.Commit(dec, f, u.InputTokens)
		}
		if s.learner != nil && res.Err == nil {
			s.learner.Remember(&learn.Decision{ID: reqID, Vec: dec.Vec, Lex: dec.Lex, Axes: dec.Plan.Prediction.Axes,
				Difficulty: dec.Plan.Prediction.Difficulty, ModelID: f.ModelID, Effort: f.Effort,
				OutTokens: max(u.OutputTokens-u.ReasoningTokens, 1), At: time.Now()}, dec.Full, dec.Prev)
		}
	} else {
		s.m.requests.Inc("", "", "", string(dec.Mode), outcome)
	}
	if s.learner != nil {
		// Failed outputs from earlier attempts are implicit negatives for those models.
		for model, sig := range res.Quality {
			if res.Final != nil && model == res.Final.ModelID && res.Err == nil && len(res.Attempts) == 1 {
				continue
			}
			id := reqID + ":" + model
			c := findCandidate(dec.Plan, model)
			if c == nil {
				continue
			}
			s.learner.Remember(&learn.Decision{ID: id, Vec: dec.Vec, Lex: dec.Lex, Axes: dec.Plan.Prediction.Axes,
				Difficulty: dec.Plan.Prediction.Difficulty, ModelID: model, Effort: c.Effort, OutTokens: c.OutTokens, At: time.Now()}, "", "")
			s.learner.Implicit(id, sig)
		}
	}
	s.dlog.Log(decisionRecord(req, dec, reqID, &res, time.Since(start)))
}

func findCandidate(p *optimize.Plan, model string) *optimize.Candidate {
	for _, c := range append([]*optimize.Candidate{p.Chosen}, p.Fallbacks...) {
		if c != nil && c.ModelID == model {
			return c
		}
	}
	return nil
}

func explain(dec *router.Decision, res *dispatch.Result) map[string]any {
	out := map[string]any{"plan": dec.Plan, "routing_us": dec.RoutingTime.Microseconds(), "session": dec.SessionKey != ""}
	if res != nil {
		out["attempts"] = res.Attempts
		out["cost_usd"] = res.CostUSD
	}
	return out
}

func decisionRecord(req *canon.Request, dec *router.Decision, id string, res *dispatch.Result, dur time.Duration) map[string]any {
	rec := map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339Nano), "id": id, "requested": req.Model, "mode": dec.Mode,
		"difficulty": dec.Plan.Prediction.Difficulty, "uncertainty": dec.Plan.Prediction.Uncertainty,
		"axes": dec.Plan.Prediction.Axes.Map(), "features": dec.Plan.Features,
		"chosen": dec.Plan.Chosen, "fallbacks": dec.Plan.Fallbacks, "lambda": dec.Plan.Lambda,
		"attempts": res.Attempts, "cost_usd": res.CostUSD, "duration_ms": dur.Milliseconds(),
		"routing_us": dec.RoutingTime.Microseconds(), "stream": req.Stream,
	}
	if res.Final != nil {
		rec["final"] = res.Final.ModelID
		rec["usage"] = res.Resp.Usage
		rec["finish"] = res.Resp.FinishReason
	}
	if res.Err != nil {
		rec["error"] = res.Err.Error()
	}
	return rec
}

func (s *Server) handleRoute(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, apiopenai.MaxBodyBytes)); err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	body := buf.Bytes()
	var short struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
		Mode   string `json:"mode"`
	}
	if json.Unmarshal(body, &short) == nil && short.Prompt != "" {
		b, _ := json.Marshal(map[string]any{"model": short.Model, "messages": []map[string]string{{"role": "user", "content": short.Prompt}}, "router": map[string]any{"mode": short.Mode}})
		body = b
	}
	req, err := apiopenai.Decode(bytes.NewReader(body))
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	applyHeaders(r, req)
	dec, err := s.Router.Route(req)
	if err != nil {
		writeError(w, 400, err.Error(), "routing_error")
		return
	}
	writeJSON(w, 200, explain(dec, nil))
}

func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	if s.learner == nil {
		writeError(w, 409, "learning is disabled", "invalid_request_error")
		return
	}
	var fb struct {
		RequestID string   `json:"request_id"`
		Score     *float64 `json:"score"`
		Rating    string   `json:"rating"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&fb); err != nil {
		writeError(w, 400, "invalid JSON", "invalid_request_error")
		return
	}
	score := -1.0
	if fb.Score != nil {
		score = *fb.Score
	}
	switch strings.ToLower(fb.Rating) {
	case "good", "up", "positive", "thumbs_up":
		score = 1
	case "bad", "down", "negative", "thumbs_down":
		score = 0
	}
	if score < 0 {
		writeError(w, 400, "provide score in [0,1] or rating good|bad", "invalid_request_error")
		return
	}
	if err := s.learner.Feedback(strings.TrimPrefix(fb.RequestID, "chatcmpl-"), score); err != nil {
		code := 400
		if errors.Is(err, learn.ErrUnknownRequest) {
			code = 404
		}
		writeError(w, code, err.Error(), "invalid_request_error")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "recorded"})
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	created := s.started.Unix()
	var data []map[string]any
	for _, v := range router.VirtualModels {
		data = append(data, map[string]any{"id": v.ID, "object": "model", "created": created, "owned_by": "modelrouter", "description": v.Description})
	}
	for _, m := range s.Router.Catalog.All() {
		if _, ok := s.providers[m.Provider]; !ok {
			continue
		}
		data = append(data, map[string]any{"id": m.ID, "object": "model", "created": created, "owned_by": m.Provider,
			"tier": m.Tier, "auto_routed": m.Enabled, "context_window": m.Context, "max_output": m.MaxOutput})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	ready := 0
	for _, m := range s.Router.Catalog.All() {
		if _, ok := s.providers[m.Provider]; ok && m.Enabled {
			ready++
		}
	}
	status := 200
	if ready == 0 {
		status = 503
	}
	writeJSON(w, status, map[string]any{"ready": ready > 0, "routable_models": ready, "providers": s.Providers(), "embedder": s.embedder.Name()})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Telemetry.MetricsAuth {
		ok := false
		s.admin(func(http.ResponseWriter, *http.Request) { ok = true })(w, r)
		if !ok {
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	s.reg.WritePrometheus(w)
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{
		"version": Version, "uptime_s": int(time.Since(s.started).Seconds()), "providers": s.Providers(),
		"health": s.health.SnapshotAll(), "budget": s.budget.Snapshot(), "catalog_snapshot": catalog.Snapshot,
	}
	if s.learner != nil {
		out["learning"] = s.learner.Stats()
	}
	if s.cache != nil {
		h, m, n, b := s.cache.Stats()
		out["cache"] = map[string]any{"hits": h, "misses": m, "items": n, "bytes": b}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"name": "modelrouter", "version": Version,
		"endpoints": []string{"POST /v1/chat/completions", "POST /v1/route", "POST /v1/feedback", "GET /v1/models", "GET /healthz", "GET /readyz", "GET /metrics", "GET /admin/state"},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ}})
}

// ProviderByName returns a configured provider adapter.
func (s *Server) ProviderByName(name string) provider.Provider { return s.providers[name] }

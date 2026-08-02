package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/desenyon/modelrouter/internal/cache"
	"github.com/desenyon/modelrouter/internal/cascade"
	"github.com/desenyon/modelrouter/internal/classifier"
	"github.com/desenyon/modelrouter/internal/config"
	"github.com/desenyon/modelrouter/internal/health"
	"github.com/desenyon/modelrouter/internal/metrics"
	"github.com/desenyon/modelrouter/internal/router"
	"github.com/desenyon/modelrouter/internal/upstream"
)

// Server is the OpenAI-compatible routing gateway.
type Server struct {
	cfg      config.Config
	engine   *router.Engine
	upstream *upstream.Client
	metrics  *metrics.Collector
	cache    *cache.Store
	health   *health.Tracker
	mux      *http.ServeMux
	log      *log.Logger
}

// New constructs the gateway HTTP server.
func New(cfg config.Config, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	ht := health.New()
	s := &Server{
		cfg:      cfg,
		engine:   router.New(cfg, ht),
		upstream: upstream.New(cfg.Upstream.BaseURL, cfg.Upstream.APIKey, cfg.Upstream.Timeout),
		metrics:  metrics.New(),
		health:   ht,
		mux:      http.NewServeMux(),
		log:      logger,
	}
	if cfg.Cache.Enabled {
		s.cache = cache.New(cfg.Cache.Capacity)
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /v1/models", s.auth(s.handleModels))
	s.mux.HandleFunc("POST /v1/chat/completions", s.auth(s.handleChat))
	s.mux.HandleFunc("POST /v1/route", s.auth(s.handleRoutePreview))
	s.mux.HandleFunc("GET /", s.handleRoot)
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.withCORS(s.mux)
}

// ListenAndServe starts the gateway.
func (s *Server) ListenAndServe() error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	luna, terra := s.engine.Thresholds()
	s.log.Printf("modelrouter gateway listening on %s (upstream %s, mode %s)",
		s.cfg.Listen, s.cfg.Upstream.BaseURL, s.cfg.ModeOrDefault())
	s.log.Printf("tiers luna=%s terra=%s sol=%s | thresholds luna≤%.2f terra≤%.2f | cache=%v cascade=%v adaptive=%v",
		s.cfg.Models.Luna.Primary, s.cfg.Models.Terra.Primary, s.cfg.Models.Sol.Primary,
		luna, terra, s.cfg.Cache.Enabled, s.cfg.Cascade.Enabled, s.cfg.Router.Adaptive)
	return srv.ListenAndServe()
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKey == "" {
			next(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		token := strings.TrimPrefix(h, "Bearer ")
		if token == "" {
			token = r.Header.Get("X-API-Key")
		}
		if token != s.cfg.APIKey {
			writeErr(w, http.StatusUnauthorized, "invalid api key", "authentication_error")
			return
		}
		next(w, r)
	}
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Optimize-For, Cache-Control")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":   "modelrouter",
		"role":   "complex-yet-efficient model routing gateway",
		"policy": "luna-first — sol only when the task demands it",
		"docs":   "https://github.com/desenyon/modelrouter",
		"pipeline": []string{
			"fingerprint-cache", "features", "score", "policy",
			"tier", "candidate+circuit", "proxy", "cascade", "adapt",
		},
		"endpoints": []string{
			"GET  /healthz",
			"GET  /metrics",
			"GET  /v1/models",
			"POST /v1/chat/completions",
			"POST /v1/route",
		},
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	luna, terra := s.engine.Thresholds()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":            "ok",
		"upstream":          s.upstream.BaseURL(),
		"upstream_key_set":  s.upstream.HasAPIKey(),
		"mode":              s.cfg.ModeOrDefault(),
		"luna_max":          luna,
		"terra_max":         terra,
		"open_circuits":     s.health.OpenCount(),
		"cache_enabled":     s.cfg.Cache.Enabled,
		"cascade_enabled":   s.cfg.Cascade.Enabled,
		"adaptive_enabled":  s.cfg.Router.Adaptive,
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	snap := s.metrics.Snapshot()
	luna, terra := s.engine.Thresholds()
	out := map[string]any{
		"gateway":    snap,
		"circuits":   s.health.SnapshotAll(),
		"thresholds": map[string]float64{"luna_max": luna, "terra_max": terra},
	}
	if s.cache != nil {
		hits, misses, size := s.cache.Stats()
		out["cache"] = map[string]any{"hits": hits, "misses": misses, "size": size}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	created := time.Now().Unix()
	list := ModelList{Object: "list"}
	for _, e := range s.engine.Registry().Catalog() {
		list.Data = append(list.Data, ModelEntry{
			ID: e.ID, Object: "model", Created: created, OwnedBy: e.OwnedBy,
		})
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleRoutePreview(w http.ResponseWriter, r *http.Request) {
	dec, _, err := s.decide(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, RoutePreview{Decision: dec, At: time.Now().UTC()})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	dec, raw, chat, err := s.decideFull(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	noCache := strings.Contains(strings.ToLower(r.Header.Get("Cache-Control")), "no-cache")
	var fp string
	if s.cache != nil && !chat.Stream && !noCache && !dec.Passthrough {
		fp = cache.Fingerprint(dec.RequestedModel, string(dec.Mode), chat.Messages, chat.Tools, isStructured(chat.ResponseFormat))
		if ent, ok := s.cache.Get(fp); ok {
			s.metrics.RecordCacheHit()
			s.metrics.RecordRoute(ent.Tier, false, time.Since(start), false, len(raw), len(ent.Body))
			s.setRouteHeaders(w, dec, true, "")
			w.Header().Set("Content-Type", ent.ContentType)
			w.Header().Set("X-Modelrouter-Cache", "HIT")
			w.WriteHeader(ent.Status)
			_, _ = w.Write(ent.Body)
			return
		}
	}

	ctx := r.Context()
	if s.cfg.Upstream.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.Upstream.Timeout)
		defer cancel()
	}

	if chat.Stream {
		final, cascLabel, status, upErr := s.proxyStream(ctx, w, raw, dec)
		latency := time.Since(start)
		success := upErr == nil && status > 0 && status < 400
		em := ""
		if upErr != nil {
			em = upErr.Error()
		} else if !success {
			em = fmt.Sprintf("status %d", status)
		}
		s.engine.RecordOutcome(final, float64(latency.Milliseconds()), success, em)
		if cascLabel != "" {
			s.metrics.RecordCascade()
		}
		s.metrics.RecordRoute(string(final.Tier), final.Passthrough, latency, !success, len(raw), 0)
		if upErr != nil && status == 0 {
			writeErr(w, http.StatusBadGateway, upErr.Error(), "upstream_error")
		}
		s.log.Printf("stream %s → %s tier=%s score=%.3f cascade=%q %s",
			final.RequestedModel, final.UpstreamModel, final.Tier, final.Score, cascLabel, latency.Truncate(time.Millisecond))
		return
	}

	status, body, ctype, final, cascLabel, upErr := s.proxyBuffered(ctx, raw, dec)
	latency := time.Since(start)
	success := upErr == nil && status > 0 && status < 400
	em := ""
	if upErr != nil {
		em = upErr.Error()
	} else if !success {
		em = fmt.Sprintf("upstream status %d", status)
	}
	s.engine.RecordOutcome(final, float64(latency.Milliseconds()), success, em)

	if upErr != nil {
		s.metrics.RecordRoute(string(final.Tier), final.Passthrough, latency, true, len(raw), 0)
		writeErr(w, http.StatusBadGateway, upErr.Error(), "upstream_error")
		return
	}

	if cascLabel != "" {
		s.metrics.RecordCascade()
	}
	s.metrics.RecordRoute(string(final.Tier), final.Passthrough, latency, !success, len(raw), len(body))

	if s.cache != nil && fp != "" && success {
		s.cache.Set(fp, cache.Entry{
			Status: status, ContentType: ctype, Body: body,
			Tier: string(final.Tier), Model: final.UpstreamModel,
			Mode: string(final.Mode), Score: final.Score,
		})
	}

	s.setRouteHeaders(w, final, false, cascLabel)
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	if s.cache != nil && fp != "" {
		w.Header().Set("X-Modelrouter-Cache", "MISS")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)

	s.log.Printf("routed %s → %s tier=%s score=%.3f status=%d cascade=%q %s",
		final.RequestedModel, final.UpstreamModel, final.Tier, final.Score, status, cascLabel, latency.Truncate(time.Millisecond))
}

func (s *Server) proxyBuffered(ctx context.Context, raw []byte, dec router.Decision) (status int, body []byte, ctype string, final router.Decision, cascLabel string, err error) {
	final = dec
	attempt := func(d router.Decision) (int, []byte, string, error) {
		forward, err := rewriteModel(raw, d.UpstreamModel)
		if err != nil {
			return 0, nil, "", err
		}
		up, err := s.upstream.ProxyChat(ctx, bytes.NewReader(forward), "application/json", false)
		if err != nil {
			return 0, nil, "", err
		}
		defer up.Body.Close()
		b, readErr := io.ReadAll(up.Body)
		ct := up.Header.Get("Content-Type")
		if readErr != nil {
			return up.StatusCode, b, ct, readErr
		}
		return up.StatusCode, b, ct, nil
	}

	status, body, ctype, err = attempt(dec)
	if err == nil && !cascade.Retryable(status, nil) {
		return status, body, ctype, final, "", nil
	}
	if err != nil && !cascade.Retryable(0, err) {
		return status, body, ctype, final, "", err
	}

	s.engine.RecordOutcome(dec, 0, false, errMsg(err, status))
	next, ok := s.engine.Escalate(dec)
	if !ok {
		return status, body, ctype, final, "", err
	}
	cascLabel = cascade.Label(string(dec.Tier), string(next.Tier), dec.UpstreamModel, next.UpstreamModel)
	st2, body2, ct2, err2 := attempt(next)
	return st2, body2, ct2, next, cascLabel, err2
}

func (s *Server) proxyStream(ctx context.Context, w http.ResponseWriter, raw []byte, dec router.Decision) (final router.Decision, cascLabel string, status int, err error) {
	final = dec
	tryWrite := func(d router.Decision) (int, error) {
		forward, err := rewriteModel(raw, d.UpstreamModel)
		if err != nil {
			return 0, err
		}
		up, err := s.upstream.ProxyChat(ctx, bytes.NewReader(forward), "application/json", true)
		if err != nil {
			return 0, err
		}
		defer up.Body.Close()
		copyHopHeaders(w.Header(), up.Header)
		s.setRouteHeaders(w, d, false, cascLabel)
		w.WriteHeader(up.StatusCode)
		_, copyErr := io.Copy(w, up.Body)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return up.StatusCode, copyErr
	}

	status, err = tryWrite(dec)
	if err == nil {
		return final, "", status, nil
	}
	// Only cascade if we failed before writing (status==0). Once headers are sent, abort.
	if status != 0 {
		return final, "", status, err
	}
	s.engine.RecordOutcome(dec, 0, false, err.Error())
	next, ok := s.engine.Escalate(dec)
	if !ok {
		return final, "", 0, err
	}
	cascLabel = cascade.Label(string(dec.Tier), string(next.Tier), dec.UpstreamModel, next.UpstreamModel)
	status, err = tryWrite(next)
	return next, cascLabel, status, err
}

func errMsg(err error, status int) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("status %d", status)
}

func copyHopHeaders(dst, src http.Header) {
	for k, vv := range src {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"te", "trailers", "transfer-encoding", "upgrade", "content-length":
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func (s *Server) setRouteHeaders(w http.ResponseWriter, dec router.Decision, cacheHit bool, cascLabel string) {
	if !s.cfg.Router.ShowRouted {
		return
	}
	w.Header().Set("X-Modelrouter-Tier", string(dec.Tier))
	w.Header().Set("X-Modelrouter-Model", dec.UpstreamModel)
	w.Header().Set("X-Modelrouter-Mode", string(dec.Mode))
	w.Header().Set("X-Modelrouter-Score", fmt.Sprintf("%.3f", dec.Score))
	w.Header().Set("X-Modelrouter-Circuit", string(dec.CircuitState))
	if cascLabel != "" {
		w.Header().Set("X-Modelrouter-Cascade", cascLabel)
	}
	if cacheHit {
		w.Header().Set("X-Modelrouter-Cache", "HIT")
	}
}

func (s *Server) decide(r *http.Request) (router.Decision, []byte, error) {
	dec, raw, _, err := s.decideFull(r)
	return dec, raw, err
}

func (s *Server) decideFull(r *http.Request) (router.Decision, []byte, ChatRequest, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return router.Decision{}, nil, ChatRequest{}, fmt.Errorf("read body: %w", err)
	}
	var chat ChatRequest
	if err := json.Unmarshal(body, &chat); err != nil {
		return router.Decision{}, nil, ChatRequest{}, fmt.Errorf("invalid json: %w", err)
	}
	if chat.Model == "" {
		chat.Model = "auto"
	}

	mode := config.Mode(strings.ToLower(strings.TrimSpace(chat.OptimizeFor)))
	if mode == "" {
		mode = config.Mode(strings.ToLower(r.Header.Get("X-Optimize-For")))
	}
	switch mode {
	case config.ModeCost, config.ModeBalance, config.ModeIntelligence, "":
	default:
		return router.Decision{}, nil, ChatRequest{}, fmt.Errorf("unknown optimize_for %q", mode)
	}

	msgs := make([]classifier.Message, 0, len(chat.Messages))
	images := false
	for _, m := range chat.Messages {
		msgs = append(msgs, classifier.Message{
			Role:    m.Role,
			Content: contentText(m.Content),
		})
		if hasImage(m.Content) {
			images = true
		}
	}

	dec := s.engine.Route(router.RouteInput{
		Model: chat.Model,
		Mode:  mode,
		Req: classifier.Request{
			Messages:      msgs,
			Tools:         toolCount(chat.Tools),
			HasImages:     images,
			StructuredOut: isStructured(chat.ResponseFormat),
			StreamHint:    chat.Stream,
		},
	})
	return dec, body, chat, nil
}

func rewriteModel(raw []byte, model string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	b, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	obj["model"] = b
	delete(obj, "optimize_for")
	return json.Marshal(obj)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg, typ string) {
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{Message: msg, Type: typ}})
}

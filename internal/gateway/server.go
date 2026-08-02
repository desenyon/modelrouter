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

	"github.com/desenyon/modelrouter/internal/classifier"
	"github.com/desenyon/modelrouter/internal/config"
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
	mux      *http.ServeMux
	log      *log.Logger
}

// New constructs the gateway HTTP server.
func New(cfg config.Config, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{
		cfg:      cfg,
		engine:   router.New(cfg),
		upstream: upstream.New(cfg.Upstream.BaseURL, cfg.Upstream.APIKey, cfg.Upstream.Timeout),
		metrics:  metrics.New(),
		mux:      http.NewServeMux(),
		log:      logger,
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
	s.log.Printf("modelrouter gateway listening on %s (upstream %s, mode %s)",
		s.cfg.Listen, s.cfg.Upstream.BaseURL, s.cfg.ModeOrDefault())
	s.log.Printf("tiers luna=%s terra=%s sol=%s", s.cfg.Models.Luna, s.cfg.Models.Terra, s.cfg.Models.Sol)
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
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Optimize-For")
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
		"name":    "modelrouter",
		"role":    "intelligent model routing gateway",
		"policy":  "luna-first — sol only when the task demands it",
		"docs":    "https://github.com/desenyon/modelrouter",
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
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"upstream":         s.upstream.BaseURL(),
		"upstream_key_set": s.upstream.HasAPIKey(),
		"mode":             s.cfg.ModeOrDefault(),
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.metrics.Snapshot())
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
	dec, raw, err := s.decide(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	// Rewrite model field to the chosen upstream ID while preserving other JSON keys.
	forward, err := rewriteModel(raw, dec.UpstreamModel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	var chat ChatRequest
	_ = json.Unmarshal(raw, &chat)

	ctx := r.Context()
	if s.cfg.Upstream.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.Upstream.Timeout)
		defer cancel()
	}

	up, err := s.upstream.ProxyChat(ctx, bytes.NewReader(forward), "application/json", chat.Stream)
	if err != nil {
		s.metrics.RecordRoute(string(dec.Tier), dec.Passthrough, time.Since(start), true, len(raw), 0)
		writeErr(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	defer up.Body.Close()

	copyHopHeaders(w.Header(), up.Header)
	if s.cfg.Router.ShowRouted {
		w.Header().Set("X-Modelrouter-Tier", string(dec.Tier))
		w.Header().Set("X-Modelrouter-Model", dec.UpstreamModel)
		w.Header().Set("X-Modelrouter-Mode", string(dec.Mode))
		w.Header().Set("X-Modelrouter-Score", fmt.Sprintf("%.3f", dec.Score))
	}
	w.WriteHeader(up.StatusCode)

	n, copyErr := io.Copy(w, up.Body)
	s.metrics.RecordRoute(string(dec.Tier), dec.Passthrough, time.Since(start), copyErr != nil || up.StatusCode >= 400, len(raw), int(n))
	if copyErr != nil {
		s.log.Printf("stream copy error: %v", copyErr)
	} else {
		s.log.Printf("routed %s → %s tier=%s score=%.3f status=%d %s",
			dec.RequestedModel, dec.UpstreamModel, dec.Tier, dec.Score, up.StatusCode, time.Since(start).Truncate(time.Millisecond))
	}
}

func (s *Server) decide(r *http.Request) (router.Decision, []byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return router.Decision{}, nil, fmt.Errorf("read body: %w", err)
	}
	var chat ChatRequest
	if err := json.Unmarshal(body, &chat); err != nil {
		return router.Decision{}, nil, fmt.Errorf("invalid json: %w", err)
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
		return router.Decision{}, nil, fmt.Errorf("unknown optimize_for %q", mode)
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
			Messages:  msgs,
			Tools:     toolCount(chat.Tools),
			HasImages: images,
			StreamHint: chat.Stream,
		},
	})
	return dec, body, nil
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

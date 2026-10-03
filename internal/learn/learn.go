// Package learn closes the loop. Outcomes, explicit (POST /v1/feedback) or
// implicit (regenerations, continued conversations, invalid JSON, refusals),
// update two things:
//
//  1. Per-model, per-axis ability offsets via the gradient of the IRT
//     log-likelihood, so models that over- or under-deliver are re-ranked.
//  2. The predictor's exemplar bank: the request embedding is added with a
//     difficulty label nudged toward what the outcome implies, so similar
//     future requests are routed better.
//
// State persists to disk and is reloaded on start.
package learn

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/lru"
	"github.com/desenyon/modelrouter/internal/predict"
)

// Decision is what the learner remembers about a routed request.
type Decision struct {
	ID         string
	Vec        []float32
	Lex        predict.Lex
	Axes       catalog.Vec
	Difficulty float64
	ModelID    string
	Effort     string
	OutTokens  int
	At         time.Time
	labeled    atomic.Bool
}

// Signal sources and their default weights.
const (
	SignalFeedback     = "feedback"
	SignalRegenerate   = "regenerate"   // user asked again: previous answer likely bad
	SignalContinue     = "continue"     // user moved on: previous answer likely fine
	SignalInvalidJSON  = "invalid_json" // structured output failed
	SignalRefusal      = "refusal"
	SignalEmpty        = "empty"
	SignalInvalidTools = "invalid_tool_args"
)

var signalWeight = map[string]float64{
	SignalFeedback: 1, SignalRegenerate: 0.6, SignalContinue: 0.15,
	SignalInvalidJSON: 0.5, SignalRefusal: 0.3, SignalEmpty: 0.5, SignalInvalidTools: 0.5,
}

// Config tunes learning.
type Config struct {
	LearningRate float64
	MaxDelta     float64 // cap on learned ability offset per axis
	StatePath    string
	MaxPersisted int
}

// Learner is safe for concurrent use.
type Learner struct {
	cfg       Config
	cat       *catalog.Catalog
	pred      *predict.Predictor
	mu        sync.RWMutex
	delta     map[string]catalog.Vec
	obs       map[string]int
	decisions *lru.Cache[string, *Decision]
	convo     *lru.Cache[string, string]
	pmu       sync.Mutex
	pending   []predict.Exemplar
	counts    sync.Map // signal → *atomic.Uint64
}

// New builds a learner.
func New(cat *catalog.Catalog, pred *predict.Predictor, cfg Config) *Learner {
	if cfg.LearningRate <= 0 {
		cfg.LearningRate = 0.004
	}
	if cfg.MaxDelta <= 0 {
		cfg.MaxDelta = 0.12
	}
	if cfg.MaxPersisted <= 0 {
		cfg.MaxPersisted = 5000
	}
	return &Learner{
		cfg: cfg, cat: cat, pred: pred,
		delta: map[string]catalog.Vec{}, obs: map[string]int{},
		decisions: lru.New[string, *Decision](200_000, 0, 6*time.Hour),
		convo:     lru.New[string, string](200_000, 0, 2*time.Hour),
	}
}

// Ability returns the model's prior ability plus learned offsets.
func (l *Learner) Ability(m *catalog.Model) catalog.Vec {
	l.mu.RLock()
	d, ok := l.delta[m.ID]
	l.mu.RUnlock()
	if !ok {
		return m.Ability
	}
	v := m.Ability
	for i := range v {
		v[i] = math.Max(0, math.Min(1, v[i]+d[i]))
	}
	return v
}

// Remember stores a decision and inspects conversation fingerprints for
// implicit signals about earlier turns. full/prev come from session.Fingerprints.
func (l *Learner) Remember(d *Decision, full, prev string) []string {
	l.decisions.Put(d.ID, d, 1)
	var sigs []string
	if full != "" {
		if old, ok := l.convo.Get(full); ok && old != d.ID {
			// Identical conversation sent again → the previous answer was rejected.
			if l.applyID(old, 0, SignalRegenerate) {
				sigs = append(sigs, SignalRegenerate)
			}
		}
		l.convo.Put(full, d.ID, 1)
	}
	if prev != "" {
		if old, ok := l.convo.Get(prev); ok {
			if l.applyID(old, 1, SignalContinue) {
				sigs = append(sigs, SignalContinue)
			}
		}
	}
	return sigs
}

// ErrUnknownRequest is returned for feedback on unknown/expired ids.
var ErrUnknownRequest = errors.New("unknown or expired request id")

// Feedback applies explicit feedback: score in [0,1] (1 = good).
func (l *Learner) Feedback(id string, score float64) error {
	if score < 0 || score > 1 || math.IsNaN(score) {
		return fmt.Errorf("score must be in [0,1]")
	}
	d, ok := l.decisions.Get(id)
	if !ok {
		return ErrUnknownRequest
	}
	d.labeled.Store(false) // explicit feedback overrides implicit labels
	l.apply(d, score, SignalFeedback)
	return nil
}

// Implicit applies a negative implicit signal for a request.
func (l *Learner) Implicit(id, signal string) {
	l.applyID(id, 0, signal)
}

func (l *Learner) applyID(id string, y float64, signal string) bool {
	d, ok := l.decisions.Get(id)
	if !ok {
		return false
	}
	return l.apply(d, y, signal)
}

// apply performs one IRT gradient step and queues a learned exemplar.
// Each decision is labeled at most once by implicit signals.
func (l *Learner) apply(d *Decision, y float64, signal string) bool {
	if signal != SignalFeedback && !d.labeled.CompareAndSwap(false, true) {
		return false
	}
	m, ok := l.cat.Lookup(d.ModelID)
	if !ok {
		return false
	}
	w := signalWeight[signal]
	ab := l.Ability(m)
	theta := predict.EffectiveAbility(ab, m.DefaultEffort, d.Effort, d.Axes)
	p := predict.SuccessProb(theta, d.Difficulty)
	// ∂/∂θ_a of the Bernoulli log-likelihood = (y − p)·k·w_a; k is folded into the rate.
	step := l.cfg.LearningRate * w * (y - p) * 9
	l.mu.Lock()
	dv := l.delta[m.ID]
	for a := range dv {
		dv[a] = math.Max(-l.cfg.MaxDelta, math.Min(l.cfg.MaxDelta, dv[a]+step*d.Axes[a]))
	}
	l.delta[m.ID] = dv
	l.obs[m.ID]++
	l.mu.Unlock()

	if d.Vec != nil {
		// Difficulty implied by the outcome: success means δ ≲ θ, failure δ ≳ θ.
		target := d.Difficulty
		if y >= 0.5 {
			target = math.Min(target, theta-0.05)
		} else {
			target = math.Max(target, theta+0.05)
		}
		label := d.Difficulty + 0.5*w*(target-d.Difficulty)
		out := math.Log(math.Max(10, float64(d.OutTokens)))
		ex := predict.Exemplar{D: math.Max(0, math.Min(1, label)), Axes: d.Axes, LogOut: out, Vec: d.Vec, Lex: d.Lex, Weight: 0.5 * w, Learned: true}
		l.pmu.Lock()
		l.pending = append(l.pending, ex)
		l.pmu.Unlock()
	}
	c, _ := l.counts.LoadOrStore(signal, new(atomic.Uint64))
	c.(*atomic.Uint64).Add(1)
	return true
}

// Flush moves queued exemplars into the predictor and refits its ridge head.
func (l *Learner) Flush() int {
	l.pmu.Lock()
	ex := l.pending
	l.pending = nil
	l.pmu.Unlock()
	if len(ex) == 0 {
		return 0
	}
	l.pred.AddLearned(ex, true)
	return len(ex)
}

// Stats is a JSON view of learning state.
type Stats struct {
	Signals  map[string]uint64             `json:"signals"`
	Learned  int                           `json:"learned_exemplars"`
	Offsets  map[string]map[string]float64 `json:"ability_offsets"`
	Observed map[string]int                `json:"observations"`
}

// Stats returns learning counters and ability offsets.
func (l *Learner) Stats() Stats {
	s := Stats{Signals: map[string]uint64{}, Offsets: map[string]map[string]float64{}, Observed: map[string]int{}}
	l.counts.Range(func(k, v any) bool {
		s.Signals[k.(string)] = v.(*atomic.Uint64).Load()
		return true
	})
	_, s.Learned = l.pred.Size()
	l.mu.RLock()
	for id, d := range l.delta {
		s.Offsets[id] = d.Map()
		s.Observed[id] = l.obs[id]
	}
	l.mu.RUnlock()
	return s
}

type persisted struct {
	Version   int                           `json:"version"`
	Embedder  string                        `json:"embedder"`
	Offsets   map[string][]float64          `json:"offsets"`
	Observed  map[string]int                `json:"observed"`
	Exemplars []persistedExemplar           `json:"exemplars"`
	Signals   map[string]uint64             `json:"signals,omitempty"`
	Extra     map[string]map[string]float64 `json:"-"`
}

type persistedExemplar struct {
	D      float64     `json:"d"`
	Axes   []float64   `json:"a"`
	LogOut float64     `json:"o"`
	Vec    []float32   `json:"v"`
	Lex    predict.Lex `json:"l"`
	W      float64     `json:"w"`
}

// Save writes learned state atomically.
func (l *Learner) Save(embedder string) error {
	if l.cfg.StatePath == "" {
		return nil
	}
	l.Flush()
	p := persisted{Version: 1, Embedder: embedder, Offsets: map[string][]float64{}, Observed: map[string]int{}}
	l.mu.RLock()
	for id, d := range l.delta {
		p.Offsets[id] = d[:]
		p.Observed[id] = l.obs[id]
	}
	l.mu.RUnlock()
	ex := l.pred.Learned()
	if len(ex) > l.cfg.MaxPersisted {
		ex = ex[len(ex)-l.cfg.MaxPersisted:]
	}
	for _, e := range ex {
		p.Exemplars = append(p.Exemplars, persistedExemplar{D: e.D, Axes: e.Axes[:], LogOut: e.LogOut, Vec: e.Vec, Lex: e.Lex, W: e.Weight})
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.cfg.StatePath), 0o755); err != nil {
		return err
	}
	tmp := l.cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.cfg.StatePath)
}

// Load restores learned state (missing file is not an error). Exemplars
// embedded by a different embedder are discarded.
func (l *Learner) Load(embedder string) error {
	if l.cfg.StatePath == "" {
		return nil
	}
	b, err := os.ReadFile(l.cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("learn state: %w", err)
	}
	l.mu.Lock()
	for id, d := range p.Offsets {
		var v catalog.Vec
		copy(v[:], d)
		l.delta[id] = v
		l.obs[id] = p.Observed[id]
	}
	l.mu.Unlock()
	if p.Embedder != embedder {
		return nil
	}
	ex := make([]predict.Exemplar, 0, len(p.Exemplars))
	for _, e := range p.Exemplars {
		var ax catalog.Vec
		copy(ax[:], e.Axes)
		ex = append(ex, predict.Exemplar{D: e.D, Axes: ax, LogOut: e.LogOut, Vec: e.Vec, Lex: e.Lex, Weight: e.W, Learned: true})
	}
	l.pred.AddLearned(ex, true)
	return nil
}

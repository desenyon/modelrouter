// Package health tracks per-model circuit breakers and EWMA latency.
package health

import (
	"sync"
	"time"
)

// State is the circuit breaker state for one upstream model.
type State string

const (
	Closed   State = "closed"
	Open     State = "open"
	HalfOpen State = "half_open"
)

// Tracker holds health for many models.
type Tracker struct {
	mu       sync.RWMutex
	models   map[string]*modelHealth
	failOpen int           // consecutive failures to open
	coolDown time.Duration // time before half-open probe
	alpha    float64       // EWMA alpha for latency
}

type modelHealth struct {
	state        State
	failures     int
	successes    int
	openedAt     time.Time
	ewmaLatency  float64 // ms
	latencySamples int
	lastError    string
}

// New creates a tracker with sensible defaults.
func New() *Tracker {
	return &Tracker{
		models:   make(map[string]*modelHealth),
		failOpen: 3,
		coolDown: 30 * time.Second,
		alpha:    0.3,
	}
}

func (t *Tracker) get(model string) *modelHealth {
	h, ok := t.models[model]
	if !ok {
		h = &modelHealth{state: Closed}
		t.models[model] = h
	}
	return h
}

// Allow reports whether a model may receive traffic.
func (t *Tracker) Allow(model string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.get(model)
	switch h.state {
	case Open:
		if time.Since(h.openedAt) >= t.coolDown {
			h.state = HalfOpen
			return true
		}
		return false
	default:
		return true
	}
}

// StateOf returns the circuit state for a model.
func (t *Tracker) StateOf(model string) State {
	t.mu.RLock()
	defer t.mu.RUnlock()
	h, ok := t.models[model]
	if !ok {
		return Closed
	}
	if h.state == Open && time.Since(h.openedAt) >= t.coolDown {
		return HalfOpen
	}
	return h.state
}

// RecordSuccess notes a healthy response.
func (t *Tracker) RecordSuccess(model string, latency time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.get(model)
	ms := float64(latency.Milliseconds())
	if h.latencySamples == 0 {
		h.ewmaLatency = ms
	} else {
		h.ewmaLatency = t.alpha*ms + (1-t.alpha)*h.ewmaLatency
	}
	h.latencySamples++
	h.failures = 0
	h.successes++
	h.state = Closed
	h.lastError = ""
}

// RecordFailure notes a retryable/upstream failure.
func (t *Tracker) RecordFailure(model string, errMsg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.get(model)
	h.failures++
	h.lastError = errMsg
	if h.state == HalfOpen || h.failures >= t.failOpen {
		h.state = Open
		h.openedAt = time.Now()
	}
}

// LatencyEWMA returns smoothed latency in ms (0 if unknown).
func (t *Tracker) LatencyEWMA(model string) float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	h, ok := t.models[model]
	if !ok {
		return 0
	}
	return h.ewmaLatency
}

// Snapshot is a JSON-friendly health dump.
type Snapshot struct {
	Model     string  `json:"model"`
	State     State   `json:"state"`
	Failures  int     `json:"failures"`
	Successes int     `json:"successes"`
	EwmaMs    float64 `json:"ewma_latency_ms"`
	LastError string  `json:"last_error,omitempty"`
}

// SnapshotAll returns health for all known models.
func (t *Tracker) SnapshotAll() []Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Snapshot, 0, len(t.models))
	for id, h := range t.models {
		st := h.state
		if st == Open && time.Since(h.openedAt) >= t.coolDown {
			st = HalfOpen
		}
		out = append(out, Snapshot{
			Model: id, State: st, Failures: h.failures, Successes: h.successes,
			EwmaMs: h.ewmaLatency, LastError: h.lastError,
		})
	}
	return out
}

// OpenCount returns how many circuits are currently open.
func (t *Tracker) OpenCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n := 0
	for _, h := range t.models {
		if h.state == Open && time.Since(h.openedAt) < t.coolDown {
			n++
		}
	}
	return n
}

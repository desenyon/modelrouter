// Package health tracks per-model reliability and speed: a time-windowed
// circuit breaker (single-probe half-open, exponential backoff), rate-limit
// cooldowns from Retry-After, provider/model disablement on auth/404, and
// EWMA time-to-first-token and output throughput.
package health

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// State is a breaker state.
type State string

const (
	Closed   State = "closed"
	Open     State = "open"
	HalfOpen State = "half_open"
)

// Config tunes the breaker.
type Config struct {
	Window        time.Duration // failure-rate window
	MinRequests   int           // minimum samples in window before tripping on rate
	FailureRate   float64       // trip when failures/total ≥ this
	Consecutive   int           // or this many consecutive failures
	BaseCooldown  time.Duration // first open duration
	MaxCooldown   time.Duration
	NotFoundBlock time.Duration // how long a 404'd model stays disabled
	AuthBlock     time.Duration // how long an auth-failed provider stays disabled
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		Window: 60 * time.Second, MinRequests: 6, FailureRate: 0.5, Consecutive: 3,
		BaseCooldown: 15 * time.Second, MaxCooldown: 5 * time.Minute,
		NotFoundBlock: 10 * time.Minute, AuthBlock: 2 * time.Minute,
	}
}

const buckets = 12

type bucket struct {
	at       int64 // unix seconds / bucket width
	ok, fail int32
}

type modelState struct {
	mu          sync.Mutex
	b           [buckets]bucket
	consecutive int
	state       State
	openUntil   time.Time
	cooldown    time.Duration
	probing     atomic.Bool
	limitedTill time.Time
	disabledTil time.Time
	lastErr     string

	ttft, tps     float64 // EWMA ms, tokens/s
	ttftN, tpsN   int
	successes     uint64
	failures      uint64
	remainingReqs int64
	remainingToks int64
	headroomAt    time.Time
}

// Tracker is safe for concurrent use.
type Tracker struct {
	cfg       Config
	models    sync.Map // id → *modelState
	providers sync.Map // name → time.Time (disabled until)
	now       func() time.Time
}

// New creates a tracker.
func New(cfg Config) *Tracker {
	if cfg.Window == 0 {
		cfg = DefaultConfig()
	}
	return &Tracker{cfg: cfg, now: time.Now}
}

// SetClock overrides time (tests).
func (t *Tracker) SetClock(f func() time.Time) { t.now = f }

func (t *Tracker) get(id string) *modelState {
	if v, ok := t.models.Load(id); ok {
		return v.(*modelState)
	}
	v, _ := t.models.LoadOrStore(id, &modelState{state: Closed})
	return v.(*modelState)
}

// Available reports whether a model may be planned (no side effects):
// not disabled, not rate-limited, breaker not open (half-open counts as available).
func (t *Tracker) Available(provider, id string) (bool, string) {
	now := t.now()
	if v, ok := t.providers.Load(provider); ok && now.Before(v.(time.Time)) {
		return false, "provider_auth_failed"
	}
	s := t.get(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case now.Before(s.disabledTil):
		return false, "model_not_found_upstream"
	case now.Before(s.limitedTill):
		return false, "rate_limited"
	case s.state == Open && now.Before(s.openUntil):
		return false, "circuit_open"
	case s.state == HalfOpen && s.probing.Load():
		return false, "circuit_probing"
	}
	return true, ""
}

// Acquire is called right before dispatching to a model. In half-open state
// exactly one caller wins the probe; others are refused.
func (t *Tracker) Acquire(provider, id string) bool {
	ok, _ := t.Available(provider, id)
	if !ok {
		return false
	}
	s := t.get(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == Open && !t.now().Before(s.openUntil) {
		s.state = HalfOpen
	}
	if s.state == HalfOpen {
		return s.probing.CompareAndSwap(false, true)
	}
	return true
}

// Outcome describes one finished attempt.
type Outcome struct {
	OK           bool
	HealthFail   bool // counts toward the breaker
	RateLimited  bool
	RetryAfter   time.Duration
	NotFound     bool
	AuthFailed   bool
	TTFT         time.Duration // 0 if no token arrived
	Duration     time.Duration
	OutputTokens int
	Err          string
}

// Record applies an outcome.
func (t *Tracker) Record(provider, id string, o Outcome) {
	now := t.now()
	if o.AuthFailed {
		t.providers.Store(provider, now.Add(t.cfg.AuthBlock))
	}
	s := t.get(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	wasProbe := s.state == HalfOpen && s.probing.Load()
	if wasProbe {
		s.probing.Store(false)
	}
	if o.TTFT > 0 {
		ms := float64(o.TTFT.Microseconds()) / 1000
		s.ttft = ewma(s.ttft, ms, s.ttftN)
		s.ttftN++
	}
	if o.OK && o.OutputTokens > 20 && o.Duration > o.TTFT {
		gen := (o.Duration - o.TTFT).Seconds()
		if gen > 0.05 {
			s.tps = ewma(s.tps, float64(o.OutputTokens)/gen, s.tpsN)
			s.tpsN++
		}
	}
	switch {
	case o.NotFound:
		s.disabledTil = now.Add(t.cfg.NotFoundBlock)
		s.lastErr = o.Err
		return
	case o.RateLimited:
		ra := o.RetryAfter
		if ra <= 0 {
			ra = 10 * time.Second
		}
		if ra > 2*time.Minute {
			ra = 2 * time.Minute
		}
		s.limitedTill = now.Add(ra)
		s.lastErr = o.Err
		return
	}
	b := s.bucketFor(now, t.cfg.Window)
	if o.OK {
		b.ok++
		s.successes++
		s.consecutive = 0
		if s.state != Closed {
			s.state, s.cooldown = Closed, 0
		}
		return
	}
	if !o.HealthFail {
		return
	}
	b.fail++
	s.failures++
	s.consecutive++
	s.lastErr = o.Err
	if wasProbe || s.state == HalfOpen {
		s.trip(now, t.cfg)
		return
	}
	ok, fail := s.window(now, t.cfg.Window)
	total := ok + fail
	if s.consecutive >= t.cfg.Consecutive || (total >= t.cfg.MinRequests && float64(fail)/float64(total) >= t.cfg.FailureRate) {
		s.trip(now, t.cfg)
	}
}

func (s *modelState) trip(now time.Time, cfg Config) {
	if s.cooldown == 0 {
		s.cooldown = cfg.BaseCooldown
	} else {
		s.cooldown *= 2
		if s.cooldown > cfg.MaxCooldown {
			s.cooldown = cfg.MaxCooldown
		}
	}
	s.state = Open
	s.openUntil = now.Add(s.cooldown)
}

func (s *modelState) bucketFor(now time.Time, window time.Duration) *bucket {
	width := int64(window/time.Second) / buckets
	if width < 1 {
		width = 1
	}
	slot := now.Unix() / width
	b := &s.b[slot%buckets]
	if b.at != slot {
		*b = bucket{at: slot}
	}
	return b
}

func (s *modelState) window(now time.Time, window time.Duration) (ok, fail int) {
	width := int64(window/time.Second) / buckets
	if width < 1 {
		width = 1
	}
	cur := now.Unix() / width
	for _, b := range s.b {
		if cur-b.at < buckets {
			ok += int(b.ok)
			fail += int(b.fail)
		}
	}
	return
}

// ObserveHeadroom records rate-limit headroom reported by upstream headers.
func (t *Tracker) ObserveHeadroom(id string, remainingRequests, remainingTokens int64) {
	if remainingRequests < 0 && remainingTokens < 0 {
		return
	}
	s := t.get(id)
	s.mu.Lock()
	s.remainingReqs, s.remainingToks, s.headroomAt = remainingRequests, remainingTokens, t.now()
	s.mu.Unlock()
}

// Speed returns observed TTFT (ms) and throughput (tokens/s) with sample counts.
func (t *Tracker) Speed(id string) (ttftMs, tps float64, nTTFT, nTPS int) {
	s := t.get(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttft, s.tps, s.ttftN, s.tpsN
}

// TokenHeadroom returns the last reported remaining-token budget (-1 unknown or stale).
func (t *Tracker) TokenHeadroom(id string) int64 {
	s := t.get(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headroomAt.IsZero() || t.now().Sub(s.headroomAt) > 30*time.Second {
		return -1
	}
	return s.remainingToks
}

// Snapshot is a JSON view of one model's health.
type Snapshot struct {
	Model       string  `json:"model"`
	State       State   `json:"state"`
	Successes   uint64  `json:"successes"`
	Failures    uint64  `json:"failures"`
	TTFTms      float64 `json:"ttft_ms_ewma"`
	TPS         float64 `json:"tokens_per_sec_ewma"`
	RateLimited bool    `json:"rate_limited,omitempty"`
	Disabled    bool    `json:"disabled,omitempty"`
	OpenFor     float64 `json:"open_for_s,omitempty"`
	LastError   string  `json:"last_error,omitempty"`
}

// SnapshotAll returns health for every model seen.
func (t *Tracker) SnapshotAll() []Snapshot {
	now := t.now()
	var out []Snapshot
	t.models.Range(func(k, v any) bool {
		s := v.(*modelState)
		s.mu.Lock()
		st := s.state
		snap := Snapshot{Model: k.(string), Successes: s.successes, Failures: s.failures,
			TTFTms: math.Round(s.ttft), TPS: math.Round(s.tps*10) / 10, LastError: s.lastErr,
			RateLimited: now.Before(s.limitedTill), Disabled: now.Before(s.disabledTil)}
		if st == Open {
			if now.Before(s.openUntil) {
				snap.OpenFor = math.Round(s.openUntil.Sub(now).Seconds())
			} else {
				st = HalfOpen
			}
		}
		snap.State = st
		s.mu.Unlock()
		out = append(out, snap)
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// OpenCount returns how many breakers are open.
func (t *Tracker) OpenCount() int {
	n := 0
	for _, s := range t.SnapshotAll() {
		if s.State == Open {
			n++
		}
	}
	return n
}

func ewma(prev, x float64, n int) float64 {
	if n == 0 {
		return x
	}
	alpha := 0.2
	if n < 5 {
		alpha = 1 / float64(n+1) // plain mean while warming up
	}
	return alpha*x + (1-alpha)*prev
}

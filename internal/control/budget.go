// Package control implements the spend controller: an online dual-ascent
// update of the cost multiplier λ in the optimizer's utility so that the
// observed spend rate tracks a target $/hour. Overspending raises λ (routing
// shifts to cheaper arms); underspending relaxes it toward 1.
package control

import (
	"math"
	"sync"
	"time"
)

// Budget is safe for concurrent use.
type Budget struct {
	mu        sync.Mutex
	target    float64 // USD per hour; 0 disables
	eta       float64
	lambda    float64
	window    float64 // USD accumulated since last tick
	rateEWMA  float64 // USD/hour
	lastTick  time.Time
	total     float64
	minLambda float64
	maxLambda float64
	now       func() time.Time
}

// NewBudget creates a controller. usdPerHour ≤ 0 disables control (λ = 1).
func NewBudget(usdPerHour float64) *Budget {
	return &Budget{target: usdPerHour, eta: 0.3, lambda: 1, minLambda: 1, maxLambda: 16, now: time.Now, lastTick: time.Now()}
}

// Record adds actual spend.
func (b *Budget) Record(usd float64) {
	if usd <= 0 || math.IsNaN(usd) {
		return
	}
	b.mu.Lock()
	b.window += usd
	b.total += usd
	b.mu.Unlock()
}

// Tick updates λ from the spend since the previous tick. Call periodically.
func (b *Budget) Tick() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	dt := now.Sub(b.lastTick).Hours()
	if dt <= 0 {
		return
	}
	rate := b.window / dt
	b.window, b.lastTick = 0, now
	if b.rateEWMA == 0 {
		b.rateEWMA = rate
	} else {
		b.rateEWMA = 0.3*rate + 0.7*b.rateEWMA
	}
	if b.target <= 0 {
		b.lambda = 1
		return
	}
	// Multiplicative dual ascent on log λ, clamped.
	ratio := b.rateEWMA / b.target
	b.lambda *= math.Exp(b.eta * math.Max(-1, math.Min(1, ratio-1)))
	b.lambda = math.Max(b.minLambda, math.Min(b.maxLambda, b.lambda))
}

// Lambda returns the current cost multiplier.
func (b *Budget) Lambda() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lambda
}

// Snapshot is a JSON view.
type Snapshot struct {
	TargetUSDPerHour float64 `json:"target_usd_per_hour"`
	RateUSDPerHour   float64 `json:"rate_usd_per_hour"`
	Lambda           float64 `json:"lambda"`
	TotalUSD         float64 `json:"total_usd"`
}

// Snapshot returns controller state.
func (b *Budget) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Snapshot{TargetUSDPerHour: b.target, RateUSDPerHour: b.rateEWMA, Lambda: b.lambda, TotalUSD: b.total}
}

// SetClock overrides time (tests).
func (b *Budget) SetClock(f func() time.Time) {
	b.mu.Lock()
	b.now, b.lastTick = f, f()
	b.mu.Unlock()
}

// RetryBudget is a token bucket that caps retries/hedges to a fraction of
// primary traffic, preventing retry storms during provider incidents.
type RetryBudget struct {
	mu     sync.Mutex
	tokens float64
	ratio  float64
	max    float64
}

// NewRetryBudget allows roughly ratio retries per primary request, with a burst of 10.
func NewRetryBudget(ratio float64) *RetryBudget {
	if ratio <= 0 {
		ratio = 0.2
	}
	return &RetryBudget{tokens: 10, ratio: ratio, max: 10 + 100*ratio}
}

// Deposit credits one primary request.
func (r *RetryBudget) Deposit() {
	r.mu.Lock()
	r.tokens = math.Min(r.max, r.tokens+r.ratio)
	r.mu.Unlock()
}

// Take consumes one retry if available.
func (r *RetryBudget) Take() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tokens >= 1-1e-9 {
		r.tokens--
		return true
	}
	return false
}

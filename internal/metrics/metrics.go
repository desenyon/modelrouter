// Package metrics keeps lightweight in-process routing counters.
package metrics

import (
	"sync"
	"sync/atomic"
	"time"
)

// Collector tracks gateway volume, tier mix, cache, and cascade.
type Collector struct {
	started time.Time

	requests    atomic.Uint64
	errors      atomic.Uint64
	bytesIn     atomic.Uint64
	bytesOut    atomic.Uint64
	luna        atomic.Uint64
	terra       atomic.Uint64
	sol         atomic.Uint64
	passthrough atomic.Uint64
	cacheHits   atomic.Uint64
	cascades    atomic.Uint64

	mu           sync.Mutex
	latencyN     int64
	latencySumMs int64
}

// New creates a collector.
func New() *Collector {
	return &Collector{started: time.Now().UTC()}
}

// RecordRoute increments tier counters.
func (c *Collector) RecordRoute(tier string, passthrough bool, latency time.Duration, err bool, in, out int) {
	c.requests.Add(1)
	if err {
		c.errors.Add(1)
	}
	c.bytesIn.Add(uint64(in))
	c.bytesOut.Add(uint64(out))
	if passthrough {
		c.passthrough.Add(1)
	} else {
		switch tier {
		case "luna":
			c.luna.Add(1)
		case "terra":
			c.terra.Add(1)
		case "sol":
			c.sol.Add(1)
		}
	}
	c.mu.Lock()
	c.latencyN++
	c.latencySumMs += latency.Milliseconds()
	c.mu.Unlock()
}

// RecordCacheHit notes a fingerprint cache hit.
func (c *Collector) RecordCacheHit() { c.cacheHits.Add(1) }

// RecordCascade notes a tier/model escalate.
func (c *Collector) RecordCascade() { c.cascades.Add(1) }

// Snapshot is a JSON-friendly metrics dump.
type Snapshot struct {
	UptimeSeconds float64 `json:"uptime_seconds"`
	Requests      uint64  `json:"requests"`
	Errors        uint64  `json:"errors"`
	BytesIn       uint64  `json:"bytes_in"`
	BytesOut      uint64  `json:"bytes_out"`
	Luna          uint64  `json:"luna"`
	Terra         uint64  `json:"terra"`
	Sol           uint64  `json:"sol"`
	Passthrough   uint64  `json:"passthrough"`
	CacheHits     uint64  `json:"cache_hits"`
	Cascades      uint64  `json:"cascades"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	SolShare      float64 `json:"sol_share"`
	LunaShare     float64 `json:"luna_share"`
	CascadeRate   float64 `json:"cascade_rate"`
}

// Snapshot returns current counters.
func (c *Collector) Snapshot() Snapshot {
	req := c.requests.Load()
	luna := c.luna.Load()
	terra := c.terra.Load()
	sol := c.sol.Load()
	routed := luna + terra + sol
	cascades := c.cascades.Load()

	c.mu.Lock()
	n, sum := c.latencyN, c.latencySumMs
	c.mu.Unlock()

	var avg float64
	if n > 0 {
		avg = float64(sum) / float64(n)
	}
	var solShare, lunaShare, cascadeRate float64
	if routed > 0 {
		solShare = float64(sol) / float64(routed)
		lunaShare = float64(luna) / float64(routed)
	}
	if req > 0 {
		cascadeRate = float64(cascades) / float64(req)
	}

	return Snapshot{
		UptimeSeconds: time.Since(c.started).Seconds(),
		Requests:      req,
		Errors:        c.errors.Load(),
		BytesIn:       c.bytesIn.Load(),
		BytesOut:      c.bytesOut.Load(),
		Luna:          luna,
		Terra:         terra,
		Sol:           sol,
		Passthrough:   c.passthrough.Load(),
		CacheHits:     c.cacheHits.Load(),
		Cascades:      cascades,
		AvgLatencyMs:  avg,
		SolShare:      solShare,
		LunaShare:     lunaShare,
		CascadeRate:   cascadeRate,
	}
}

package health

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) adv(d time.Duration) { c.t = c.t.Add(d) }
func newT() (*Tracker, *clock) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	tr := New(DefaultConfig())
	tr.SetClock(c.now)
	return tr, c
}

func TestTripsOnConsecutiveFailures(t *testing.T) {
	tr, _ := newT()
	for i := 0; i < 3; i++ {
		tr.Record("p", "m", Outcome{HealthFail: true, Err: "500"})
	}
	if ok, why := tr.Available("p", "m"); ok || why != "circuit_open" {
		t.Fatalf("want open, got ok=%v why=%s", ok, why)
	}
}

func TestHalfOpenAllowsExactlyOneProbe(t *testing.T) {
	tr, c := newT()
	for i := 0; i < 3; i++ {
		tr.Record("p", "m", Outcome{HealthFail: true})
	}
	c.adv(16 * time.Second)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tr.Acquire("p", "m") {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected exactly one probe, got %d", wins.Load())
	}
	tr.Record("p", "m", Outcome{OK: true})
	if !tr.Acquire("p", "m") || !tr.Acquire("p", "m") {
		t.Fatal("breaker should close after successful probe")
	}
}

func TestFailedProbeBacksOffExponentially(t *testing.T) {
	tr, c := newT()
	for i := 0; i < 3; i++ {
		tr.Record("p", "m", Outcome{HealthFail: true})
	}
	c.adv(16 * time.Second)
	if !tr.Acquire("p", "m") {
		t.Fatal("probe should be allowed")
	}
	tr.Record("p", "m", Outcome{HealthFail: true})
	c.adv(20 * time.Second) // < 30s doubled cooldown
	if ok, _ := tr.Available("p", "m"); ok {
		t.Fatal("cooldown should have doubled")
	}
	c.adv(11 * time.Second)
	if ok, _ := tr.Available("p", "m"); !ok {
		t.Fatal("should be half-open after doubled cooldown")
	}
}

func TestRateLimitIsCooldownNotFailure(t *testing.T) {
	tr, c := newT()
	tr.Record("p", "m", Outcome{RateLimited: true, RetryAfter: 5 * time.Second})
	if ok, why := tr.Available("p", "m"); ok || why != "rate_limited" {
		t.Fatalf("want rate_limited, got %v %s", ok, why)
	}
	c.adv(6 * time.Second)
	if ok, _ := tr.Available("p", "m"); !ok {
		t.Fatal("cooldown should expire")
	}
	if s := tr.SnapshotAll()[0]; s.Failures != 0 || s.State != Closed {
		t.Fatalf("429 must not count as health failure: %+v", s)
	}
}

func TestNonHealthFailuresIgnored(t *testing.T) {
	tr, _ := newT()
	for i := 0; i < 10; i++ {
		tr.Record("p", "m", Outcome{Err: "client canceled"})
	}
	if ok, _ := tr.Available("p", "m"); !ok {
		t.Fatal("client cancellations must not trip the breaker")
	}
}

func TestAuthFailureDisablesProvider(t *testing.T) {
	tr, _ := newT()
	tr.Record("anthropic", "a", Outcome{AuthFailed: true})
	if ok, why := tr.Available("anthropic", "b"); ok || why != "provider_auth_failed" {
		t.Fatalf("provider should be disabled: %v %s", ok, why)
	}
}

func TestSpeedEWMA(t *testing.T) {
	tr, _ := newT()
	tr.Record("p", "m", Outcome{OK: true, TTFT: 400 * time.Millisecond, Duration: 2400 * time.Millisecond, OutputTokens: 200})
	ttft, tps, _, _ := tr.Speed("m")
	if ttft != 400 || tps != 100 {
		t.Fatalf("ttft=%v tps=%v", ttft, tps)
	}
}

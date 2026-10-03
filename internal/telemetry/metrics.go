// Package telemetry provides dependency-free Prometheus metrics and an
// asynchronous JSONL decision log (the training data for offline analysis).
package telemetry

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds metric families and renders Prometheus text format.
type Registry struct {
	mu       sync.Mutex
	families []family
}

type family interface {
	write(w io.Writer)
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// WritePrometheus renders all metrics.
func (r *Registry) WritePrometheus(w io.Writer) {
	r.mu.Lock()
	fs := append([]family(nil), r.families...)
	r.mu.Unlock()
	for _, f := range fs {
		f.write(w)
	}
}

func (r *Registry) add(f family) {
	r.mu.Lock()
	r.families = append(r.families, f)
	r.mu.Unlock()
}

// CounterVec is a labeled float counter.
type CounterVec struct {
	name, help string
	labels     []string
	m          sync.Map // key → *atomic.Uint64 (float bits)
}

// NewCounterVec registers a counter family.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, labels: labels}
	r.add(c)
	return c
}

// Add increments the series identified by label values.
func (c *CounterVec) Add(v float64, lv ...string) {
	key := strings.Join(lv, "\x00")
	p, ok := c.m.Load(key)
	if !ok {
		p, _ = c.m.LoadOrStore(key, new(atomic.Uint64))
	}
	u := p.(*atomic.Uint64)
	for {
		old := u.Load()
		nv := math.Float64bits(math.Float64frombits(old) + v)
		if u.CompareAndSwap(old, nv) {
			return
		}
	}
}

// Inc adds 1.
func (c *CounterVec) Inc(lv ...string) { c.Add(1, lv...) }

func (c *CounterVec) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	writeSeries(w, c.name, c.labels, &c.m, func(v any) float64 { return math.Float64frombits(v.(*atomic.Uint64).Load()) })
}

// GaugeFunc reports a value computed at scrape time.
type GaugeFunc struct {
	name, help string
	labels     []string
	fn         func() map[string]float64 // label-key → value
}

// NewGaugeFunc registers a gauge whose series come from fn (keys are label
// values joined by \x00; use "" for an unlabeled gauge).
func (r *Registry) NewGaugeFunc(name, help string, fn func() map[string]float64, labels ...string) {
	r.add(&GaugeFunc{name: name, help: help, labels: labels, fn: fn})
}

func (g *GaugeFunc) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
	vals := g.fn()
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %g\n", g.name, labelStr(g.labels, k), vals[k])
	}
}

// HistogramVec is a labeled histogram with fixed buckets.
type HistogramVec struct {
	name, help string
	labels     []string
	buckets    []float64
	m          sync.Map // key → *hist
}

type hist struct {
	mu     sync.Mutex
	counts []uint64
	sum    float64
	n      uint64
}

// NewHistogramVec registers a histogram family.
func (r *Registry) NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	h := &HistogramVec{name: name, help: help, labels: labels, buckets: buckets}
	r.add(h)
	return h
}

// Observe records v.
func (h *HistogramVec) Observe(v float64, lv ...string) {
	key := strings.Join(lv, "\x00")
	p, ok := h.m.Load(key)
	if !ok {
		p, _ = h.m.LoadOrStore(key, &hist{counts: make([]uint64, len(h.buckets))})
	}
	x := p.(*hist)
	x.mu.Lock()
	for i, b := range h.buckets {
		if v <= b {
			x.counts[i]++
		}
	}
	x.sum += v
	x.n++
	x.mu.Unlock()
}

func (h *HistogramVec) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	var keys []string
	h.m.Range(func(k, _ any) bool { keys = append(keys, k.(string)); return true })
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := h.m.Load(k)
		x := v.(*hist)
		x.mu.Lock()
		for i, b := range h.buckets {
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelStrExtra(h.labels, k, "le", fmt.Sprintf("%g", b)), x.counts[i])
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelStrExtra(h.labels, k, "le", "+Inf"), x.n)
		fmt.Fprintf(w, "%s_sum%s %g\n%s_count%s %d\n", h.name, labelStr(h.labels, k), x.sum, h.name, labelStr(h.labels, k), x.n)
		x.mu.Unlock()
	}
}

func writeSeries(w io.Writer, name string, labels []string, m *sync.Map, val func(any) float64) {
	var keys []string
	m.Range(func(k, _ any) bool { keys = append(keys, k.(string)); return true })
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := m.Load(k)
		fmt.Fprintf(w, "%s%s %g\n", name, labelStr(labels, k), val(v))
	}
}

func labelStr(names []string, key string) string { return labelStrExtra(names, key, "", "") }

func labelStrExtra(names []string, key, en, ev string) string {
	if len(names) == 0 && en == "" {
		return ""
	}
	var vals []string
	if len(names) > 0 {
		vals = strings.Split(key, "\x00")
	}
	var b strings.Builder
	b.WriteByte('{')
	n := 0
	for i, nm := range names {
		if i >= len(vals) {
			break
		}
		if n > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", nm, vals[i])
		n++
	}
	if en != "" {
		if n > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", en, ev)
	}
	b.WriteByte('}')
	return b.String()
}

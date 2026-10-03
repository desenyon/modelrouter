// Package cache is an exact-match response cache with byte-bounded LRU
// eviction, TTL, and singleflight de-duplication of identical in-flight
// requests. Keys cover every generation-affecting parameter, and only
// deterministic requests (temperature 0 or a fixed seed) or explicit opt-ins
// are cached.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/lru"
)

// Entry is a cached completion plus the routing outcome that produced it.
type Entry struct {
	Resp    canon.Response
	ModelID string
	Tier    string
	Effort  string
	CostUSD float64
	At      time.Time
}

// Store is safe for concurrent use.
type Store struct {
	c      *lru.Cache[string, Entry]
	hits   atomic.Uint64
	misses atomic.Uint64
	mu     sync.Mutex
	flight map[string]*call
}

type call struct {
	wg  sync.WaitGroup
	ent Entry
	ok  bool
}

// New builds a store bounded by maxBytes.
func New(maxBytes int64, ttl time.Duration) *Store {
	return &Store{c: lru.New[string, Entry](0, maxBytes, ttl), flight: map[string]*call{}}
}

// Cacheable reports whether a request is deterministic enough to cache.
func Cacheable(r *canon.Request) bool {
	if r.Router.NoCache {
		return false
	}
	if r.Router.ForceCache {
		return true
	}
	return (r.Temperature != nil && *r.Temperature == 0) || r.Seed != nil
}

// Key hashes every field that can change the completion, plus the routing
// constraints (so a quality-mode request never gets a cost-mode answer).
func Key(r *canon.Request, routeKey string) string {
	type k struct {
		Route   string                `json:"r"`
		Msgs    []canon.Message       `json:"m"`
		Tools   []canon.Tool          `json:"t,omitempty"`
		TC      canon.ToolChoice      `json:"tc"`
		PTC     *bool                 `json:"p,omitempty"`
		RF      *canon.ResponseFormat `json:"f,omitempty"`
		Max     int                   `json:"x,omitempty"`
		Temp    *float64              `json:"te,omitempty"`
		TopP    *float64              `json:"tp,omitempty"`
		Stop    []string              `json:"s,omitempty"`
		Seed    *int64                `json:"sd,omitempty"`
		Effort  string                `json:"e,omitempty"`
		Allow   []string              `json:"a,omitempty"`
		Deny    []string              `json:"d,omitempty"`
		MinQ    float64               `json:"q,omitempty"`
		MaxCost float64               `json:"c,omitempty"`
	}
	b, _ := json.Marshal(k{routeKey, r.Messages, r.Tools, r.ToolChoice, r.ParallelToolCalls, r.ResponseFormat,
		r.MaxTokens, r.Temperature, r.TopP, r.Stop, r.Seed, r.ReasoningEffort,
		r.Router.Allow, r.Router.Deny, r.Router.MinQuality, r.Router.MaxCostUSD})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Get returns a cached entry.
func (s *Store) Get(key string) (Entry, bool) {
	e, ok := s.c.Get(key)
	if ok {
		s.hits.Add(1)
	} else {
		s.misses.Add(1)
	}
	return e, ok
}

// Put stores an entry; cost is approximated by payload size.
func (s *Store) Put(key string, e Entry) {
	size := int64(len(e.Resp.Content) + len(e.Resp.Refusal) + 256)
	for _, tc := range e.Resp.ToolCalls {
		size += int64(len(tc.Arguments) + len(tc.Name) + len(tc.ID))
	}
	s.c.Put(key, e, size)
}

// Do de-duplicates concurrent identical requests: the first caller runs fn,
// followers wait and receive its result. shared reports a follower.
func (s *Store) Do(key string, fn func() (Entry, bool)) (ent Entry, ok, shared bool) {
	s.mu.Lock()
	if c, inflight := s.flight[key]; inflight {
		s.mu.Unlock()
		c.wg.Wait()
		return c.ent, c.ok, true
	}
	c := &call{}
	c.wg.Add(1)
	s.flight[key] = c
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.flight, key)
		s.mu.Unlock()
		c.wg.Done()
	}()
	c.ent, c.ok = fn()
	if c.ok {
		s.Put(key, c.ent)
	}
	return c.ent, c.ok, false
}

// Stats returns hit/miss counts and resident bytes.
func (s *Store) Stats() (hits, misses uint64, items int, bytes int64) {
	return s.hits.Load(), s.misses.Load(), s.c.Len(), s.c.Cost()
}

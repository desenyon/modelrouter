// Package cache is an exact-match response cache with byte-bounded LRU
// eviction, TTL, and singleflight de-duplication of identical in-flight
// requests. Keys cover every generation-affecting parameter, and only
// deterministic requests (temperature 0 or a fixed seed) or explicit opt-ins
// are cached.
package cache

import (
	"context"
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
	done chan struct{}
	ent  Entry
	ok   bool
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
	// Hash the canonical request rather than maintaining a second field list.
	// Only presentation and cache policy are excluded; routing/session/user
	// constraints remain part of the identity.
	rq := *r
	rq.Stream, rq.StreamUsage = false, false
	rq.Router.Explain, rq.Router.NoCache, rq.Router.ForceCache = false, false, false
	b, err := json.Marshal(struct {
		Route   string        `json:"route"`
		Request canon.Request `json:"request"`
	}{routeKey, rq})
	if err != nil {
		return "" // invalid requests must never share a cache key
	}
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
	return clone(e), ok
}

// Put stores an entry; cost is approximated by payload size.
func (s *Store) Put(key string, e Entry) {
	size := int64(len(e.Resp.Content) + len(e.Resp.Refusal) + 256)
	for _, tc := range e.Resp.ToolCalls {
		size += int64(len(tc.Arguments) + len(tc.Name) + len(tc.ID))
	}
	s.c.Put(key, clone(e), size)
}

// Do de-duplicates concurrent identical requests: the first caller runs fn,
// followers wait and receive its result. shared reports a follower.
func (s *Store) Do(key string, fn func() (Entry, bool)) (ent Entry, ok, shared bool) {
	ent, ok, shared, _ = s.DoContext(context.Background(), key, fn)
	return
}

// DoContext lets followers abandon their wait without canceling the leader.
// The leader owns fn's lifetime; if it fails, callers may retry independently.
func (s *Store) DoContext(ctx context.Context, key string, fn func() (Entry, bool)) (ent Entry, ok, shared bool, err error) {
	s.mu.Lock()
	if c, inflight := s.flight[key]; inflight {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Entry{}, false, true, ctx.Err()
		case <-c.done:
			if err := ctx.Err(); err != nil {
				return Entry{}, false, true, err
			}
			return clone(c.ent), c.ok, true, nil
		}
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Entry{}, false, false, err
	}
	// Close the miss→join race: a previous leader may have populated the cache
	// after the caller's initial lookup and before this lock was acquired.
	if e, found := s.c.Get(key); found {
		s.mu.Unlock()
		return clone(e), true, true, nil
	}
	c := &call{done: make(chan struct{})}
	s.flight[key] = c
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.flight, key)
		close(c.done)
		s.mu.Unlock()
	}()
	c.ent, c.ok = fn()
	c.ent = clone(c.ent)
	if c.ok {
		s.Put(key, c.ent)
	}
	return clone(c.ent), c.ok, false, nil
}

// Stats returns hit/miss counts and resident bytes.
func (s *Store) Stats() (hits, misses uint64, items int, bytes int64) {
	return s.hits.Load(), s.misses.Load(), s.c.Len(), s.c.Cost()
}

// clone keeps callers from mutating cached or shared tool calls.
func clone(e Entry) Entry {
	e.Resp.ToolCalls = append([]canon.ToolCall(nil), e.Resp.ToolCalls...)
	return e
}

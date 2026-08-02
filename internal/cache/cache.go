// Package cache provides an LRU fingerprint cache for identical non-stream chats.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"
)

// Entry is a cached upstream HTTP response body + status + content-type.
type Entry struct {
	Status      int
	ContentType string
	Body        []byte
	Tier        string
	Model       string
	Mode        string
	Score       float64
}

// Store is a bounded LRU of fingerprint → Entry.
type Store struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List
	items    map[string]*list.Element
	hits     atomic.Uint64
	misses   atomic.Uint64
}

type lruItem struct {
	key   string
	entry Entry
}

// New creates an LRU with the given capacity (min 64).
func New(capacity int) *Store {
	if capacity < 64 {
		capacity = 64
	}
	return &Store{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[string]*list.Element, capacity),
	}
}

// Fingerprint hashes the routing-relevant request fields.
func Fingerprint(virtualModel, mode string, messages any, tools json.RawMessage, structured bool) string {
	type key struct {
		Model      string          `json:"m"`
		Mode       string          `json:"o"`
		Messages   any             `json:"msg"`
		Tools      json.RawMessage `json:"tools,omitempty"`
		Structured bool            `json:"so,omitempty"`
	}
	b, _ := json.Marshal(key{
		Model: virtualModel, Mode: mode, Messages: messages,
		Tools: tools, Structured: structured,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// Get returns a cached entry if present.
func (s *Store) Get(fp string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[fp]; ok {
		s.ll.MoveToFront(el)
		s.hits.Add(1)
		return el.Value.(*lruItem).entry, true
	}
	s.misses.Add(1)
	return Entry{}, false
}

// Set inserts or refreshes an entry.
func (s *Store) Set(fp string, e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[fp]; ok {
		s.ll.MoveToFront(el)
		el.Value.(*lruItem).entry = e
		return
	}
	el := s.ll.PushFront(&lruItem{key: fp, entry: e})
	s.items[fp] = el
	for s.ll.Len() > s.capacity {
		back := s.ll.Back()
		if back == nil {
			break
		}
		s.ll.Remove(back)
		delete(s.items, back.Value.(*lruItem).key)
	}
}

// Stats returns hit/miss counters.
func (s *Store) Stats() (hits, misses uint64, size int) {
	s.mu.Lock()
	size = s.ll.Len()
	s.mu.Unlock()
	return s.hits.Load(), s.misses.Load(), size
}

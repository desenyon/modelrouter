// Package lru is a small generic LRU with optional TTL and size accounting.
package lru

import (
	"container/list"
	"sync"
	"time"
)

// Cache is safe for concurrent use.
type Cache[K comparable, V any] struct {
	mu       sync.Mutex
	ll       *list.List
	m        map[K]*list.Element
	maxItems int
	maxCost  int64
	cost     int64
	ttl      time.Duration
	now      func() time.Time
}

type entry[K comparable, V any] struct {
	k    K
	v    V
	cost int64
	exp  time.Time
}

// New builds a cache bounded by item count (maxItems>0) and/or total cost
// (maxCost>0). ttl=0 disables expiry.
func New[K comparable, V any](maxItems int, maxCost int64, ttl time.Duration) *Cache[K, V] {
	return &Cache[K, V]{ll: list.New(), m: make(map[K]*list.Element), maxItems: maxItems, maxCost: maxCost, ttl: ttl, now: time.Now}
}

// Get returns a live value and refreshes recency.
func (c *Cache[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.m[k]
	if !ok {
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if !e.exp.IsZero() && c.now().After(e.exp) {
		c.remove(el)
		return zero, false
	}
	c.ll.MoveToFront(el)
	return e.v, true
}

// Put inserts or replaces a value with the given cost.
func (c *Cache[K, V]) Put(k K, v V, cost int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var exp time.Time
	if c.ttl > 0 {
		exp = c.now().Add(c.ttl)
	}
	if el, ok := c.m[k]; ok {
		e := el.Value.(*entry[K, V])
		c.cost += cost - e.cost
		e.v, e.cost, e.exp = v, cost, exp
		c.ll.MoveToFront(el)
	} else {
		c.m[k] = c.ll.PushFront(&entry[K, V]{k: k, v: v, cost: cost, exp: exp})
		c.cost += cost
	}
	for (c.maxItems > 0 && c.ll.Len() > c.maxItems) || (c.maxCost > 0 && c.cost > c.maxCost && c.ll.Len() > 1) {
		c.remove(c.ll.Back())
	}
}

// Delete removes a key.
func (c *Cache[K, V]) Delete(k K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[k]; ok {
		c.remove(el)
	}
}

// Len returns the item count; Cost returns total cost.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Cost returns the summed cost of live entries.
func (c *Cache[K, V]) Cost() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cost
}

func (c *Cache[K, V]) remove(el *list.Element) {
	e := el.Value.(*entry[K, V])
	c.ll.Remove(el)
	delete(c.m, e.k)
	c.cost -= e.cost
}

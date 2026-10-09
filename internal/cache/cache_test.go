package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
)

func TestKeySeparatesGenerationAndRoutingConstraints(t *testing.T) {
	r := canon.Request{Model: "auto", MaxTokens: 100}
	base := Key(&r, "auto|balance")
	for name, change := range map[string]func(*canon.Request){
		"inclusive token limit": func(r *canon.Request) { r.MaxTokensInclusive = true },
		"latency budget":        func(r *canon.Request) { r.Router.MaxLatencyMs = 1000 },
		"session":               func(r *canon.Request) { r.Router.SessionID = "other" },
		"user":                  func(r *canon.Request) { r.User = "other" },
		"quality":               func(r *canon.Request) { r.Router.MinQuality = .9 },
		"cost":                  func(r *canon.Request) { r.Router.MaxCostUSD = .01 },
		"deny":                  func(r *canon.Request) { r.Router.Deny = []string{"openai"} },
	} {
		t.Run(name, func(t *testing.T) {
			other := r
			change(&other)
			if Key(&other, "auto|balance") == base {
				t.Fatal("different request reused cache key")
			}
		})
	}
	r.Stream, r.StreamUsage, r.Router.Explain = true, true, true
	if Key(&r, "auto|balance") != base {
		t.Fatal("presentation options should not split cached generation")
	}
}

func TestEntriesDoNotShareMutableToolCalls(t *testing.T) {
	s := New(4096, time.Minute)
	e := Entry{Resp: canon.Response{ToolCalls: []canon.ToolCall{{Name: "original"}}}}
	s.Put("key", e)
	e.Resp.ToolCalls[0].Name = "input mutated"
	got, _ := s.Get("key")
	if got.Resp.ToolCalls[0].Name != "original" {
		t.Fatal("store retained caller's slice")
	}
	got.Resp.ToolCalls[0].Name = "output mutated"
	again, _ := s.Get("key")
	if again.Resp.ToolCalls[0].Name != "original" {
		t.Fatal("cache returned shared slice")
	}
}

func TestFollowerCanCancelWithoutCancelingLeader(t *testing.T) {
	s := New(4096, time.Minute)
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		s.DoContext(context.Background(), "key", func() (Entry, bool) {
			close(started)
			<-release
			return Entry{Resp: canon.Response{Content: "answer"}}, true
		})
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok, shared, err := s.DoContext(ctx, "key", func() (Entry, bool) { t.Error("follower executed"); return Entry{}, false })
	if !errors.Is(err, context.Canceled) || ok || !shared {
		t.Errorf("canceled wait: ok=%v shared=%v err=%v", ok, shared, err)
	}
	close(release)
	<-finished
	if e, ok := s.Get("key"); !ok || e.Resp.Content != "answer" {
		t.Fatal("follower canceled leader")
	}
}

func TestSingleflightSharesOneCompletion(t *testing.T) {
	s := New(4096, time.Minute)
	started, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var wg sync.WaitGroup
	wg.Go(func() {
		s.DoContext(context.Background(), "key", func() (Entry, bool) {
			calls.Add(1)
			close(started)
			<-release
			return Entry{Resp: canon.Response{Content: "answer"}}, true
		})
	})
	<-started
	// The context reports when the follower has reached its cancellation select.
	ctx := &waitingContext{Context: context.Background(), joined: joined}
	wg.Go(func() {
		e, ok, shared, err := s.DoContext(ctx, "key", func() (Entry, bool) { calls.Add(1); return Entry{}, false })
		if err != nil || !ok || !shared || e.Resp.Content != "answer" {
			t.Errorf("shared result: %+v %v %v %v", e, ok, shared, err)
		}
	})
	<-joined
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("identical calls were not coalesced")
	}
}

type waitingContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *waitingContext) Done() <-chan struct{} { c.once.Do(func() { close(c.joined) }); return nil }

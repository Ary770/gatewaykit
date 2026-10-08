package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimitWindowBoundaries(t *testing.T) {
	for _, strategy := range []string{"fixed_window", "sliding_window"} {
		t.Run(strategy, func(t *testing.T) {
			l := newRateLimiter(&RateLimitConfig{Requests: 2, Window: "10s", Strategy: strategy, Per: "ip"})
			start := time.Unix(100, 0)
			for _, offset := range []time.Duration{0, 5 * time.Second} {
				if ok, _, _ := l.allow("a", start.Add(offset)); !ok {
					t.Fatal("request should fit")
				}
			}
			if ok, retry, _ := l.allow("a", start.Add(9*time.Second)); ok || retry != time.Second {
				t.Fatalf("expected rejection and 1s retry: %v %v", ok, retry)
			}
			if ok, _, _ := l.allow("b", start.Add(9*time.Second)); !ok {
				t.Fatal("IP buckets must be independent")
			}
			if ok, _, _ := l.allow("a", start.Add(10*time.Second)); !ok {
				t.Fatal("exact boundary should expire oldest request")
			}
			ok, _, _ := l.allow("a", start.Add(10*time.Second))
			if ok != (strategy == "fixed_window") {
				t.Fatalf("window behavior differs: %s allowed=%v", strategy, ok)
			}
			if ok, _, _ := l.allow("a", start.Add(20*time.Second)); !ok {
				t.Fatal("idle bucket should reset")
			}
		})
	}
}

func TestRateLimitGlobalAndConcurrent(t *testing.T) {
	for _, strategy := range []string{"fixed_window", "sliding_window"} {
		t.Run(strategy, func(t *testing.T) {
			l := newRateLimiter(&RateLimitConfig{Requests: 10, Window: "1m", Strategy: strategy, Per: "global"})
			now := time.Now()
			var accepted atomic.Int64
			var wg sync.WaitGroup
			for i := 0; i < 50; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					if ok, _, _ := l.allow(fmt.Sprint(i), now); ok {
						accepted.Add(1)
					}
				}(i)
			}
			wg.Wait()
			if accepted.Load() != 10 {
				t.Fatalf("accepted %d; want 10", accepted.Load())
			}
			if len(l.buckets) != 1 {
				t.Fatalf("global identities=%d", len(l.buckets))
			}
		})
	}
}

func TestRateLimitCapacityAndCleanup(t *testing.T) {
	for _, strategy := range []string{"fixed_window", "sliding_window"} {
		t.Run(strategy, func(t *testing.T) {
			l := newRateLimiter(&RateLimitConfig{Requests: 10, Window: "1s", Strategy: strategy, Per: "ip"})
			l.capacity = 2
			now := time.Unix(100, 0)
			l.allow("a", now)
			l.allow("b", now)
			if ok, retry, capacity := l.allow("c", now); ok || capacity || retry <= 0 {
				t.Fatalf("expected bounded capacity: %v %v %v", ok, retry, capacity)
			}
			if ok, _, _ := l.allow("a", now); !ok {
				t.Fatal("existing identity should still work")
			}
			if ok, _, _ := l.allow("c", now.Add(time.Second)); !ok {
				t.Fatal("expired entries should be swept")
			}
			if len(l.buckets) != 1 {
				t.Fatalf("stale buckets remain: %d", len(l.buckets))
			}
		})
	}
}

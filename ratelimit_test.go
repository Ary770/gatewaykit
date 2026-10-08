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
				if decision := l.allow("a", start.Add(offset)); !decision.allowed {
					t.Fatal("request should fit")
				}
			}
			if decision := l.allow("a", start.Add(9*time.Second)); decision.allowed || decision.capacityExceeded || decision.retryAfter != time.Second {
				t.Fatalf("expected quota rejection and 1s retry: %+v", decision)
			}
			if decision := l.allow("b", start.Add(9*time.Second)); !decision.allowed {
				t.Fatal("IP buckets must be independent")
			}
			if decision := l.allow("a", start.Add(10*time.Second)); !decision.allowed {
				t.Fatal("exact boundary should expire oldest request")
			}
			decision := l.allow("a", start.Add(10*time.Second))
			if decision.allowed != (strategy == "fixed_window") {
				t.Fatalf("window behavior differs: %s allowed=%v", strategy, decision.allowed)
			}
			if decision := l.allow("a", start.Add(20*time.Second)); !decision.allowed {
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
					if decision := l.allow(fmt.Sprint(i), now); decision.allowed {
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
			if decision := l.allow("c", now); decision.allowed || !decision.capacityExceeded || decision.retryAfter != time.Second {
				t.Fatalf("expected capacity rejection and 1s retry: %+v", decision)
			}
			if decision := l.allow("a", now); !decision.allowed {
				t.Fatal("existing identity should still work")
			}
			if decision := l.allow("c", now.Add(time.Second)); !decision.allowed {
				t.Fatal("expired entries should be swept")
			}
			if len(l.buckets) != 1 {
				t.Fatalf("stale buckets remain: %d", len(l.buckets))
			}
		})
	}
}

func TestSlidingWindowReorderedConcurrentTimestamps(t *testing.T) {
	l := newRateLimiter(&RateLimitConfig{Requests: 2, Window: "10s", Strategy: "sliding_window", Per: "global"})
	start := time.Unix(100, 0)
	// The earlier caller can acquire the mutex after the later caller.
	if decision := l.allow("a", start.Add(9*time.Second)); !decision.allowed {
		t.Fatal("first request rejected")
	}
	if decision := l.allow("b", start.Add(time.Second)); !decision.allowed {
		t.Fatal("delayed request rejected")
	}
	if decision := l.allow("c", start.Add(11*time.Second)); decision.allowed {
		t.Fatal("reordered timestamps prematurely expired live requests")
	}
	if decision := l.allow("d", start.Add(19*time.Second)); !decision.allowed {
		t.Fatal("requests did not expire at their effective evaluation time")
	}
}

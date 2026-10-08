package main

import (
	"sync"
	"time"
)

// The cap bounds identity growth from unauthenticated traffic. Existing clients
// keep their buckets; new identities fail closed until expired buckets are swept.
const maxRateLimitBuckets = 10000

type rateBucket struct {
	start      time.Time
	count      int
	timestamps []time.Time
}

type rateLimiter struct {
	mu        sync.Mutex
	config    RateLimitConfig
	window    time.Duration
	buckets   map[string]*rateBucket
	nextSweep time.Time
	lastNow   time.Time
	capacity  int
}

func newRateLimiter(config *RateLimitConfig) *rateLimiter {
	if config == nil {
		return nil
	}
	window, _ := duration(config.Window)
	return &rateLimiter{config: *config, window: window, buckets: make(map[string]*rateBucket), capacity: maxRateLimitBuckets}
}

// allow atomically checks and charges one accepted request. A false capacity
// result means the identity table is full, rather than this client's quota.
func (l *rateLimiter) allow(key string, now time.Time) (allowed bool, retry time.Duration, capacity bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A caller can capture time and then lose the lock to a later request. Keep
	// evaluation time monotonic so sliding timestamps stay ordered under contention.
	if now.Before(l.lastNow) {
		now = l.lastNow
	} else {
		l.lastNow = now
	}
	if l.config.Per == "global" {
		key = "global"
	}
	if !now.Before(l.nextSweep) {
		for key, bucket := range l.buckets {
			if l.expired(bucket, now) {
				delete(l.buckets, key)
			}
		}
		interval := min(l.window, time.Second)
		l.nextSweep = now.Add(interval)
	}
	bucket := l.buckets[key]
	if bucket == nil {
		if len(l.buckets) >= l.capacity {
			return false, l.nextSweep.Sub(now), false
		}
		bucket = &rateBucket{start: now}
		l.buckets[key] = bucket
	}
	if l.config.Strategy == "fixed_window" {
		if !now.Before(bucket.start.Add(l.window)) {
			bucket.start = now
			bucket.count = 0
		}
		if bucket.count >= l.config.Requests {
			return false, bucket.start.Add(l.window).Sub(now), true
		}
		bucket.count++
		return true, 0, true
	}
	cutoff := now.Add(-l.window)
	expired := 0
	for expired < len(bucket.timestamps) && !bucket.timestamps[expired].After(cutoff) {
		expired++
	}
	bucket.timestamps = bucket.timestamps[expired:]
	if len(bucket.timestamps) >= l.config.Requests {
		return false, bucket.timestamps[0].Add(l.window).Sub(now), true
	}
	bucket.timestamps = append(bucket.timestamps, now)
	return true, 0, true
}

func (l *rateLimiter) expired(bucket *rateBucket, now time.Time) bool {
	if l.config.Strategy == "fixed_window" {
		return !now.Before(bucket.start.Add(l.window))
	}
	return len(bucket.timestamps) == 0 || !bucket.timestamps[len(bucket.timestamps)-1].After(now.Add(-l.window))
}

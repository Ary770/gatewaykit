package main

import (
	"sync"
	"time"
)

// The cap bounds identity growth from unauthenticated traffic. Existing clients
// keep their buckets; new identities fail closed until expired buckets are swept.
const maxRateLimitBuckets = 10000

type rateLimitDecision struct {
	allowed          bool
	retryAfter       time.Duration
	capacityExceeded bool
}

type rateBucket struct {
	windowStart      time.Time
	acceptedRequests int
	acceptedAt       []time.Time
}

type rateLimiter struct {
	mu             sync.Mutex
	config         RateLimitConfig
	window         time.Duration
	buckets        map[string]*rateBucket
	nextCleanup    time.Time
	lastEvaluation time.Time
	capacity       int
}

func newRateLimiter(config *RateLimitConfig) *rateLimiter {
	if config == nil {
		return nil
	}
	window, _ := duration(config.Window)
	return &rateLimiter{config: *config, window: window, buckets: make(map[string]*rateBucket), capacity: maxRateLimitBuckets}
}

// allow atomically checks and charges one accepted request. All stateful helpers
// run under this lock so cleanup, capacity checks, and quota updates cannot race.
func (limiter *rateLimiter) allow(clientKey string, now time.Time) rateLimitDecision {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	now = limiter.monotonicEvaluationTime(now)
	limiter.removeExpiredBuckets(now)
	bucket := limiter.findOrCreateBucket(limiter.bucketKey(clientKey), now)
	if bucket == nil {
		return rateLimitDecision{retryAfter: limiter.nextCleanup.Sub(now), capacityExceeded: true}
	}
	if limiter.config.Strategy == "fixed_window" {
		return limiter.allowFixedWindow(bucket, now)
	}
	return limiter.allowSlidingWindow(bucket, now)
}

func (limiter *rateLimiter) monotonicEvaluationTime(now time.Time) time.Time {
	// A caller can capture time, then acquire the lock after a later caller.
	// Clamping keeps sliding-window timestamps ordered under contention.
	if now.Before(limiter.lastEvaluation) {
		return limiter.lastEvaluation
	}
	limiter.lastEvaluation = now
	return now
}

func (limiter *rateLimiter) bucketKey(clientKey string) string {
	if limiter.config.Per == "global" {
		return "global"
	}
	return clientKey
}

func (limiter *rateLimiter) removeExpiredBuckets(now time.Time) {
	if now.Before(limiter.nextCleanup) {
		return
	}
	for key, bucket := range limiter.buckets {
		if limiter.bucketExpired(bucket, now) {
			delete(limiter.buckets, key)
		}
	}
	cleanupInterval := min(limiter.window, time.Second)
	limiter.nextCleanup = now.Add(cleanupInterval)
}

func (limiter *rateLimiter) findOrCreateBucket(key string, now time.Time) *rateBucket {
	if bucket := limiter.buckets[key]; bucket != nil {
		return bucket
	}
	if len(limiter.buckets) >= limiter.capacity {
		return nil
	}
	bucket := &rateBucket{windowStart: now}
	limiter.buckets[key] = bucket
	return bucket
}

func (limiter *rateLimiter) allowFixedWindow(bucket *rateBucket, now time.Time) rateLimitDecision {
	if !now.Before(bucket.windowStart.Add(limiter.window)) {
		bucket.windowStart = now
		bucket.acceptedRequests = 0
	}
	if bucket.acceptedRequests >= limiter.config.Requests {
		return rateLimitDecision{retryAfter: bucket.windowStart.Add(limiter.window).Sub(now)}
	}
	bucket.acceptedRequests++
	return rateLimitDecision{allowed: true}
}

func (limiter *rateLimiter) allowSlidingWindow(bucket *rateBucket, now time.Time) rateLimitDecision {
	bucket.discardRequestsThrough(now.Add(-limiter.window))
	if len(bucket.acceptedAt) >= limiter.config.Requests {
		return rateLimitDecision{retryAfter: bucket.acceptedAt[0].Add(limiter.window).Sub(now)}
	}
	bucket.acceptedAt = append(bucket.acceptedAt, now)
	return rateLimitDecision{allowed: true}
}

func (bucket *rateBucket) discardRequestsThrough(cutoff time.Time) {
	expiredCount := 0
	for expiredCount < len(bucket.acceptedAt) && !bucket.acceptedAt[expiredCount].After(cutoff) {
		expiredCount++
	}
	bucket.acceptedAt = bucket.acceptedAt[expiredCount:]
}

func (limiter *rateLimiter) bucketExpired(bucket *rateBucket, now time.Time) bool {
	if limiter.config.Strategy == "fixed_window" {
		return !now.Before(bucket.windowStart.Add(limiter.window))
	}
	return len(bucket.acceptedAt) == 0 || !bucket.acceptedAt[len(bucket.acceptedAt)-1].After(now.Add(-limiter.window))
}

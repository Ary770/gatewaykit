// Counts allowed requests for each client IP, or for all clients of a route.
// Start with allow. A lock keeps simultaneous requests from updating the count
// at the same time. Counts are kept in memory and reset when the app restarts.

package main

import (
	"sync"
	"time"
)

// Keep at most this many client counts in memory. Existing clients keep their
// counts; new clients are rejected until old counts are removed.
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

// newRateLimiter creates the request counters, or returns nil if no limit is configured.
func newRateLimiter(config *RateLimitConfig) *rateLimiter {
	if config == nil {
		return nil
	}
	window, _ := duration(config.Window)
	return &rateLimiter{config: *config, window: window, buckets: make(map[string]*rateBucket), capacity: maxRateLimitBuckets}
}

// allow checks if the request is allowed. If yes, it adds one to the request count.
// The lock keeps the check and update together so two requests cannot take the last slot.
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
	// A request can record its time but reach the lock after a later request.
	// Keep time from moving backward so saved request times stay in order.
	if now.Before(limiter.lastEvaluation) {
		return limiter.lastEvaluation
	}
	limiter.lastEvaluation = now
	return now
}

// bucketKey uses one shared count for a global limit, or a separate count for each client.
func (limiter *rateLimiter) bucketKey(clientKey string) string {
	if limiter.config.Per == "global" {
		return "global"
	}
	return clientKey
}

// removeExpiredBuckets removes old client counts periodically while processing requests.
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

// findOrCreateBucket finds a client count or creates one. It refuses new clients
// when the table reaches its size limit; existing clients can still use their counts.
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

// allowFixedWindow starts a counting period with the first allowed request.
// Once full, it rejects requests until that period ends, without increasing the count.
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

// allowSlidingWindow counts allowed requests within the most recent time period.
// At the limit, it returns the wait until the oldest counted request expires.
func (limiter *rateLimiter) allowSlidingWindow(bucket *rateBucket, now time.Time) rateLimitDecision {
	bucket.discardRequestsThrough(now.Add(-limiter.window))
	if len(bucket.acceptedAt) >= limiter.config.Requests {
		return rateLimitDecision{retryAfter: bucket.acceptedAt[0].Add(limiter.window).Sub(now)}
	}
	bucket.acceptedAt = append(bucket.acceptedAt, now)
	return rateLimitDecision{allowed: true}
}

// discardRequestsThrough removes request times at or before the cutoff; they no longer count.
func (bucket *rateBucket) discardRequestsThrough(cutoff time.Time) {
	expiredCount := 0
	for expiredCount < len(bucket.acceptedAt) && !bucket.acceptedAt[expiredCount].After(cutoff) {
		expiredCount++
	}
	bucket.acceptedAt = bucket.acceptedAt[expiredCount:]
}

// bucketExpired checks whether a client count is too old to affect the limit.
func (limiter *rateLimiter) bucketExpired(bucket *rateBucket, now time.Time) bool {
	if limiter.config.Strategy == "fixed_window" {
		return !now.Before(bucket.windowStart.Add(limiter.window))
	}
	return len(bucket.acceptedAt) == 0 || !bucket.acceptedAt[len(bucket.acceptedAt)-1].After(now.Add(-limiter.window))
}

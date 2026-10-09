// Temporarily stops forwarding after too many backend failures. After waiting,
// one request is allowed to test recovery. Start with admit and finish.
// Background health checks do not change this failure count.

package main

import (
	"log/slog"
	"sync"
	"time"
)

type breakerOutcome uint8

const (
	breakerNeutral breakerOutcome = iota
	breakerSuccess
	breakerFailure
)

type circuitBreaker struct {
	route            string
	mu               sync.Mutex
	threshold        int
	window, cooldown time.Duration
	failures         []time.Time
	open             bool
	opened           time.Time
	probe            bool
	generation       uint64
	last             time.Time
}

type breakerPermit struct {
	breaker    *circuitBreaker
	generation uint64
	probe      bool
	once       sync.Once
}

// newCircuitBreaker sets the failure limit and wait times, or disables the breaker if absent.
func newCircuitBreaker(c *CircuitBreakerConfig) *circuitBreaker {
	if c == nil {
		return nil
	}
	window, _ := duration(c.Window)
	cooldown, _ := duration(c.Cooldown)
	return &circuitBreaker{threshold: c.Threshold, window: window, cooldown: cooldown}
}

// clock keeps recorded time from moving backward when requests finish out of order.
func (b *circuitBreaker) clock(now time.Time) time.Time {
	if now.Before(b.last) {
		return b.last
	}
	b.last = now
	return now
}

// admit checks whether sending is allowed. During the wait it returns no permission
// and the remaining wait time. Afterward, only one request may test recovery.
func (b *circuitBreaker) admit(now time.Time) (*breakerPermit, time.Duration) {
	if b == nil {
		return &breakerPermit{}, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now = b.clock(now)
	if b.open {
		remaining := b.opened.Add(b.cooldown).Sub(now)
		if remaining > 0 {
			return nil, remaining
		}
		if b.probe {
			return nil, time.Second
		}
		b.probe = true
	}
	return &breakerPermit{breaker: b, generation: b.generation, probe: b.open}, 0
}

// finish records the client request result once. A canceled request releases the
// recovery slot without counting as a backend failure. Old results are ignored
// if the breaker has changed state since their requests started.
func (p *breakerPermit) finish(outcome breakerOutcome, now time.Time) {
	p.once.Do(func() {
		b := p.breaker
		if b == nil {
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if p.generation != b.generation {
			return
		}
		now = b.clock(now)
		if p.probe {
			b.probe = false
			if outcome == breakerNeutral {
				return
			}
			b.generation++
			if outcome == breakerSuccess {
				b.open = false
				b.failures = nil
				slog.Info("circuit breaker closed", "route", b.route)
			} else {
				b.opened = now
				slog.Warn("circuit breaker reopened", "route", b.route)
			}
			return
		}
		if outcome != breakerFailure || b.open {
			return
		}
		cutoff := now.Add(-b.window)
		first := 0
		for first < len(b.failures) && !b.failures[first].After(cutoff) {
			first++
		}
		remaining := copy(b.failures, b.failures[first:])
		b.failures = append(b.failures[:remaining], now)
		if len(b.failures) >= b.threshold {
			b.open = true
			b.opened = now
			b.generation++
			b.failures = nil
			slog.Warn("circuit breaker opened", "route", b.route)
		}
	})
}

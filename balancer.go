// Chooses a backend for each request. Higher weights receive more requests.
// Unhealthy backends are skipped. Start with next. The lock protects the choice
// and scores; it is released before sending an HTTP request.

package main

import (
	"net/url"
	"sync"
)

type backend struct {
	url       *url.URL
	weight    int64
	current   int64
	unhealthy bool
}

type balancer struct {
	mu       sync.Mutex
	backends []backend
}

// newBalancer reads the backend addresses and starts their scores at zero.
// For ordinary round robin, all backends have the same weight.
func newBalancer(config UpstreamConfig) *balancer {
	b := &balancer{}
	if config.URL != "" {
		u, _ := url.Parse(config.URL)
		b.backends = append(b.backends, backend{url: u, weight: 1})
	}
	for _, target := range config.Targets {
		u, _ := url.Parse(target.URL)
		weight := int64(1)
		if config.Balance == "weighted_round_robin" {
			weight = int64(target.Weight)
		}
		b.backends = append(b.backends, backend{url: u, weight: weight})
	}
	return b
}

// next picks an available backend, or returns nil if all are unhealthy.
// It updates scores under a lock so simultaneous requests keep the configured ratio.
func (b *balancer) next() *url.URL {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Use scores to spread requests by weight without storing repeated backend entries.
	// Equal weights make the backends take turns.
	best := -1
	total := int64(0)
	for i := range b.backends {
		target := &b.backends[i]
		if target.unhealthy {
			continue
		}
		total += target.weight
		target.current += target.weight
		if best == -1 || target.current > b.backends[best].current {
			best = i
		}
	}
	if best == -1 {
		return nil
	}
	b.backends[best].current -= total
	return b.backends[best].url
}

// setHealthy changes whether a backend can receive requests. It resets scores
// so a backend that returns to service does not get a sudden burst of traffic.
func (b *balancer) setHealthy(index int, healthy bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.backends[index].unhealthy == !healthy {
		return false
	}
	b.backends[index].unhealthy = !healthy
	for i := range b.backends {
		b.backends[i].current = 0
	}
	return true
}

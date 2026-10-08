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

func (b *balancer) next() *url.URL {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Smooth weighted round robin distributes traffic without allocating a list
	// proportional to the weights. Equal weights give ordinary round robin.
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

// setHealthy resets accumulated credits so a recovering target cannot monopolize traffic.
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

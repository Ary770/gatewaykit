package main

import (
	"net/url"
	"sync"
)

type backend struct {
	url     *url.URL
	weight  int64
	current int64
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
	best := 0
	total := int64(0)
	for i := range b.backends {
		target := &b.backends[i]
		total += target.weight
		target.current += target.weight
		if target.current > b.backends[best].current {
			best = i
		}
	}
	b.backends[best].current -= total
	return b.backends[best].url
}

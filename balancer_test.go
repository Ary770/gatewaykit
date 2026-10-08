package main

import (
	"sync"
	"testing"
)

func TestBalancerDistribution(t *testing.T) {
	for _, tc := range []struct {
		name     string
		config   UpstreamConfig
		expected map[string]int
	}{
		{"single", UpstreamConfig{URL: "http://single"}, map[string]int{"single": 40}},
		{"round robin", UpstreamConfig{Balance: "round_robin", Targets: []TargetConfig{{URL: "http://a", Weight: 3}, {URL: "http://b", Weight: 1}}}, map[string]int{"a": 20, "b": 20}},
		{"weighted", UpstreamConfig{Balance: "weighted_round_robin", Targets: []TargetConfig{{URL: "http://a", Weight: 3}, {URL: "http://b", Weight: 1}}}, map[string]int{"a": 30, "b": 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBalancer(tc.config)
			counts := map[string]int{}
			var mu sync.Mutex
			var wg sync.WaitGroup
			for i := 0; i < 40; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); host := b.next().Host; mu.Lock(); counts[host]++; mu.Unlock() }()
			}
			wg.Wait()
			for host, want := range tc.expected {
				if counts[host] != want {
					t.Fatalf("distribution=%v expected=%v", counts, tc.expected)
				}
			}
		})
	}
}

func TestWeightedBalancerSmoothSequence(t *testing.T) {
	b := newBalancer(UpstreamConfig{Balance: "weighted_round_robin", Targets: []TargetConfig{{URL: "http://a", Weight: 3}, {URL: "http://b", Weight: 1}}})
	for _, want := range []string{"a", "a", "b", "a", "a", "a", "b", "a"} {
		if got := b.next().Host; got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	}
}

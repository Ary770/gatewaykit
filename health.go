package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

func validateHealthCheck(c *HealthCheckConfig) error {
	if c == nil {
		return nil
	}
	u, err := url.Parse(c.Path)
	if err != nil || !strings.HasPrefix(c.Path, "/") || strings.HasPrefix(c.Path, "//") || strings.ContainsAny(c.Path, "\\\r\n") || u.IsAbs() || u.Host != "" || u.Fragment != "" || ambiguousPath(u) {
		return fmt.Errorf("path must be an unambiguous origin-relative path")
	}
	if _, err := duration(c.Interval); err != nil {
		return fmt.Errorf("interval: %w", err)
	}
	if c.UnhealthyThreshold == 0 {
		c.UnhealthyThreshold = 1
	}
	if c.UnhealthyThreshold < 1 {
		return fmt.Errorf("unhealthy_threshold must be positive")
	}
	return nil
}

// startHealthChecks is explicitly owned by run after binding the listener.
// The returned stop function cancels in-flight probes and joins every worker.
func (g *Gateway) startHealthChecks(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	for _, r := range g.routes {
		if r.config.HealthCheck == nil {
			continue
		}
		for index := range r.balancer.backends {
			workers.Add(1)
			go func(selectedRoute *route, backendIndex int) {
				defer workers.Done()
				g.monitorBackend(ctx, selectedRoute, backendIndex)
			}(r, index)
		}
	}
	return func() { cancel(); workers.Wait() }
}

func (g *Gateway) monitorBackend(ctx context.Context, selectedRoute *route, backendIndex int) {
	policy := selectedRoute.config.HealthCheck
	interval, _ := duration(policy.Interval)
	timeout := min(selectedRoute.timeout, interval, 5*time.Second)
	target := selectedRoute.balancer.backends[backendIndex].url
	endpoint := healthProbeURL(target, policy.Path)
	consecutiveFailures := 0
	for ctx.Err() == nil {
		healthy := g.probeBackend(ctx, endpoint, timeout)
		if ctx.Err() != nil {
			return
		}
		if healthy {
			consecutiveFailures = 0
		} else if consecutiveFailures < policy.UnhealthyThreshold {
			consecutiveFailures++
		}
		if healthy || consecutiveFailures >= policy.UnhealthyThreshold {
			updateBackendHealth(selectedRoute, backendIndex, healthy)
		}
		if !waitForHealthInterval(ctx, interval) {
			return
		}
	}
}

func healthProbeURL(target *url.URL, path string) *url.URL {
	endpoint := *target
	probePath, _ := url.Parse(path)
	endpoint.Path, endpoint.RawPath, endpoint.RawQuery = probePath.Path, probePath.RawPath, probePath.RawQuery
	endpoint.ForceQuery = probePath.ForceQuery
	return &endpoint
}

func (g *Gateway) probeBackend(parent context.Context, endpoint *url.URL, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	// RoundTrip does not follow redirects; header receipt defines probe success.
	response, err := g.transport.RoundTrip(request)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	return err == nil && response.StatusCode >= 200 && response.StatusCode < 400
}

func updateBackendHealth(selectedRoute *route, backendIndex int, healthy bool) {
	if !selectedRoute.balancer.setHealthy(backendIndex, healthy) {
		return
	}
	target := selectedRoute.balancer.backends[backendIndex].url
	slog.Info("upstream health changed", "route", selectedRoute.config.Path, "target", target.Host, "healthy", healthy)
}

func waitForHealthInterval(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

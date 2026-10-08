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
			go func(r *route, index int, target *url.URL) {
				defer workers.Done()
				c := r.config.HealthCheck
				interval, _ := duration(c.Interval)
				timeout := min(r.timeout, interval, 5*time.Second)
				endpoint := *target
				path, _ := url.Parse(c.Path)
				endpoint.Path, endpoint.RawPath, endpoint.RawQuery = path.Path, path.RawPath, path.RawQuery
				endpoint.ForceQuery = path.ForceQuery
				failures := 0
				for {
					if ctx.Err() != nil {
						return
					}
					probeCtx, stop := context.WithTimeout(ctx, timeout)
					req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, endpoint.String(), nil)
					// RoundTrip does not follow redirects; header receipt defines probe success.
					response, err := g.transport.RoundTrip(req)
					healthy := err == nil && response.StatusCode >= 200 && response.StatusCode < 400
					if response != nil && response.Body != nil {
						response.Body.Close()
					}
					stop()
					if ctx.Err() != nil {
						return
					}
					if healthy {
						failures = 0
					} else if failures < c.UnhealthyThreshold {
						failures++
					}
					if healthy || failures >= c.UnhealthyThreshold {
						if r.balancer.setHealthy(index, healthy) {
							slog.Info("upstream health changed", "route", r.config.Path, "target", target.Host, "healthy", healthy)
						}
					}
					timer := time.NewTimer(interval)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}(r, index, r.balancer.backends[index].url)
		}
	}
	return func() { cancel(); workers.Wait() }
}

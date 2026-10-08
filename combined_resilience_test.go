package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCombinedResilienceTransformsRetriesHealthAndQuota(t *testing.T) {
	var healthy atomic.Bool
	var calls atomic.Int32
	var mu sync.Mutex
	var bodies, timestamps []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			if !healthy.Load() {
				w.WriteHeader(503)
			}
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		bodies = append(bodies, string(data))
		timestamps = append(timestamps, r.Header.Get("X-Request-Start"))
		mu.Unlock()
		if calls.Add(1) == 1 {
			// The discarded retry response cannot be JSON-transformed successfully.
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(503)
			io.WriteString(w, "temporary outage")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{
		Path: "/items", Methods: []string{"PUT"}, Upstream: UpstreamConfig{URL: upstream.URL},
		HealthCheck:    &HealthCheckConfig{Path: "/healthz", Interval: "20ms", UnhealthyThreshold: 1},
		CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "10s", Cooldown: "1s"},
		Retry:          retryTestConfig(), RateLimit: &RateLimitConfig{Requests: 2, Window: "1h", Strategy: "fixed_window", Per: "global"},
		RequestTransform:  &RequestTransformConfig{Headers: TransformHeaders{Add: map[string]string{"X-Request-Start": "$request_time"}}, Body: &RequestTransformBody{Mapping: map[string]string{"user.id": "id", "timestamp": "$request_time"}}},
		ResponseTransform: &ResponseTransformConfig{Body: &ResponseTransformBody{Envelope: map[string]any{"data": "$body", "route": "$route_path"}}},
	})
	var ticks atomic.Int64
	g.now = func() time.Time { return time.Unix(100, ticks.Add(1)) }
	stop := g.startHealthChecks(context.Background())
	defer stop()
	waitHealth(t, g, false)
	healthy.Store(true)
	waitHealth(t, g, true)
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/items", strings.NewReader(`{"id":7}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		return w
	}
	w := call()
	if w.Code != 200 || w.Body.String() != `{"data":{"ok":true},"route":"/items"}` {
		t.Fatalf("final response=%d %s", w.Code, w.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("attempts=%d want=2", calls.Load())
	}
	mu.Lock()
	first, second := bodies[0], bodies[1]
	firstTime, secondTime := timestamps[0], timestamps[1]
	mu.Unlock()
	if first != second || firstTime == "" || firstTime != secondTime {
		t.Fatalf("replay changed body/timestamp: %q %q %q %q", first, second, firstTime, secondTime)
	}
	var mapped struct {
		User struct {
			ID int `json:"id"`
		} `json:"user"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(first), &mapped); err != nil || mapped.User.ID != 7 || mapped.Timestamp != firstTime {
		t.Fatalf("mapping=%s err=%v", first, err)
	}
	// A failed attempt followed by success must not open the threshold-one breaker.
	b := g.routes[0].breaker
	b.mu.Lock()
	open := b.open
	failures := len(b.failures)
	b.mu.Unlock()
	if open || failures != 0 {
		t.Fatalf("retry counted as logical failure: open=%v failures=%d", open, failures)
	}
	healthy.Store(false)
	waitHealth(t, g, false)
	healthy.Store(true)
	waitHealth(t, g, true)
	if w = call(); w.Code != 200 {
		t.Fatalf("recovered request=%d %s; retries may have charged extra quota", w.Code, w.Body.String())
	}
	if w = call(); w.Code != 429 {
		t.Fatalf("third logical request=%d want=429", w.Code)
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream calls=%d want=3", calls.Load())
	}
}

func TestCombinedResilienceBreakerCountsLogicalRequests(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		io.WriteString(w, "unavailable")
	}))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}, Retry: retryTestConfig(), CircuitBreaker: &CircuitBreakerConfig{Threshold: 2, Window: "10s", Cooldown: "1s"}})
	call := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		return w
	}
	if w := call(); w.Code != 503 || calls.Load() != 3 {
		t.Fatalf("first request status=%d attempts=%d", w.Code, calls.Load())
	}
	b := g.routes[0].breaker
	b.mu.Lock()
	failures := len(b.failures)
	open := b.open
	b.mu.Unlock()
	if failures != 1 || open {
		t.Fatalf("one logical failure: failures=%d open=%v", failures, open)
	}
	if w := call(); w.Code != 503 || calls.Load() != 6 {
		t.Fatalf("second request status=%d attempts=%d", w.Code, calls.Load())
	}
	w := call()
	if w.Code != 503 || calls.Load() != 6 || !strings.Contains(w.Body.String(), "service_unavailable") || w.Header().Get("Retry-After") == "" {
		t.Fatalf("open breaker status=%d attempts=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
}

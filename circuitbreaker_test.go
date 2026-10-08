package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBreakerLifecycle(t *testing.T) {
	now := time.Unix(100, 0)
	b := newCircuitBreaker(&CircuitBreakerConfig{Threshold: 2, Window: "10s", Cooldown: "5s"})
	late, _ := b.admit(now)
	for i := 0; i < 2; i++ {
		p, _ := b.admit(now)
		p.finish(breakerFailure, now)
	}
	if p, d := b.admit(now); p != nil || d != 5*time.Second {
		t.Fatalf("open admission %v %v", p, d)
	}
	now = now.Add(5 * time.Second)
	p, _ := b.admit(now)
	if p == nil {
		t.Fatal("missing probe")
	}
	if other, _ := b.admit(now); other != nil {
		t.Fatal("multiple probes")
	}
	p.finish(breakerNeutral, now)
	p, _ = b.admit(now)
	p.finish(breakerFailure, now)
	if p, _ := b.admit(now); p != nil {
		t.Fatal("failed probe did not reopen")
	}
	now = now.Add(5 * time.Second)
	p, _ = b.admit(now)
	p.finish(breakerSuccess, now)
	late.finish(breakerFailure, now)
	if b.open || len(b.failures) != 0 {
		t.Fatal("stale outcome mutated recovered state")
	}
	p, _ = b.admit(now)
	p.finish(breakerFailure, now)
	p.finish(breakerFailure, now)
	if b.open || len(b.failures) != 1 {
		t.Fatal("permit finished twice")
	}
}

func TestBreakerWindowAndClock(t *testing.T) {
	now := time.Unix(100, 0)
	b := newCircuitBreaker(&CircuitBreakerConfig{Threshold: 2, Window: "10s", Cooldown: "5s"})
	p, _ := b.admit(now)
	p.finish(breakerFailure, now)
	now = now.Add(10 * time.Second)
	p, _ = b.admit(now)
	p.finish(breakerFailure, now)
	if b.open || len(b.failures) != 1 {
		t.Fatal("window boundary retained expired failure")
	}
	p, _ = b.admit(now.Add(-time.Second))
	p.finish(breakerSuccess, now.Add(-time.Second))
	if !b.last.Equal(now) {
		t.Fatal("clock regressed")
	}
	p, _ = b.admit(now)
	p.finish(breakerFailure, now)
	if !b.open || len(b.failures) > b.threshold {
		t.Fatal("threshold or bounded storage violated")
	}
	p, _ = (*circuitBreaker)(nil).admit(now)
	p.finish(breakerFailure, now)
}

func TestBreakerConcurrentProbe(t *testing.T) {
	now := time.Unix(100, 0)
	b := newCircuitBreaker(&CircuitBreakerConfig{Threshold: 1, Window: "10s", Cooldown: "1s"})
	p, _ := b.admit(now)
	p.finish(breakerFailure, now)
	var count atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, _ := b.admit(now.Add(time.Second)); p != nil {
				count.Add(1)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("probes=%d", count.Load())
	}
}

func TestCircuitBreakerValidation(t *testing.T) {
	for _, c := range []*CircuitBreakerConfig{{Threshold: 0, Window: "1s", Cooldown: "1s"}, {Threshold: 1000001, Window: "1s", Cooldown: "1s"}, {Threshold: 1, Window: "bad", Cooldown: "1s"}, {Threshold: 1, Window: "1s", Cooldown: "0s"}} {
		if validateCircuitBreaker(c) == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	if validateCircuitBreaker(nil) != nil {
		t.Fatal("nil config rejected")
	}
	_, _, err := decodeConfig(strings.NewReader("routes:\n  - path: /\n    methods: [GET]\n    upstream: {url: 'http://localhost:3001'}\n    circuit_breaker: {threshold: 0, window: 1s, cooldown: 1s}\n"))
	if err == nil || !strings.Contains(err.Error(), "circuit_breaker") {
		t.Fatalf("error=%v", err)
	}
}

func TestCircuitBreakerHTTPRecovery(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
		} else {
			io.WriteString(w, "ok")
		}
	}))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}, CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "10s", Cooldown: "5s"}})
	now := time.Unix(100, 0)
	g.now = func() time.Time { return now }
	call := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		return w
	}
	if w := call(); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if w := call(); w.Code != 503 || w.Header().Get("Retry-After") != "5" || !strings.Contains(w.Body.String(), "service_unavailable") {
		t.Fatalf("response=%+v", w)
	}
	if calls.Load() != 1 {
		t.Fatal("open circuit contacted upstream")
	}
	now = now.Add(5 * time.Second)
	if w := call(); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := call(); w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestCircuitBreakerCancelledProbeNeutral(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}, CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "10s", Cooldown: "1s"}})
	now := time.Unix(100, 0)
	g.now = func() time.Time { return now }
	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	now = now.Add(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil).WithContext(ctx))
	p, _ := g.routes[0].breaker.admit(now)
	if p == nil {
		t.Fatal("cancelled probe remained reserved")
	}
	p.finish(breakerNeutral, now)
}

func TestCircuitBreakerTruncatedBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "20")
		io.WriteString(w, "short")
	}))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}, CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "10s", Cooldown: "1s"}})
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Error("expected truncated response abort")
			}
		}()
		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	if !g.routes[0].breaker.open {
		t.Fatal("truncated response not counted")
	}
}

func TestCircuitBreakerTransportFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}, CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "10s", Cooldown: "1s"}})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 502 || !g.routes[0].breaker.open {
		t.Fatalf("status=%d open=%v", w.Code, g.routes[0].breaker.open)
	}
}

type failingDownstream struct{ *httptest.ResponseRecorder }

func (w failingDownstream) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }
func TestCircuitBreakerDownstreamFailureNeutral(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "response") }))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}, CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "10s", Cooldown: "1s"}})
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Error("expected response abort")
			}
		}()
		g.ServeHTTP(failingDownstream{httptest.NewRecorder()}, httptest.NewRequest("GET", "/", nil))
	}()
	if g.routes[0].breaker.open || len(g.routes[0].breaker.failures) != 0 {
		t.Fatal("downstream failure affected breaker")
	}
}

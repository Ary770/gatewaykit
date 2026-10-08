package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthValidation(t *testing.T) {
	for _, field := range []string{`{path: //other/health, interval: 1s}`, `{path: /../health, interval: 1s}`, `{path: /health, interval: 0s}`, `{path: /health, interval: 1s, unhealthy_threshold: -1}`, `{path: /health, interval: 1s, unknown: true}`} {
		if _, _, err := decodeConfig(strings.NewReader(minimalConfig + "    health_check: " + field + "\n")); err == nil {
			t.Fatalf("accepted %s", field)
		}
	}
}

func healthGateway(t *testing.T, address string) *Gateway {
	t.Helper()
	c := Config{Gateway: GatewayConfig{GlobalTimeout: "1s"}, Routes: []RouteConfig{{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: address}, HealthCheck: &HealthCheckConfig{Path: "/healthz?check=1", Interval: "5ms", UnhealthyThreshold: 2}}}}
	if _, err := c.validate(); err != nil {
		t.Fatal(err)
	}
	return newGateway(c, http.DefaultTransport)
}

func waitHealth(t *testing.T, g *Gateway, healthy bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if (g.routes[0].balancer.next() != nil) == healthy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("health did not become %v", healthy)
}

func TestHealthExclusionRecoveryAndRedirect(t *testing.T) {
	var status atomic.Int32
	status.Store(500)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/healthz" || r.URL.RawQuery != "check=1" || r.Method != "GET" {
			t.Errorf("bad probe %s", r.URL)
		}
		w.Header().Set("Location", "http://127.0.0.1:1/must-not-follow")
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()
	g := healthGateway(t, server.URL+"/base?discard=1")
	if calls.Load() != 0 || g.routes[0].balancer.next() == nil {
		t.Fatal("constructor started probes or excluded initial target")
	}
	stop := g.startHealthChecks(context.Background())
	defer stop()
	waitHealth(t, g, false)
	response := httptest.NewRecorder()
	g.ServeHTTP(response, httptest.NewRequest("GET", "http://gateway/test", nil))
	if response.Code != 503 {
		t.Fatalf("status %d", response.Code)
	}
	status.Store(302)
	waitHealth(t, g, true)
	stop()
	count := calls.Load()
	time.Sleep(15 * time.Millisecond)
	if calls.Load() != count {
		t.Fatal("probes continued after join")
	}
}

func TestHealthCancellationJoinsInFlight(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { started <- struct{}{}; <-r.Context().Done() }))
	defer server.Close()
	g := healthGateway(t, server.URL)
	g.routes[0].config.HealthCheck.Interval = "1s"
	stop := g.startHealthChecks(context.Background())
	<-started
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("probe did not cancel")
	}
	if g.routes[0].balancer.next() == nil {
		t.Fatal("cancellation marked unhealthy")
	}
}

func TestBalancerHealthDistribution(t *testing.T) {
	b := newBalancer(UpstreamConfig{Balance: "weighted_round_robin", Targets: []TargetConfig{{URL: "http://a", Weight: 3}, {URL: "http://b", Weight: 1}}})
	b.next()
	b.setHealthy(0, false)
	for i := 0; i < 10; i++ {
		if b.next().Host != "b" {
			t.Fatal("unhealthy selected")
		}
	}
	b.setHealthy(0, true)
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		counts[b.next().Host]++
	}
	if counts["a"] != 30 || counts["b"] != 10 {
		t.Fatal(counts)
	}
	b.setHealthy(0, false)
	b.setHealthy(1, false)
	if b.next() != nil {
		t.Fatal("expected no targets")
	}
}

func TestHealthConcurrentBalancing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	g := healthGateway(t, server.URL)
	r := g.routes[0]
	r.balancer = newBalancer(UpstreamConfig{Targets: []TargetConfig{{URL: server.URL}, {URL: server.URL}, {URL: server.URL}}})
	stop := g.startHealthChecks(context.Background())
	defer stop()
	deadline := time.Now().Add(30 * time.Millisecond)
	for time.Now().Before(deadline) {
		r.balancer.next()
	}
	waitHealth(t, g, false)
}

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testGateway(t *testing.T, routes ...RouteConfig) *Gateway {
	t.Helper()
	c := Config{Routes: routes}
	if _, err := c.validate(); err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	t.Cleanup(transport.CloseIdleConnections)
	return newGateway(c, transport)
}

func TestProxyPreservesRequestAndResponse(t *testing.T) {
	type observed struct {
		method, uri, body, host string
		header                  http.Header
	}
	received := make(chan observed, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- observed{r.Method, r.RequestURI, string(body), r.Host, r.Header.Clone()}
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.Header().Set("Connection", "X-Private")
		w.Header().Set("X-Private", "remove")
		w.Header().Set("X-Public", "keep")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	}))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/api/users", Methods: []string{"POST"}, Upstream: UpstreamConfig{URL: upstream.URL + "/base?fixed=1"}})
	req := httptest.NewRequest("POST", "http://gateway/api/users/a%2Fb?q=a%20b&q=c", strings.NewReader(`{"name":"Ada"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "X-Secret")
	req.Header.Set("X-Secret", "remove")
	req.Header.Set("X-Forwarded-For", "spoofed")
	req.RemoteAddr = "192.0.2.25:1234"
	result := httptest.NewRecorder()
	g.ServeHTTP(result, req)
	got := <-received
	if got.method != "POST" || got.uri != "/base/api/users/a%2Fb?fixed=1&q=a%20b&q=c" || got.body != `{"name":"Ada"}` {
		t.Fatalf("upstream request: %+v", got)
	}
	target, _ := url.Parse(upstream.URL)
	if got.host != target.Host || got.header.Get("X-Secret") != "" || got.header.Get("X-Forwarded-For") != "192.0.2.25" || got.header.Get("Content-Type") != "application/json" {
		t.Fatalf("upstream headers: %+v", got)
	}
	if result.Code != 201 || result.Body.String() != `{"created":true}` || result.Header().Get("X-Private") != "" || result.Header().Get("X-Public") != "keep" || len(result.Header().Values("Set-Cookie")) != 2 {
		t.Fatalf("response: %+v %s", result, result.Body.String())
	}
}

func TestRoutingAndStripping(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.RequestURI) }))
	defer upstream.Close()
	routes := []RouteConfig{
		{Path: "/api", Methods: []string{"GET", "POST"}, Upstream: UpstreamConfig{URL: upstream.URL}},
		{Path: "/api/products", Methods: []string{"GET"}, StripPrefix: true, Upstream: UpstreamConfig{URL: upstream.URL}},
		{Path: "/api/products", Methods: []string{"DELETE"}, StripPrefix: true, Upstream: UpstreamConfig{URL: upstream.URL}},
	}
	g := testGateway(t, routes...)
	for _, tc := range []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/api/products/123?q=1", 200, "/123?q=1"},
		{"GET", "/api/products", 200, "/"},
		{"GET", "/api/products/", 200, "/"},
		{"GET", "/api/products/a%2Fb", 200, "/a%2Fb"},
		{"GET", "/api/users", 200, "/api/users"},
		{"GET", "/api/productsXYZ", 200, "/api/productsXYZ"},
		{"GET", "/apiXYZ", 404, ""},
		{"GET", "/missing", 404, ""},
		{"POST", "/api/products", 405, ""},
		{"DELETE", "/api/products", 200, "/"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.status || tc.body != "" && w.Body.String() != tc.body {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
			if tc.status == 405 && w.Header().Get("Allow") != "DELETE, GET" {
				t.Fatalf("Allow=%q", w.Header().Get("Allow"))
			}
		})
	}
}

func TestHealthAlwaysAvailable(t *testing.T) {
	g := testGateway(t, RouteConfig{Path: "/health", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: "http://127.0.0.1:1"}})
	g.now = func() time.Time { return g.started.Add(7 * time.Second) }
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	var body struct {
		Status string `json:"status"`
		Uptime int    `json:"uptime_seconds"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || body.Status != "healthy" || body.Uptime != 7 {
		t.Fatalf("health: %d %+v", w.Code, body)
	}
}

func TestUpstreamFailureAndTimeout(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	for _, tc := range []struct {
		name, url string
		code      int
	}{{"dead", dead.URL, 502}, {"slow", slow.URL, 504}} {
		t.Run(tc.name, func(t *testing.T) {
			g := testGateway(t, RouteConfig{Path: "/test", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: tc.url, Timeout: "30ms"}})
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))
			if w.Code != tc.code {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestClientCancellationReachesUpstream(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(canceled) }))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil).WithContext(ctx))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("upstream not reached")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream not canceled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler not finished")
	}
}

func TestUpstreamRedirectReturnedWithoutFollowing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://example.invalid/next")
		w.WriteHeader(302)
	}))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 302 || w.Header().Get("Location") != "http://example.invalid/next" {
		t.Fatalf("redirect: %+v", w)
	}
}

func TestAuthenticationAndRateLimitPipeline(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/private", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}, Auth: &AuthConfig{Type: "api_key", Header: "X-Key", Keys: []string{"first", "second"}}, RateLimit: &RateLimitConfig{Requests: 1, Window: "10s", Strategy: "fixed_window", Per: "ip"}})
	now := time.Unix(100, 0)
	g.now = func() time.Time { return now }
	for _, tc := range []struct {
		key  string
		code int
	}{{"", 401}, {"wrong", 401}, {"second", 204}, {"first", 429}} {
		req := httptest.NewRequest("GET", "/private", nil)
		if tc.key != "" {
			req.Header.Set("X-Key", tc.key)
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Fatalf("got %d want %d", w.Code, tc.code)
		}
		if tc.code == 429 && w.Header().Get("Retry-After") != "10" {
			t.Fatalf("retry=%s", w.Header().Get("Retry-After"))
		}
		if tc.code == 401 && w.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("missing authentication challenge")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("unauthorized/limited request reached upstream; calls=%d", calls.Load())
	}
	now = now.Add(10 * time.Second)
	req := httptest.NewRequest("GET", "/private", nil)
	req.Header.Set("X-Key", "first")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != 204 || calls.Load() != 2 {
		t.Fatalf("window did not reset: %d calls=%d", w.Code, calls.Load())
	}
}

func TestDefaultRateLimitAndRouteOverride(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer upstream.Close()
	c := Config{Gateway: GatewayConfig{GlobalRateLimit: &RateLimitConfig{Requests: 1, Window: "1m", Strategy: "fixed_window", Per: "global"}}, Routes: []RouteConfig{
		{Path: "/one", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}},
		{Path: "/two", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}},
		{Path: "/override", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}, RateLimit: &RateLimitConfig{Requests: 2, Window: "1m", Strategy: "sliding_window", Per: "global"}},
	}}
	if _, err := c.validate(); err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	g := newGateway(c, transport)
	for _, tc := range []struct {
		path string
		code int
	}{{"/one", 204}, {"/one", 429}, {"/two", 204}, {"/override", 204}, {"/override", 204}, {"/override", 429}, {"/health", 200}} {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.code {
			t.Fatalf("%s got %d want %d", tc.path, w.Code, tc.code)
		}
	}
}

func TestHeaderAuthenticationRejectsMultipleValues(t *testing.T) {
	auth := &AuthConfig{Header: "X-Key", Keys: []string{"abc"}}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add("X-Key", "abc")
	req.Header.Add("X-Key", "abc")
	if authorized(req, auth) {
		t.Fatal("ambiguous multiple key values accepted")
	}
}

func TestWeightedBackendsEndToEnd(t *testing.T) {
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "a:"+r.URL.Path) }))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "b:"+r.URL.Path) }))
	defer b.Close()
	g := testGateway(t, RouteConfig{Path: "/products", Methods: []string{"GET"}, StripPrefix: true, Upstream: UpstreamConfig{Balance: "weighted_round_robin", Targets: []TargetConfig{{URL: a.URL, Weight: 3}, {URL: b.URL, Weight: 1}}}})
	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("GET", "/products/123", nil))
		if w.Code != 200 {
			t.Fatalf("status=%d", w.Code)
		}
		counts[w.Body.String()]++
	}
	if counts["a:/123"] != 6 || counts["b:/123"] != 2 {
		t.Fatalf("responses=%v", counts)
	}
}

func TestRateLimitCapacityHTTPResponse(t *testing.T) {
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: "http://127.0.0.1:1"}, RateLimit: &RateLimitConfig{Requests: 10, Window: "1s", Strategy: "fixed_window", Per: "ip"}})
	g.routes[0].limiter.capacity = 0
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("got %d headers=%v", w.Code, w.Header())
	}
}

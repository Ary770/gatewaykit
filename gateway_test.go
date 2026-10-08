package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

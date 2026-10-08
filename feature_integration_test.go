package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type featureTransport func(*http.Request) (*http.Response, error)

func (f featureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type interruptedUpload struct{}

func (*interruptedUpload) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (*interruptedUpload) Close() error             { return nil }

func TestBreakerIgnoresUploadFailures(t *testing.T) {
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"POST"}, Upstream: UpstreamConfig{URL: "http://backend"}, CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "1m", Cooldown: "1m"}})
	calls := 0
	g.transport = featureTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		_, err := io.ReadAll(req.Body)
		req.Body.Close()
		return nil, err
	})
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/", nil)
		req.Body = &interruptedUpload{}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		if w.Code != 502 {
			t.Fatalf("upload error opened circuit: %d", w.Code)
		}
	}
	if calls != 2 || len(g.routes[0].breaker.failures) != 0 {
		t.Fatal("client upload counted as backend failure")
	}
}

func TestBreakerCountsTruncatedTransformedResponse(t *testing.T) {
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: "http://backend"}, CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "1m", Cooldown: "1m"}, ResponseTransform: &ResponseTransformConfig{Body: &ResponseTransformBody{Envelope: map[string]any{"data": "$body"}}}})
	g.transport = featureTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: &interruptedUpload{}}, nil
	})
	for _, want := range []int{502, 503} {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != want {
			t.Fatalf("got %d want %d", w.Code, want)
		}
	}
}

func TestBreakerIgnoresLocalTransformFailures(t *testing.T) {
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"POST"}, Upstream: UpstreamConfig{URL: "http://backend"}, CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "1m", Cooldown: "1m"}, RequestTransform: &RequestTransformConfig{Body: &RequestTransformBody{Mapping: map[string]string{"name": "name"}}}})
	g.transport = featureTransport(func(req *http.Request) (*http.Response, error) { t.Fatal("invalid body forwarded"); return nil, nil })
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/", strings.NewReader("invalid"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("got %d", w.Code)
		}
	}
	if len(g.routes[0].breaker.failures) != 0 {
		t.Fatal("local failure counted")
	}
}

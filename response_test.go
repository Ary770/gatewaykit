package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type closeTrackingResponseBody struct {
	io.ReadCloser
	closed bool
}

func TestForwardTransportErrorResponse(t *testing.T) {
	for _, tc := range []struct {
		name, wantError, wantConnection string
		err                             error
		wantStatus                      int
	}{
		{name: "connection error", err: io.ErrUnexpectedEOF, wantStatus: 502, wantError: "bad_gateway"},
		{name: "timeout", err: context.DeadlineExceeded, wantStatus: 504, wantError: "gateway_timeout", wantConnection: "close"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: "http://backend"}, CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "1m", Cooldown: "1m"}})
			g.transport = featureTransport(func(*http.Request) (*http.Response, error) { return nil, tc.err })
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.wantStatus || body["error"] != tc.wantError || w.Header().Get("Connection") != tc.wantConnection || !g.routes[0].breaker.open {
				t.Fatalf("status=%d body=%v connection=%q open=%v", w.Code, body, w.Header().Get("Connection"), g.routes[0].breaker.open)
			}
		})
	}
}

func TestRetryLosesHealthyBackendKeepsBreakerNeutral(t *testing.T) {
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: "http://backend"}, Retry: &RetryConfig{Attempts: 2, Backoff: "fixed", InitialDelay: "1ns", On: []int{503}}, CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "1m", Cooldown: "1m"}})
	body := &closeTrackingResponseBody{ReadCloser: io.NopCloser(strings.NewReader("unavailable"))}
	calls := 0
	g.transport = featureTransport(func(*http.Request) (*http.Response, error) {
		calls++
		g.routes[0].balancer.setHealthy(0, false)
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: body}, nil
	})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	var response map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 503 || response["error"] != "no_healthy_upstream" || calls != 1 || !body.closed || g.routes[0].breaker.open || len(g.routes[0].breaker.failures) != 0 {
		t.Fatalf("status=%d body=%v calls=%d closed=%v breaker=%+v", w.Code, response, calls, body.closed, g.routes[0].breaker)
	}
}

func (b *closeTrackingResponseBody) Close() error {
	b.closed = true
	return b.ReadCloser.Close()
}

// Exercise the full request path so extracting response helpers cannot change
// which failures open the breaker or how incomplete responses are handled.
func TestForwardResponseFailureAccounting(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		status, wantStatus                       int
		transform, brokenBody, failedClientWrite bool
		wantOpen, wantAbort                      bool
	}{
		{name: "success", status: 200, wantStatus: 200},
		{name: "backend error", status: 503, wantStatus: 503, wantOpen: true},
		{name: "protocol upgrade", status: 101, wantStatus: 502, wantOpen: true},
		{name: "invalid JSON is neutral", status: 200, transform: true, wantStatus: 502},
		{name: "invalid JSON with backend error", status: 503, transform: true, wantStatus: 502, wantOpen: true},
		{name: "broken backend body", status: 200, brokenBody: true, wantStatus: 200, wantOpen: true, wantAbort: true},
		{name: "failed client write", status: 200, failedClientWrite: true, wantStatus: 200, wantAbort: true},
		{name: "failed client write with backend error", status: 503, failedClientWrite: true, wantStatus: 503, wantAbort: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: "http://backend"}, CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "1m", Cooldown: "1m"}}
			if tc.transform {
				route.ResponseTransform = &ResponseTransformConfig{Body: &ResponseTransformBody{Envelope: map[string]any{"data": "$body"}}}
			}
			g := testGateway(t, route)
			body := &closeTrackingResponseBody{ReadCloser: io.NopCloser(strings.NewReader("not JSON"))}
			if tc.brokenBody {
				body.ReadCloser = &interruptedUpload{}
			}
			g.transport = featureTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
			})
			recorder := httptest.NewRecorder()
			var writer http.ResponseWriter = recorder
			if tc.failedClientWrite {
				writer = failingDownstream{recorder}
			}
			aborted := false
			func() {
				defer func() {
					if failure := recover(); failure != nil {
						if failure != http.ErrAbortHandler {
							panic(failure)
						}
						aborted = true
					}
				}()
				g.ServeHTTP(writer, httptest.NewRequest("GET", "/", nil))
			}()
			if recorder.Code != tc.wantStatus || g.routes[0].breaker.open != tc.wantOpen || aborted != tc.wantAbort {
				t.Fatalf("status=%d open=%v aborted=%v; want status=%d open=%v aborted=%v", recorder.Code, g.routes[0].breaker.open, aborted, tc.wantStatus, tc.wantOpen, tc.wantAbort)
			}
			if !body.closed {
				t.Fatal("backend response body was not closed")
			}
		})
	}
}

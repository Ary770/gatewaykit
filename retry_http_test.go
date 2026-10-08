package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryRealTransportFailures(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		io.WriteString(w, "recovered")
	}))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}, Retry: retryTestConfig()})
	g.transport.(*http.Transport).DisableKeepAlives = true
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || w.Body.String() != "recovered" || calls.Load() != 3 {
		t.Fatalf("status=%d body=%s calls=%d", w.Code, w.Body, calls.Load())
	}
}

func TestRetryDoesNotReplayPOST(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(503)
	}))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"POST"}, Upstream: UpstreamConfig{URL: upstream.URL}, Retry: retryTestConfig()})
	req := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", maxReplayBodyBytes+1)))
	req.Header.Set("Idempotency-Key", "does-not-enable-unsafe-retries")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != 503 || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d", w.Code, calls.Load())
	}
}

func TestRetryBodyTooLargeBeforeAnyAttempt(t *testing.T) {
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"PUT"}, Upstream: UpstreamConfig{URL: "http://unused"}, Retry: retryTestConfig(), CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "1m", Cooldown: "1m"}})
	g.transport = featureTransport(func(*http.Request) (*http.Response, error) { t.Fatal("oversized body sent"); return nil, nil })
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("PUT", "/", strings.NewReader(strings.Repeat("x", maxReplayBodyBytes+1))))
	if w.Code != 413 || g.routes[0].breaker.open {
		t.Fatalf("status=%d breaker=%v", w.Code, g.routes[0].breaker.open)
	}
	req := httptest.NewRequest("PUT", "/", nil)
	req.Body = &interruptedUpload{}
	w = httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != 400 || g.routes[0].breaker.open {
		t.Fatalf("status=%d breaker=%v", w.Code, g.routes[0].breaker.open)
	}
}

func TestRetryStalledUploadDeadline(t *testing.T) {
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"PUT"}, Upstream: UpstreamConfig{URL: "http://unused", Timeout: "50ms"}, Retry: retryTestConfig()})
	g.transport = featureTransport(func(*http.Request) (*http.Response, error) { t.Error("partial body sent"); return nil, io.EOF })
	server := httptest.NewServer(g)
	defer server.Close()
	u, _ := url.Parse(server.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprint(conn, "PUT / HTTP/1.1\r\nHost: gateway\r\nContent-Length: 1000\r\n\r\nx")
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 504 || !response.Close {
		t.Fatalf("status=%d close=%v", response.StatusCode, response.Close)
	}
}

func TestRetryHTTPBackoffDeadline(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer upstream.Close()
	c := retryTestConfig()
	c.InitialDelay = "1h"
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL, Timeout: "50ms"}, Retry: c})
	w := httptest.NewRecorder()
	start := time.Now()
	g.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 504 || calls.Load() != 1 || time.Since(start) > time.Second {
		t.Fatalf("status=%d calls=%d elapsed=%v", w.Code, calls.Load(), time.Since(start))
	}
}

func TestRetryCancellationDuringAttempt(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}, Retry: retryTestConfig(), CircuitBreaker: &CircuitBreakerConfig{Threshold: 1, Window: "1m", Cooldown: "1m"}})
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
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop request")
	}
	if calls.Load() != 1 || g.routes[0].breaker.open {
		t.Fatal("cancelled request retried or opened breaker")
	}
}

func TestRetryConfigStrictFields(t *testing.T) {
	for _, config := range []string{
		"    retry: {attempts: 3, backoff: fixed, initial_delay: 1ms, on: [503], unknown: true}\n",
		"    retry: {attempts: 0, backoff: fixed, initial_delay: 1ms, on: [503]}\n",
	} {
		if _, _, err := decodeConfig(strings.NewReader(minimalConfig + config)); err == nil {
			t.Fatal("invalid retry config accepted")
		}
	}
}

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func retryTestConfig() *RetryConfig {
	return &RetryConfig{Attempts: 3, Backoff: "exponential", InitialDelay: "1ms", On: []int{502, 503, 504}}
}

type retryRoundTripper func(*http.Request) (*http.Response, error)

func (f retryRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type retryResponseBody struct {
	io.Reader
	closed bool
}

func (b *retryResponseBody) Close() error { b.closed = true; return nil }

func TestRetryAttemptsCloseDiscardedBodiesAndReselect(t *testing.T) {
	c := retryTestConfig()
	r := &route{config: RouteConfig{Path: "/api", StripPrefix: true}}
	r.balancer = newBalancer(UpstreamConfig{Targets: []TargetConfig{{URL: "http://a/base"}, {URL: "http://b/base"}}})
	out := httptest.NewRequest("PUT", "http://gateway/api/item?q=1", strings.NewReader("payload"))
	attempts, err := prepareRetry(out, c)
	if err != nil {
		t.Fatal(err)
	}
	first := r.balancer.next()
	var bodies []*retryResponseBody
	var hosts []string
	g := &Gateway{transport: retryRoundTripper(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		data, _ := io.ReadAll(req.Body)
		req.Body.Close()
		if string(data) != "payload" || req.URL.RequestURI() != "/base/item?q=1" {
			t.Fatalf("request %s %q", req.URL, data)
		}
		body := &retryResponseBody{Reader: strings.NewReader("reply")}
		bodies = append(bodies, body)
		status := 503
		if len(hosts) == 3 {
			status = 200
		}
		return &http.Response{StatusCode: status, Body: body}, nil
	})}
	response, err := g.roundTripAttempts(out, r, out.URL, first, attempts, c)
	if err != nil || response.StatusCode != 200 || strings.Join(hosts, ",") != "a,b,a" {
		t.Fatalf("hosts=%v response=%v err=%v", hosts, response, err)
	}
	if !bodies[0].closed || !bodies[1].closed || bodies[2].closed {
		t.Fatal("wrong response lifecycle")
	}
	response.Body.Close()
}

func TestRetryAttemptBoundaries(t *testing.T) {
	for _, status := range []int{200, 400, 500, 503} {
		c := retryTestConfig()
		r := &route{config: RouteConfig{Path: "/"}, balancer: newBalancer(UpstreamConfig{URL: "http://backend"})}
		calls := 0
		g := &Gateway{transport: retryRoundTripper(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("final"))}, nil
		})}
		req := httptest.NewRequest("GET", "/", nil)
		response, err := g.roundTripAttempts(req, r, req.URL, r.balancer.next(), 3, c)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		want := 1
		if status == 503 {
			want = 3
		}
		if calls != want {
			t.Fatalf("status=%d calls=%d", status, calls)
		}
	}
	r := &route{config: RouteConfig{Path: "/"}, balancer: newBalancer(UpstreamConfig{URL: "http://backend"})}
	calls := 0
	g := &Gateway{transport: retryRoundTripper(func(req *http.Request) (*http.Response, error) { calls++; return nil, io.ErrUnexpectedEOF })}
	req := httptest.NewRequest("GET", "/", nil)
	_, err := g.roundTripAttempts(req, r, req.URL, r.balancer.next(), 3, retryTestConfig())
	if calls != 3 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	_, err = g.roundTripAttempts(req, r, req.URL, nil, 3, retryTestConfig())
	if !errors.Is(err, errNoHealthyBackends) {
		t.Fatal(err)
	}
}

func TestRetrySharedDeadlineAndCancellation(t *testing.T) {
	for _, cancelImmediately := range []bool{true, false} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		if cancelImmediately {
			cancel()
		}
		calls := 0
		g := &Gateway{transport: retryRoundTripper(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 503, Body: http.NoBody}, nil
		})}
		c := retryTestConfig()
		c.InitialDelay = "1h"
		r := &route{config: RouteConfig{Path: "/"}, balancer: newBalancer(UpstreamConfig{URL: "http://backend"})}
		req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
		started := time.Now()
		_, err := g.roundTripAttempts(req, r, req.URL, r.balancer.next(), 3, c)
		cancel()
		want := 1
		if cancelImmediately {
			want = 0
		}
		if calls != want || err == nil || time.Since(started) > time.Second {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if upstreamErrorStatus(io.EOF, ctx) != 504 || upstreamErrorStatus(io.EOF, context.Background()) != 502 {
		t.Fatal("wrong transport status")
	}
}

func TestRetryValidation(t *testing.T) {
	if err := validateRetry(nil); err != nil {
		t.Fatal(err)
	}
	if err := validateRetry(retryTestConfig()); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*RetryConfig){
		func(c *RetryConfig) { c.Attempts = 0 },
		func(c *RetryConfig) { c.Attempts = 101 },
		func(c *RetryConfig) { c.Backoff = "random" },
		func(c *RetryConfig) { c.InitialDelay = "0s" },
		func(c *RetryConfig) { c.On = nil },
		func(c *RetryConfig) { c.On = []int{200} },
		func(c *RetryConfig) { c.On = []int{600} },
		func(c *RetryConfig) { c.On = []int{503, 503} },
	} {
		c := retryTestConfig()
		mutate(c)
		if validateRetry(c) == nil {
			t.Fatalf("accepted invalid config %+v", c)
		}
	}
}

func TestRetryReplayAndMethodSafety(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "OPTIONS", "TRACE", "PUT", "DELETE", "POST", "PATCH", "CUSTOM"} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/", strings.NewReader("payload"))
			req.Header.Set("Idempotency-Key", "not-proof-of-backend-support")
			req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("payload")), nil }
			original := req.Body
			attempts, err := prepareRetry(req, retryTestConfig())
			if err != nil {
				t.Fatal(err)
			}
			if !retryableMethod(method) {
				if attempts != 1 || req.Body != original || req.GetBody != nil {
					t.Fatal("unsafe request was buffered/replayed")
				}
				return
			}
			if attempts != 3 || req.ContentLength != 7 {
				t.Fatalf("attempts=%d length=%d", attempts, req.ContentLength)
			}
			for i := 0; i < 3; i++ {
				body, _ := req.GetBody()
				data, err := io.ReadAll(body)
				body.Close()
				if err != nil || string(data) != "payload" {
					t.Fatalf("replay=%q err=%v", data, err)
				}
			}
		})
	}
}

type retryFailingBody struct{ closed bool }

func (b *retryFailingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (b *retryFailingBody) Close() error             { b.closed = true; return nil }

func TestRetryBodyBoundaries(t *testing.T) {
	for _, size := range []int{0, maxReplayBodyBytes, maxReplayBodyBytes + 1} {
		req := httptest.NewRequest("PUT", "/", strings.NewReader(strings.Repeat("x", size)))
		_, err := prepareRetry(req, retryTestConfig())
		if errors.Is(err, errReplayBodyTooLarge) != (size > maxReplayBodyBytes) {
			t.Fatalf("size=%d err=%v", size, err)
		}
	}
	req := httptest.NewRequest("PUT", "/", nil)
	broken := &retryFailingBody{}
	req.Body = broken
	if _, err := prepareRetry(req, retryTestConfig()); !errors.Is(err, io.ErrUnexpectedEOF) || !broken.closed {
		t.Fatalf("err=%v closed=%v", err, broken.closed)
	}
	for _, body := range []io.ReadCloser{nil, http.NoBody} {
		req.Body = body
		if attempts, err := prepareRetry(req, retryTestConfig()); attempts != 3 || err != nil {
			t.Fatalf("%d %v", attempts, err)
		}
	}
	for _, c := range []*RetryConfig{nil, {Attempts: 1}} {
		req.Body = &retryFailingBody{}
		if attempts, err := prepareRetry(req, c); attempts != 1 || err != nil {
			t.Fatal("streaming request read early")
		}
	}
}

func TestRetryBackoff(t *testing.T) {
	c := retryTestConfig()
	for i, want := range []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond} {
		if got := c.delay(i + 1); got != want {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	c.Backoff = "fixed"
	if c.delay(10) != time.Millisecond {
		t.Fatal("fixed delay changed")
	}
	c.Backoff = "exponential"
	c.InitialDelay = "2000000h"
	if c.delay(100) != time.Duration(1<<63-1) {
		t.Fatal("backoff overflowed")
	}
	if !c.includes(503) || c.includes(500) || (*RetryConfig)(nil).includes(503) {
		t.Fatal("wrong status selection")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitBackoff(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := waitBackoff(context.Background(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
}

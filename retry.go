// Tries selected failures again after a delay. Saves the request body once
// and returns only the final response. All attempts share one time limit,
// one place in the request count, and one result for the circuit breaker.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Attempts includes the first send. Retry only methods intended to have the same effect when repeated.
type RetryConfig struct {
	Attempts     int    `yaml:"attempts"`
	Backoff      string `yaml:"backoff"`
	InitialDelay string `yaml:"initial_delay"`
	On           []int  `yaml:"on"`
}

const maxReplayBodyBytes = 1 << 20

var errReplayBodyTooLarge = errors.New("retry request body exceeds 1 MiB")
var errNoHealthyBackends = errors.New("no healthy upstreams")

// validateRetry checks the total attempt count, delay settings, and retryable HTTP statuses.
func validateRetry(c *RetryConfig) error {
	if c == nil {
		return nil
	}
	if c.Attempts < 1 || c.Attempts > 100 {
		return fmt.Errorf("attempts must be between 1 and 100 (including the first)")
	}
	if c.Backoff != "fixed" && c.Backoff != "exponential" {
		return fmt.Errorf("backoff must be fixed or exponential")
	}
	if _, err := duration(c.InitialDelay); err != nil {
		return fmt.Errorf("initial_delay: %w", err)
	}
	if len(c.On) == 0 {
		return fmt.Errorf("on must contain at least one HTTP status")
	}
	seen := make(map[int]bool)
	for _, status := range c.On {
		if status < 400 || status > 599 || seen[status] {
			return fmt.Errorf("on must contain unique HTTP error statuses (400-599)")
		}
		seen[status] = true
	}
	return nil
}

// retryableMethod allows retries for HTTP methods intended to have the same effect when repeated.
// POST/PATCH are sent once, even if the client provides an idempotency key.
func retryableMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// prepareRetry saves the full body before sending, so a retry cannot reuse an incomplete upload.
// Requests that will not be retried keep sending their bodies as they are read.
func prepareRetry(req *http.Request, c *RetryConfig) (int, error) {
	if err := req.Context().Err(); err != nil {
		return 0, err
	}
	if !retryableMethod(req.Method) {
		// Changing the body must not enable automatic retries of POST/PATCH
		// just because the client supplied an idempotency header.
		req.GetBody = nil
		return 1, nil
	}
	if c == nil || c.Attempts <= 1 {
		return 1, nil
	}
	if req.Body == nil || req.Body == http.NoBody {
		return c.Attempts, nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxReplayBodyBytes+1))
	_ = req.Body.Close()
	if err != nil {
		return 0, err
	}
	if len(body) > maxReplayBodyBytes {
		return 0, errReplayBodyTooLarge
	}
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	req.Body, _ = req.GetBody()
	return c.Attempts, nil
}

func (c *RetryConfig) includes(status int) bool {
	if c == nil {
		return false
	}
	for _, configured := range c.On {
		if configured == status {
			return true
		}
	}
	return false
}

// retryNumber 1 means the wait before attempt 2.
func (c *RetryConfig) delay(retryNumber int) time.Duration {
	delay, _ := duration(c.InitialDelay)
	if c.Backoff == "exponential" {
		for i := 1; i < retryNumber; i++ {
			if delay > time.Duration(1<<63-1)/2 {
				return time.Duration(1<<63 - 1)
			}
			delay *= 2
		}
	}
	return delay
}

// waitBackoff waits before retrying and stops immediately if the request is canceled.
func waitBackoff(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// roundTripAttempts sends the request and retries failures allowed by the settings.
// Return only the final response. Every attempt uses the same time limit.
func (g *Gateway) roundTripAttempts(out *http.Request, r *route, incoming *url.URL, target *url.URL, attempts int, c *RetryConfig) (*http.Response, error) {
	for attempt := 0; attempt < attempts; attempt++ {
		if err := out.Context().Err(); err != nil {
			return nil, err
		}
		if attempt > 0 {
			// Wait before retrying, then choose a currently healthy backend.
			if err := waitBackoff(out.Context(), c.delay(attempt)); err != nil {
				return nil, err
			}
			target = r.balancer.next()
		}
		if target == nil {
			return nil, errNoHealthyBackends
		}
		// Set the backend address and remove the route prefix if configured.
		request := out.Clone(out.Context())
		request.URL = upstreamURL(incoming, target, r)
		request.Host = target.Host
		if attempt > 0 && out.GetBody != nil {
			// Send the same saved body again for this retry.
			var err error
			request.Body, err = out.GetBody()
			if err != nil {
				return nil, err
			}
		}
		// Make the actual HTTP request using the shared backend transport.
		response, err := g.transport.RoundTrip(request)
		status := 0
		if err != nil {
			if response != nil && response.Body != nil {
				response.Body.Close()
				response = nil
			}
			status = upstreamErrorStatus(err, out.Context())
		} else {
			status = response.StatusCode
		}
		// Return the response or error when we should stop retrying.
		if attempt+1 == attempts || out.Context().Err() != nil || !c.includes(status) {
			return response, err
		}
		if response != nil {
			// Close this response because we are going to try again.
			response.Body.Close()
		}
	}
	panic("retry attempts must be positive")
}

// upstreamErrorStatus returns 504 for a timeout, otherwise 502 for a connection error.
// It also checks the time limit because timeout and cancellation can be reported in either order.
func upstreamErrorStatus(err error, ctx context.Context) int {
	var netErr net.Error
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline && !time.Now().Before(deadline) || errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

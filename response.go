// Returns backend responses and errors to the client.
// Start with relayUpstreamResponse after the gateway receives a backend response.

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// writeUpstreamError returns 503 for unavailable backends, 504 for timeouts, or 502 otherwise.
func writeUpstreamError(w http.ResponseWriter, ctx context.Context, routePath string, err error) breakerOutcome {
	if errors.Is(err, errNoHealthyBackends) {
		writeError(w, http.StatusServiceUnavailable, "no_healthy_upstream")
		return breakerNeutral
	}
	status, message := http.StatusBadGateway, "bad_gateway"
	// Check the clock too; a client read timeout can cancel the request first.
	if upstreamErrorStatus(err, ctx) == http.StatusGatewayTimeout {
		status, message = http.StatusGatewayTimeout, "gateway_timeout"
		// Close this client connection; it cannot be reused after a read timeout.
		w.Header().Set("Connection", "close")
	}
	slog.Warn("upstream request failed", "route", routePath, "category", message)
	writeError(w, status, message)
	return breakerFailure
}

// relayUpstreamResponse applies response changes and sends the result to the client.
// Return the breaker result and any body-copy error; the caller handles connection closure.
func (g *Gateway) relayUpstreamResponse(w http.ResponseWriter, req *http.Request, r *route, response *http.Response, values transformValues, ctx context.Context, deadline time.Time) (breakerOutcome, error) {
	if response.StatusCode == http.StatusSwitchingProtocols {
		writeError(w, http.StatusBadGateway, "unsupported_protocol_upgrade")
		return breakerFailure, nil
	}
	// Track backend read errors separately from client write errors.
	body := &observedUpstreamBody{ReadCloser: response.Body}
	response.Body = body
	removeHopHeaders(response.Header)
	// FEATURE ADD-ON: Response transformation - apply configured header and JSON changes.
	if err := g.transformResponse(response, req, r, values); err != nil {
		outcome := breakerNeutral
		if body.failed || response.StatusCode >= 500 {
			outcome = breakerFailure
		}
		status := http.StatusBadGateway
		if !time.Now().Before(deadline) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			w.Header().Set("Connection", "close")
		}
		writeError(w, status, "response_transform_failed")
		return outcome, nil
	}
	if err := copyUpstreamResponse(w, response); err != nil {
		if body.failed {
			return breakerFailure, err
		}
		// A failed client write does not count as a backend failure.
		return breakerNeutral, err
	}
	if response.StatusCode >= 500 {
		return breakerFailure, nil
	}
	return breakerSuccess, nil
}

// copyUpstreamResponse sends the final headers, status, and body to the client.
func copyUpstreamResponse(w http.ResponseWriter, response *http.Response) error {
	removeHopHeaders(response.Header)
	for key, values := range response.Header {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(response.StatusCode)
	_, err := io.Copy(w, response.Body)
	return err
}

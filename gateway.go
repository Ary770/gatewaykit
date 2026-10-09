// Follow a request through ServeHTTP, forward, and forwardPreparedRequest.
// retry.go sends it to the backend; response.go returns the response to the client.

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type route struct {
	config   RouteConfig
	timeout  time.Duration
	balancer *balancer
	limiter  *rateLimiter
	breaker  *circuitBreaker
}

type Gateway struct {
	routes         []*route
	transport      http.RoundTripper
	started        time.Time
	defaultTimeout time.Duration
	now            func() time.Time
}

// ServeHTTP receives each client request and checks it before contacting a backend.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Set a default time limit while we find the route.
	setClientDeadlines(w, g.defaultTimeout)
	// Answer the gateway's health request without contacting a backend.
	if req.Method == http.MethodGet && req.URL.Path == "/health" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "healthy", "uptime_seconds": int64(g.now().Sub(g.started).Seconds())})
		return
	}
	// Reject paths the gateway and backend could interpret differently.
	if ambiguousPath(req.URL) {
		writeError(w, http.StatusBadRequest, "ambiguous_path")
		return
	}
	// Check the longest matching route for this path and HTTP method.
	r, allowed := g.match(req.URL.Path, req.Method)
	if r == nil {
		writeRouteRejection(w, allowed)
		return
	}
	// Use this route's time limit for the rest of the request.
	deadline := setClientDeadlines(w, r.timeout)
	// Check the API key if this route requires one.
	if !authorized(req, r.config.Auth) {
		w.Header().Set("WWW-Authenticate", `ApiKey realm="gatewaykit"`)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// Check the request limit; an allowed request counts once, even with retries.
	if !g.allowRateLimitedRequest(w, req, r.limiter) {
		return
	}
	// Route checks passed; prepare and send a separate request to the backend.
	g.forward(w, req, r, deadline)
}

// newGateway prepares each route's backend selector, request counter, and circuit breaker.
// Check longer route paths first. No requests or background tasks start here.
func newGateway(c Config, transport http.RoundTripper) *Gateway {
	defaultTimeout, _ := duration(c.Gateway.GlobalTimeout)
	g := &Gateway{transport: transport, started: time.Now(), now: time.Now, defaultTimeout: defaultTimeout}
	for _, rc := range c.Routes {
		timeout, _ := duration(rc.Upstream.Timeout)
		r := &route{config: rc, timeout: timeout}
		r.balancer = newBalancer(rc.Upstream)
		r.breaker = newCircuitBreaker(rc.CircuitBreaker)
		if r.breaker != nil {
			r.breaker.route = rc.Path
		}
		limit := rc.RateLimit
		if limit == nil {
			limit = c.Gateway.GlobalRateLimit
		}
		r.limiter = newRateLimiter(limit)
		g.routes = append(g.routes, r)
	}
	sort.SliceStable(g.routes, func(i, j int) bool { return len(g.routes[i].config.Path) > len(g.routes[j].config.Path) })
	return g
}

// writeRouteRejection distinguishes an unknown path (404) from a disallowed method (405).
func writeRouteRejection(w http.ResponseWriter, allowedMethods []string) {
	if len(allowedMethods) == 0 {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	w.Header().Set("Allow", strings.Join(allowedMethods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
}

// allowRateLimitedRequest counts an allowed request or returns 429/503 with Retry-After.
// Retries do not pass through this check, so they do not count as new client requests.
func (g *Gateway) allowRateLimitedRequest(w http.ResponseWriter, req *http.Request, limiter *rateLimiter) bool {
	if limiter == nil {
		return true
	}
	decision := limiter.allow(clientIP(req), g.now())
	if decision.allowed {
		return true
	}
	retrySeconds := max(int64(1), int64((decision.retryAfter-1)/time.Second)+1)
	w.Header().Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
	if decision.capacityExceeded {
		writeError(w, http.StatusServiceUnavailable, "rate_limit_capacity_exceeded")
		return false
	}
	writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded")
	return false
}

// match checks the longest route path that matches the request.
// Match /api/users and its children, but not /api/users-extra.
// If the method is rejected, do not try a shorter route to bypass its rules.
func (g *Gateway) match(path, method string) (*route, []string) {
	var allowed []string
	matchedLength := -1
	for _, r := range g.routes {
		prefix := r.config.Path
		if matchedLength >= 0 && len(prefix) < matchedLength {
			break
		}
		if prefix != "/" && path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		matchedLength = len(prefix)
		for _, m := range r.config.Methods {
			if m == method {
				return r, nil
			}
			allowed = append(allowed, m)
		}
	}
	sort.Strings(allowed)
	return nil, allowed
}

// forward prepares the backend request and passes it to forwardPreparedRequest.
// Preparing, sending, and returning the response all use the same time limit.
func (g *Gateway) forward(w http.ResponseWriter, req *http.Request, r *route, deadline time.Time) {
	// Stop backend work when the time limit expires or the client disconnects.
	ctx, cancel := context.WithDeadline(req.Context(), deadline)
	defer cancel()
	// Make a separate backend request so the original request stays unchanged.
	out := cloneUpstreamRequest(req, ctx)
	values := transformValues{requestTime: g.now().UTC().Format(time.RFC3339Nano), route: r.config.Path}
	// Apply request changes once and save the body if it needs to be sent again.
	attempts, ready := g.prepareUpstreamRequest(w, out, r, values, deadline)
	if !ready {
		return
	}
	// Continue to backend selection, sending, and response handling.
	g.forwardPreparedRequest(w, req, out, r, values, attempts, deadline)
}

// cloneUpstreamRequest copies the client request and removes headers that cannot be forwarded.
// Use the client's actual connection to identify it, rather than trusting its headers.
func cloneUpstreamRequest(req *http.Request, ctx context.Context) *http.Request {
	out := req.Clone(ctx)
	out.RequestURI = ""
	out.Close = false
	out.Trailer = nil
	out.TransferEncoding = nil
	out.Header = req.Header.Clone()
	removeHopHeaders(out.Header)
	// Replace client-supplied forwarding headers with values from the actual connection.
	out.Header.Del("Forwarded")
	out.Header.Del("X-Forwarded-For")
	out.Header.Del("X-Forwarded-Host")
	out.Header.Del("X-Forwarded-Proto")
	out.Header.Set("X-Forwarded-For", clientIP(req))
	out.Header.Set("X-Forwarded-Host", req.Host)
	proto := "http"
	if req.TLS != nil {
		proto = "https"
	}
	out.Header.Set("X-Forwarded-Proto", proto)
	return out
}

// prepareUpstreamRequest applies configured request changes once.
// Save the body if retries need it. Stop here if preparation fails.
func (g *Gateway) prepareUpstreamRequest(w http.ResponseWriter, out *http.Request, r *route, values transformValues, deadline time.Time) (int, bool) {
	if status, err := g.transformRequest(out, r, values); err != nil {
		if !time.Now().Before(deadline) || errors.Is(out.Context().Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			w.Header().Set("Connection", "close")
		}
		writeError(w, status, "request_transform_failed")
		return 0, false
	}
	attempts, err := prepareRetry(out, r.config.Retry)
	if err != nil {
		status, message := http.StatusBadRequest, "request_body_read_failed"
		if errors.Is(err, errReplayBodyTooLarge) {
			status, message = http.StatusRequestEntityTooLarge, "retry_body_too_large"
		}
		if upstreamErrorStatus(err, out.Context()) == http.StatusGatewayTimeout {
			status, message = http.StatusGatewayTimeout, "gateway_timeout"
			w.Header().Set("Connection", "close")
		}
		writeError(w, status, message)
		return 0, false
	}
	return attempts, true
}

// forwardPreparedRequest chooses a backend, checks the breaker, and sends the request.
// response.go handles the response. Record the breaker result after that work finishes.
func (g *Gateway) forwardPreparedRequest(w http.ResponseWriter, req, out *http.Request, r *route, values transformValues, attempts int, deadline time.Time) {
	// Choose a backend that is currently marked healthy.
	target := r.balancer.next()
	if target == nil {
		writeError(w, http.StatusServiceUnavailable, "no_healthy_upstream")
		return
	}
	var upload *observedRequestBody
	if out.Body != nil {
		upload = &observedRequestBody{ReadCloser: out.Body}
		out.Body = upload
	}
	// Ask the circuit breaker whether this route can contact a backend now.
	permit, retryAfter := r.breaker.admit(g.now())
	if permit == nil {
		seconds := max(int64(1), int64((retryAfter-1)/time.Second)+1)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "service_unavailable", "retry_after": seconds})
		return
	}
	// Record one circuit-breaker result after response handling finishes.
	outcome := breakerNeutral
	defer func() {
		if req.Context().Err() != nil || upload != nil && upload.failed.Load() {
			outcome = breakerNeutral
		}
		permit.finish(outcome, g.now())
	}()
	// Send through retry.go; get the final backend response or a sending error.
	response, err := g.roundTripAttempts(out, r, req.URL, target, attempts, r.config.Retry)
	if err != nil {
		outcome = writeUpstreamError(w, out.Context(), r.config.Path, err)
		return
	}
	defer response.Body.Close()
	// Return the response and record its result before handling an interrupted body.
	outcome, err = g.relayUpstreamResponse(w, req, r, response, values, out.Context(), deadline)
	if err != nil {
		// The response has started; close the connection to show that the body is incomplete.
		slog.Warn("upstream response interrupted", "route", r.config.Path)
		panic(http.ErrAbortHandler)
	}
}

// Track upstream reads separately from downstream write errors.
type observedUpstreamBody struct {
	io.ReadCloser
	failed bool
}

func (b *observedUpstreamBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		b.failed = true
	}
	return n, err
}

// net/http may read uploads on a transport goroutine.
type observedRequestBody struct {
	io.ReadCloser
	failed atomic.Bool
}

func (b *observedRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		b.failed.Store(true)
	}
	return n, err
}

// upstreamURL builds the backend URL, removes the route prefix if configured,
// and keeps the query string and path encoding.
func upstreamURL(in, target *url.URL, r *route) *url.URL {
	result := *target
	path := in.EscapedPath()
	if path == "" {
		path = "/"
	}
	if r.config.StripPrefix {
		path = stripEscapedPrefix(path, r.config.Path)
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	raw := strings.TrimRight(target.EscapedPath(), "/") + path
	result.Path, _ = url.PathUnescape(raw)
	result.RawPath = raw
	result.RawQuery = target.RawQuery
	if in.RawQuery != "" {
		if result.RawQuery != "" {
			result.RawQuery += "&"
		}
		result.RawQuery += in.RawQuery
	}
	result.ForceQuery = in.ForceQuery || target.ForceQuery
	return &result
}

// removeHopHeaders removes headers that apply only to the original connection.
// Also remove any headers named in the Connection header.
func removeHopHeaders(h http.Header) {
	for _, line := range h.Values("Connection") {
		for _, key := range strings.Split(line, ",") {
			h.Del(strings.TrimSpace(key))
		}
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(key)
	}
}

// clientIP reads the client's IP address from its connection, not from its headers.
func clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("response write failed", "status", status)
	}
}

// authorized checks that one API-key header contains an allowed key.
// Without auth settings, allow the request. Compare every allowed key without stopping at a match.
func authorized(req *http.Request, auth *AuthConfig) bool {
	if auth == nil {
		return true
	}
	values := req.Header.Values(auth.Header)
	if len(values) != 1 {
		return false
	}
	matched := 0
	for _, key := range auth.Keys {
		matched |= subtle.ConstantTimeCompare([]byte(values[0]), []byte(key))
	}
	return matched == 1
}

// Reject paths that common backend routers normalize differently. Matching the
// decoded path also prevents encoded ordinary characters from bypassing auth.
func ambiguousPath(u *url.URL) bool {
	raw := strings.ToLower(u.EscapedPath())
	if strings.Contains(raw, "%2f") || strings.Contains(u.Path, "\\") || strings.Contains(u.Path, "//") {
		return true
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

// The decoded prefix has already matched. Each escape consumes one decoded byte,
// so this preserves the raw encoding of the suffix, including UTF-8 bytes.
func stripEscapedPrefix(raw, prefix string) string {
	index := 0
	for consumed := 0; consumed < len(prefix); consumed++ {
		if raw[index] == '%' {
			index += 3
		} else {
			index++
		}
	}
	return raw[index:]
}

// setClientDeadlines limits how long client uploads and response writes can take.
// Return the same deadline for the backend request.
func setClientDeadlines(w http.ResponseWriter, timeout time.Duration) time.Time {
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(timeout)
	// Backend cancellation alone cannot stop client reads and writes.
	// Allow one extra second to send a 504 response after a read timeout.
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline.Add(time.Second))
	return deadline
}

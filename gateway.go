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
	"time"
)

type route struct {
	config   RouteConfig
	timeout  time.Duration
	balancer *balancer
	limiter  *rateLimiter
}

type Gateway struct {
	routes         []*route
	transport      http.RoundTripper
	started        time.Time
	defaultTimeout time.Duration
	now            func() time.Time
}

func newGateway(c Config, transport http.RoundTripper) *Gateway {
	defaultTimeout, _ := duration(c.Gateway.GlobalTimeout)
	g := &Gateway{transport: transport, started: time.Now(), now: time.Now, defaultTimeout: defaultTimeout}
	for _, rc := range c.Routes {
		timeout, _ := duration(rc.Upstream.Timeout)
		r := &route{config: rc, timeout: timeout}
		r.balancer = newBalancer(rc.Upstream)
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

func (g *Gateway) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	setClientDeadlines(w, g.defaultTimeout)
	if req.Method == http.MethodGet && req.URL.Path == "/health" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "healthy", "uptime_seconds": int64(g.now().Sub(g.started).Seconds())})
		return
	}
	if ambiguousPath(req.URL) {
		writeError(w, http.StatusBadRequest, "ambiguous_path")
		return
	}
	r, allowed := g.match(req.URL.Path, req.Method)
	if r == nil {
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		} else {
			writeError(w, http.StatusNotFound, "not_found")
		}
		return
	}
	setClientDeadlines(w, r.timeout)
	if !authorized(req, r.config.Auth) {
		w.Header().Set("WWW-Authenticate", `ApiKey realm="gatewaykit"`)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.limiter != nil {
		allowed, retry, capacity := r.limiter.allow(clientIP(req), g.now())
		if !allowed {
			seconds := max(int64(1), int64((retry-1)/time.Second)+1)
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
			if !capacity {
				writeError(w, http.StatusServiceUnavailable, "rate_limit_capacity_exceeded")
			} else {
				writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded")
			}
			return
		}
	}
	g.forward(w, req, r, r.balancer.next())
}

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

func (g *Gateway) forward(w http.ResponseWriter, req *http.Request, r *route, target *url.URL) {
	ctx, cancel := context.WithTimeout(req.Context(), r.timeout)
	defer cancel()
	out := req.Clone(ctx)
	out.RequestURI = ""
	out.URL = upstreamURL(req.URL, target, r)
	out.Host = target.Host
	out.Header = req.Header.Clone()
	removeHopHeaders(out.Header)
	// Forwarded identity is derived from the socket, never from client-supplied headers.
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
	response, err := g.transport.RoundTrip(out)
	if err != nil {
		status, message := http.StatusBadGateway, "bad_gateway"
		var netErr net.Error
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
			status, message = http.StatusGatewayTimeout, "gateway_timeout"
		}
		slog.Warn("upstream request failed", "route", r.config.Path, "target", target.Host, "category", message)
		writeError(w, status, message)
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusSwitchingProtocols {
		writeError(w, http.StatusBadGateway, "unsupported_protocol_upgrade")
		return
	}
	removeHopHeaders(response.Header)
	for key, values := range response.Header {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		// Headers may already be sent; abort instead of presenting a truncated body as complete.
		slog.Warn("upstream response interrupted", "route", r.config.Path)
		panic(http.ErrAbortHandler)
	}
}

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

func setClientDeadlines(w http.ResponseWriter, timeout time.Duration) {
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(timeout)
	// Incoming request bodies and blocked downstream writes are independent of
	// the outbound context. The write grace lets us send a 504 after a read timeout.
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline.Add(time.Second))
}

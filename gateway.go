package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type route struct {
	config      RouteConfig
	escapedPath string
	timeout     time.Duration
	targets     []*url.URL
}

type Gateway struct {
	routes    []*route
	transport http.RoundTripper
	started   time.Time
	now       func() time.Time
}

func newGateway(c Config, transport http.RoundTripper) *Gateway {
	g := &Gateway{transport: transport, started: time.Now(), now: time.Now}
	for _, rc := range c.Routes {
		timeout, _ := duration(rc.Upstream.Timeout)
		r := &route{config: rc, escapedPath: (&url.URL{Path: rc.Path}).EscapedPath(), timeout: timeout}
		if rc.Upstream.URL != "" {
			u, _ := url.Parse(rc.Upstream.URL)
			r.targets = append(r.targets, u)
		}
		for _, t := range rc.Upstream.Targets {
			u, _ := url.Parse(t.URL)
			r.targets = append(r.targets, u)
		}
		g.routes = append(g.routes, r)
	}
	sort.SliceStable(g.routes, func(i, j int) bool { return len(g.routes[i].escapedPath) > len(g.routes[j].escapedPath) })
	return g
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet && req.URL.Path == "/health" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "healthy", "uptime_seconds": int64(g.now().Sub(g.started).Seconds())})
		return
	}
	r, allowed := g.match(req.URL.EscapedPath(), req.Method)
	if r == nil {
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		} else {
			writeError(w, http.StatusNotFound, "not_found")
		}
		return
	}
	g.forward(w, req, r, r.targets[0])
}

func (g *Gateway) match(path, method string) (*route, []string) {
	var allowed []string
	matchedLength := -1
	for _, r := range g.routes {
		prefix := r.escapedPath
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
		if req.Context().Err() != nil {
			return
		}
		status, message := http.StatusBadGateway, "bad_gateway"
		var netErr net.Error
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
			status, message = http.StatusGatewayTimeout, "gateway_timeout"
		}
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
	if r.config.StripPrefix {
		path = strings.TrimPrefix(path, r.escapedPath)
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
		slog.Debug("response write failed", "status", fmt.Sprint(status))
	}
}

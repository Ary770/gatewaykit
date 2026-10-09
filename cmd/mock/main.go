// Starts local demo backends that return predictable test responses.
// The responses are mocked, but the gateway calls these servers over real HTTP.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// main reads the demo ports and stops the servers when the app is told to exit.
func main() {
	ports := flag.String("ports", "3001,3002,3003,3004,3005,3006", "comma-separated loopback ports")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, *ports); err != nil {
		slog.Error("mock upstreams stopped", "error", err)
		os.Exit(1)
	}
}

// serve opens all demo ports before starting the servers. It closes every port
// if startup fails or the app stops.
func serve(ctx context.Context, ports string) error {
	var listeners []net.Listener
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for _, port := range strings.Split(ports, ",") {
		number, err := strconv.Atoi(strings.TrimSpace(port))
		if err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("invalid port %q", port)
		}
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", number))
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
	}
	failures := make(chan error, len(listeners))
	var servers []*http.Server
	defer func() {
		for _, server := range servers {
			_ = server.Close()
		}
	}()
	for _, listener := range listeners {
		name := listener.Addr().String()
		server := &http.Server{Handler: mockHandler(name), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
		servers = append(servers, server)
		go func() { failures <- server.Serve(listener) }()
		slog.Info("mock upstream listening", "address", name)
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-failures:
		return err
	}
}

// mockHandler echoes requests or returns a chosen delay, status, or health response.
func mockHandler(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Mock-Upstream", name)
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, `{"status":"healthy"}`)
			return
		}
		if r.URL.Path == "/slow" {
			delay := 2 * time.Second
			if raw := r.URL.Query().Get("delay"); raw != "" {
				var err error
				delay, err = time.ParseDuration(raw)
				if err != nil || delay < 0 || delay > time.Minute {
					http.Error(w, "invalid delay", 400)
					return
				}
			}
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "request body too large or unreadable", 413)
			return
		}
		status := http.StatusOK
		if raw := r.URL.Query().Get("status"); raw != "" {
			status, err = strconv.Atoi(raw)
			if err != nil || status < 200 || status > 599 {
				http.Error(w, "invalid status", 400)
				return
			}
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"upstream": name, "method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery, "body": string(body), "headers": r.Header})
	})
}

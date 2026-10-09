package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveConfigPath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		env, want string
		wantErr   string
	}{
		{name: "environment", env: "env.yaml", want: "env.yaml"},
		{name: "positional overrides environment", args: []string{"arg.yaml"}, env: "env.yaml", want: "arg.yaml"},
		{name: "flag overrides environment", args: []string{"-config", "flag.yaml"}, env: "env.yaml", want: "flag.yaml"},
		{name: "empty flag falls back to environment", args: []string{"-config", ""}, env: "env.yaml", want: "env.yaml"},
		{name: "required", wantErr: "configuration required"},
		{name: "multiple paths", args: []string{"a", "b"}, wantErr: "provide one"},
		{name: "conflicting paths", args: []string{"-config", "a", "b"}, wantErr: "provide one"},
		{name: "unknown flag", args: []string{"-unknown"}, wantErr: "flag provided but not defined"},
		{name: "missing flag value", args: []string{"-config"}, wantErr: "flag needs an argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := resolveConfigPath(tc.args, tc.env)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || path != "" {
					t.Fatalf("got path %q, error %v; want empty path and %q", path, err, tc.wantErr)
				}
				return
			}
			if err != nil || path != tc.want {
				t.Fatalf("got path %q, error %v; want %q", path, err, tc.want)
			}
		})
	}
	if _, err := resolveConfigPath([]string{"-help"}, ""); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help must preserve flag.ErrHelp, got %v", err)
	}
}

func TestNewUpstreamTransportPreservesCompressedResponse(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(`{"message":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Automatic compression negotiation would change what the backend receives.
		w.Header().Set("X-Received-Accept-Encoding", r.Header.Get("Accept-Encoding"))
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	defer upstream.Close()
	transport := newUpstreamTransport()
	defer transport.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("X-Received-Accept-Encoding") != "" {
		t.Fatal("transport added compression negotiation")
	}
	if response.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(body, compressed.Bytes()) {
		t.Fatal("transport changed the compressed response representation")
	}
}

func TestConfigPathSelection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		env, want string
	}{
		{"required", nil, "", "configuration required"},
		{"environment", nil, "env-missing.yaml", "env-missing.yaml"},
		{"positional", []string{"arg-missing.yaml"}, "env.yaml", "arg-missing.yaml"},
		{"flag", []string{"-config", "flag-missing.yaml"}, "env.yaml", "flag-missing.yaml"},
		{"multiple", []string{"a", "b"}, "", "provide one"},
		{"ambiguous", []string{"-config", "a", "b"}, "", "provide one"},
		{"unknown flag", []string{"-unknown"}, "", "flag provided but not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(context.Background(), tc.args, tc.env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v want %s", err, tc.want)
			}
		})
	}
}

func TestPortAlreadyInUse(t *testing.T) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("gateway:\n  port: %d\n", listener.Addr().(*net.TCPAddr).Port)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{path}, ""); err == nil || !strings.Contains(err.Error(), "listen:") {
		t.Fatalf("expected bind error, got %v", err)
	}
}

func TestRunServesAndDrainsOnShutdown(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(204) }))
	defer upstream.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	config := fmt.Sprintf("gateway:\n  port: %d\nroutes:\n  - path: /\n    methods: [GET]\n    upstream:\n      url: %s\n", port, upstream.URL)
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- run(ctx, []string{"-config", path}, "") }()
	client := &http.Client{Timeout: 2 * time.Second}
	address := fmt.Sprintf("http://127.0.0.1:%d", port)
	ready := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		response, err := client.Get(address + "/health")
		if err == nil {
			_ = response.Body.Close()
			ready = response.StatusCode == 200
			if ready {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		close(release)
		t.Fatal("gateway never started")
	}
	requestDone := make(chan int, 1)
	go func() {
		response, err := client.Get(address + "/work")
		if err != nil {
			requestDone <- 0
			return
		}
		defer response.Body.Close()
		requestDone <- response.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("request did not reach upstream")
	}
	cancel()
	select {
	case err := <-stopped:
		close(release)
		t.Fatalf("exited before draining: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case code := <-requestDone:
		if code != 204 {
			t.Fatalf("in-flight response failed: %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not complete")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not stop")
	}
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMockEndpoints(t *testing.T) {
	for _, tc := range []struct {
		path string
		code int
	}{{"/healthz", 200}, {"/slow?delay=0s", 200}, {"/slow?delay=invalid", 400}, {"/echo?status=503", 503}, {"/echo?status=bad", 400}, {"/echo?status=100", 400}} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mockHandler("mock").ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.code {
				t.Fatalf("got %d want %d", w.Code, tc.code)
			}
		})
	}
}

func TestMockEcho(t *testing.T) {
	req := httptest.NewRequest("POST", "/data?a=1", strings.NewReader(`{"hello":"world"}`))
	req.Header.Set("X-Test", "yes")
	w := httptest.NewRecorder()
	mockHandler("test").ServeHTTP(w, req)
	var result struct {
		Upstream, Method, Path, Query, Body string
		Headers                             map[string][]string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Upstream != "test" || result.Method != "POST" || result.Path != "/data" || result.Query != "a=1" || result.Body != `{"hello":"world"}` || result.Headers["X-Test"][0] != "yes" {
		t.Fatalf("bad echo: %+v", result)
	}
}

func TestMockBodyLimitAndCancellation(t *testing.T) {
	w := httptest.NewRecorder()
	mockHandler("test").ServeHTTP(w, httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", (1<<20)+1))))
	if w.Code != 413 {
		t.Fatalf("oversized body: %d", w.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = httptest.NewRecorder()
	mockHandler("test").ServeHTTP(w, httptest.NewRequest("GET", "/slow", nil).WithContext(ctx))
	if w.Body.Len() != 0 {
		t.Fatal("canceled request wrote body")
	}
}

func TestInvalidPorts(t *testing.T) {
	for _, ports := range []string{"bad", "0", "65536"} {
		if err := serve(context.Background(), ports); err == nil {
			t.Fatalf("accepted invalid ports %q", ports)
		}
	}
}

func TestMockServerLifecycleAndBindFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := serve(context.Background(), fmt.Sprint(port)); err == nil {
		_ = listener.Close()
		t.Fatal("occupied port accepted")
	}
	_ = listener.Close()
	if err := serve(context.Background(), fmt.Sprintf("%d,bad", port)); err == nil {
		t.Fatal("partial startup accepted invalid port")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- serve(ctx, fmt.Sprint(port)) }()
	client := &http.Client{Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
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
		t.Fatal("mock server never became ready")
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("mock server did not stop")
	}
}

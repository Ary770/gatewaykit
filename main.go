package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("config", "", "path to gateway YAML (also accepts GATEWAY_CONFIG or one positional path)")
	flag.Parse()
	if flag.NArg() > 1 || *path != "" && flag.NArg() != 0 {
		return fmt.Errorf("provide one configuration path")
	}
	if *path == "" && flag.NArg() == 1 {
		*path = flag.Arg(0)
	}
	if *path == "" {
		*path = os.Getenv("GATEWAY_CONFIG")
	}
	if *path == "" {
		return fmt.Errorf("configuration required: gatewaykit -config gateway.yaml")
	}
	config, warnings, err := loadConfig(*path)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		slog.Warn(warning)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 20
	transport.MaxResponseHeaderBytes = 1 << 20
	defer transport.CloseIdleConnections()
	gateway := newGateway(config, transport)
	server := &http.Server{
		Addr: fmt.Sprintf(":%d", config.Gateway.Port), Handler: gateway,
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
	}()
	slog.Info("gateway listening", "address", listener.Addr().String(), "routes", len(config.Routes))
	err = server.Serve(listener)
	close(done)
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

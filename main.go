package main

import (
	"context"
	"errors"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv("GATEWAY_CONFIG")); err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, configEnv string) error {
	flags := flag.NewFlagSet("gatewaykit", flag.ContinueOnError)
	path := flags.String("config", "", "path to gateway YAML (also accepts GATEWAY_CONFIG or one positional path)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 || *path != "" && flags.NArg() != 0 {
		return fmt.Errorf("provide one configuration path")
	}
	if *path == "" && flags.NArg() == 1 {
		*path = flags.Arg(0)
	}
	if *path == "" {
		*path = configEnv
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
	stopHealth := gateway.startHealthChecks(ctx)
	defer stopHealth()
	slog.Info("gateway listening", "address", listener.Addr().String(), "routes", len(config.Routes))
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Wait here, so main cannot exit while active requests are still draining.
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}

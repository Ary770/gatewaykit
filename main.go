// Starts and stops the gateway. Reads the settings, prepares the HTTP sender,
// and listens for requests. Start with run.

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

// main tells the app when to stop and reports errors.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv("GATEWAY_CONFIG")); err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

// run starts the app and cleans up when it stops. One HTTP sender is reused
// for backend requests. Incoming requests are handled by Gateway.ServeHTTP.
func run(ctx context.Context, args []string, configEnv string) error {
	path, err := resolveConfigPath(args, configEnv)
	if err != nil {
		return err
	}

	// Read the YAML, fill in defaults, and check the settings before accepting requests.
	config, warnings, err := loadConfig(path)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		slog.Warn(warning)
	}

	// Reuse backend connections and close unused ones when the server stops.
	transport := newUpstreamTransport()
	defer transport.CloseIdleConnections()

	// Create each route and its counters, then give the gateway to the HTTP server.
	gateway := newGateway(config, transport)
	return serveGateway(ctx, gateway, config.Gateway.Port)
}

// resolveConfigPath uses the settings path from the command line first, then GATEWAY_CONFIG.
// It rejects more than one command-line path.
func resolveConfigPath(args []string, configEnv string) (string, error) {
	flags := flag.NewFlagSet("gatewaykit", flag.ContinueOnError)
	path := flags.String("config", "", "path to gateway YAML (also accepts GATEWAY_CONFIG or one positional path)")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() > 1 || *path != "" && flags.NArg() != 0 {
		return "", fmt.Errorf("provide one configuration path")
	}
	if *path == "" && flags.NArg() == 1 {
		*path = flags.Arg(0)
	}
	if *path == "" {
		*path = configEnv
	}
	if *path == "" {
		return "", fmt.Errorf("configuration required: gatewaykit -config gateway.yaml")
	}
	return *path, nil
}

// newUpstreamTransport creates the HTTP sender and enables reuse of backend connections.
// It sends directly to backends and leaves compressed responses unchanged.
func newUpstreamTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 20
	transport.MaxResponseHeaderBytes = 1 << 20
	return transport
}

// serveGateway opens the server port, starts health checks, and stops the server.
// The server calls Gateway.ServeHTTP for every incoming request.
func serveGateway(ctx context.Context, gateway *Gateway, port int) error {
	server := &http.Server{
		Addr: fmt.Sprintf(":%d", port), Handler: gateway,
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	// Open the server port before health checks, so a busy port fails early.
	// When serveGateway returns, stop the checks and wait for them to finish.
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	stopHealth := gateway.startHealthChecks(ctx)
	defer stopHealth()
	slog.Info("gateway listening", "address", listener.Addr().String(), "routes", len(gateway.routes))

	// Run the server in the background while waiting for an error or a stop signal.
	// The channel holds one result so the server can report an error while shutdown waits.
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// Stop accepting requests and give active requests up to five seconds to finish.
		// Use a new shutdown timer because the app's stop signal has already fired.
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			// Close any remaining connections if waiting for requests to finish fails.
			_ = server.Close()
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}

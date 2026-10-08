# GatewayKit decisions

## Scope and priorities

This is a two-hour, AI-assisted take-home project. The first priority is a real, tested HTTP proxy that works with alternate configurations. Next are timeouts, API-key authentication, rate limiting, and backend selection. Retries, transformations, active upstream health checking, and circuit breakers are deliberately deferred. Those configured features will produce startup warnings instead of preventing the provided example configuration from starting.

## Implementation plan

1. Configuration validation and an always-available health endpoint.
2. Routing, method filtering, prefix stripping, and manual HTTP forwarding.
3. Authentication, rate limits, and backend selection, with focused tests.
4. Failure-path and concurrent integration tests.
5. Run instructions, documented semantics and omissions, and a submission ZIP containing Git history.

Go's standard HTTP server and transport provide connection handling; the gateway owns routing and forwarding. No reverse-proxy framework or helper is used. The only runtime dependency is a YAML parser.

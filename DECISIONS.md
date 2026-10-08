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

## Pipeline and module boundaries

`main.go` owns configuration selection, listener lifecycle, signal handling, and transport reuse. `config.go` parses and validates the schema once. `gateway.go` contains the visible request pipeline and HTTP forwarding. `ratelimit.go` and `balancer.go` own their mutable state and locks. Tests live alongside each module; `cmd/mock` is a separate demonstration command.

The pipeline is health -> route/method -> authentication -> rate limit -> backend selection -> forwarding. Authentication precedes rate limiting so invalid keys cannot exhaust an authorized client's quota. Each route owns a limiter and balancer. Network I/O never occurs while either lock is held. No middleware registry, plugin system, dependency-injection container, or shared data store was needed for this scope.

## Proxy choices

The standard HTTP transport supplies connection pooling, HTTP framing, and cancellation, but it does not route or copy requests/responses for us. We explicitly build the upstream URL, remove hop-by-hop headers, replace untrusted forwarding headers, forward the body, copy response status/headers, and stream the response. We use `RoundTrip` so redirects are returned rather than followed and disable automatic decompression to preserve representations.

Bodies are streamed to avoid a memory cost proportional to payload size. A timeout before headers can become a clean 504; after headers are sent, changing the status is impossible, so an interrupted body terminates the downstream exchange. WebSockets, protocol upgrades, and trailers are outside this submission.

## Ambiguities resolved

- The PDF's port example is 8080; `gateway.port` remains configurable for alternate-config evaluation. The omitted-port default is 8080.
- `upstream.timeout` overrides `gateway.global_timeout`. Rendering the PDF was necessary to confirm the nested YAML indentation.
- Route matching uses the longest decoded segment-boundary prefix. Encoded ordinary characters must select the same authentication policy as their plain representation; ambiguous encoded separators, backslashes, repeated slashes, and dot segments are rejected. A more specific route's method rejection does not fall through to a broader route.
- A global rate limit is a default copied to each route, following the example's "unless overridden" comment. `per: global` means all clients of that route, not all routes together.
- Fixed windows start on first accepted request. Sliding windows use exact timestamp histories. Both exclude rejected requests.
- Socket IP is the identity. Trusting arbitrary `X-Forwarded-For` would let a client evade limits. Trusted proxy configuration would be a separate feature.
- Unsupported configuration produces explicit warnings rather than failing startup, because the supplied config includes every stretch feature and must still run. This compatibility choice is appropriate for the exercise, but a production version should offer strict capability validation.

## Why these features came first

The baseline proxy, error behavior, and configurable routes establish one end-to-end slice. Authentication and rate limiting exercise security boundaries and concurrent state. Weighted balancing completes the supplied multi-target route without introducing retry safety issues. Both limiter algorithms fit within the implementation budget and have deterministic boundary tests.

Retries were deferred because retrying POST/PUT requests can duplicate side effects and requires deliberate replay/idempotency semantics. Transformations need explicit behavior for invalid JSON, missing paths, non-JSON responses, and body-size limits. Active health checking adds background lifecycle and recovery policy. Circuit breakers require a defined failure classification and synchronized half-open probes. These should each be implemented and tested completely rather than partially advertised.

## Trade-offs and next steps

The route scan is linear, appropriate for a small static route list. A mutex serializes each route's limiter bookkeeping and backend selection; it never serializes proxy I/O. Exact sliding windows use memory proportional to accepted requests retained in the active window. Identity tables have a 10,000-entry cap per route and lazy expiration; limits are not durable or shared across replicas.

Next, I would implement transformations with bounded buffering and explicit JSON failure contracts, then retry behavior restricted by idempotency policy. Circuit breakers and health checks would follow with controllable clocks and recovery tests. Before production use, I would add traffic metrics, structured failure categorization, trusted-proxy configuration, end-to-end resource limits, and load tests. More features are less valuable than proving the deployed operating limits.

## AI use and verification

Codex generated and edited code and tests under an approved feature plan. A separate plan review challenged scope. The implementation was checked against the rendered PDF, tested with mock HTTP servers and alternate configuration values, and exercised under Go's race detector. Commit history records the build in functional stages. The walkthrough explains the generated code and trade-offs so the candidate can inspect and defend every part of the submission.

## Review-driven corrections

The first independent review reproduced three boundary defects: literal escaped-path matching could choose a weaker policy than the backend's decoded path; timestamps captured before limiter locking could be appended out of order; and an outbound context deadline could not interrupt a blocked inbound-body read. Corrections match decoded paths and reject ambiguous forms, keep limiter evaluation time nondecreasing under the lock, and set independent downstream read/write deadlines. Regression tests cover all three, including real stalled sockets. The review also prompted categorized upstream failure logs and preserving demo logs when an assertion fails.

A clean ZIP extraction subsequently exposed a timing-dependent 502/504 classification race. The downstream read deadline can cancel the request context before its deadline timer records `DeadlineExceeded`. The final correction uses one shared deadline for socket and upstream operations and consults it when classifying errors. Both the implementation pass and independent reviewer repeated the stalled-upload regression 100 times under the race detector before repackaging.

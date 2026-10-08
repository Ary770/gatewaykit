# GatewayKit decisions

## Scope and priorities

The original two-hour submission is preserved at tag `core-submission-2026-10-08`. It prioritized real HTTP proxying, timeouts, authentication, rate limiting and balancing. Follow-up work has added all four features: transformations, active health checks, circuit breakers and safe gateway-managed retries.

## Implementation plan

1. Configuration validation and an always-available health endpoint.
2. Routing, method filtering, prefix stripping, and manual HTTP forwarding.
3. Authentication, rate limits, and backend selection, with focused tests.
4. Failure-path and concurrent integration tests.
5. Run instructions, documented semantics and omissions, and a submission ZIP containing Git history.

Go's standard HTTP server and transport provide connection handling; the gateway owns routing and forwarding. No reverse-proxy framework or helper is used. The only runtime dependency is a YAML parser.

## Pipeline and module boundaries

`main.go` owns configuration selection, listener lifecycle, signal handling, and transport reuse. `config.go` parses and validates the schema once. `gateway.go` contains the visible request pipeline and HTTP forwarding. `ratelimit.go` and `balancer.go` own their mutable state and locks. Tests live alongside each module; `cmd/mock` is a separate demonstration command.

The pipeline is liveness -> route/method -> authentication -> rate limit -> request transformation/replay preparation -> backend selection -> circuit admission -> attempt/backoff loop -> final response transformation/forwarding -> breaker completion. Authentication precedes rate limiting so invalid keys cannot exhaust an authorized client's quota. Each route owns a limiter and balancer. Network I/O never occurs while either lock is held. No middleware registry, plugin system, dependency-injection container, or shared data store was needed for this scope.

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
- The original submission warned about deferred features. The follow-up uses typed, strictly validated configuration for every supplied feature; malformed fields now fail startup.

## Why these features came first

The baseline proxy, error behavior, and configurable routes establish one end-to-end slice. Authentication and rate limiting exercise security boundaries and concurrent state. Weighted balancing completes the supplied multi-target route without introducing retry safety issues. Both limiter algorithms fit within the implementation budget and have deterministic boundary tests.

Retries were deferred because retrying POST/PUT requests can duplicate side effects and requires deliberate replay/idempotency semantics. Transformations need explicit behavior for invalid JSON, missing paths, non-JSON responses, and body-size limits. Active health checking now has an explicit lifecycle: run starts probes after binding and joins them on every exit. Each route target tracks health independently; network I/O happens outside balancing locks, and state transitions reset scheduling credit. Circuit breakers require a defined failure classification and synchronized half-open probes. These should each be implemented and tested completely rather than partially advertised.

## Trade-offs and next steps

The route scan is linear, appropriate for a small static route list. A mutex serializes each route's limiter bookkeeping and backend selection; it never serializes proxy I/O. Exact sliding windows use memory proportional to accepted requests retained in the active window. Identity tables have a 10,000-entry cap per route and lazy expiration; limits are not durable or shared across replicas.

The four originally deferred features are now implemented and tested. Before production use, I would add traffic metrics, structured failure categorization, trusted-proxy configuration, end-to-end resource limits, and load tests. More features are less valuable than proving the deployed operating limits.

## AI use and verification

Codex generated and edited code and tests under an approved feature plan. A separate plan review challenged scope. The implementation was checked against the rendered PDF, tested with mock HTTP servers and alternate configuration values, and exercised under Go's race detector. Commit history records the build in functional stages. The walkthrough explains the generated code and trade-offs so the candidate can inspect and defend every part of the submission.

## Review-driven corrections

The first independent review reproduced three boundary defects: literal escaped-path matching could choose a weaker policy than the backend's decoded path; timestamps captured before limiter locking could be appended out of order; and an outbound context deadline could not interrupt a blocked inbound-body read. Corrections match decoded paths and reject ambiguous forms, keep limiter evaluation time nondecreasing under the lock, and set independent downstream read/write deadlines. Regression tests cover all three, including real stalled sockets. The review also prompted categorized upstream failure logs and preserving demo logs when an assertion fails.

A clean ZIP extraction subsequently exposed a timing-dependent 502/504 classification race. The downstream read deadline can cancel the request context before its deadline timer records `DeadlineExceeded`. The final correction uses one shared deadline for socket and upstream operations and consults it when classifying errors. Both the implementation pass and independent reviewer repeated the stalled-upload regression 100 times under the race detector before repackaging.

## Follow-up: transformations

After the original time-box, request mapping and response envelopes were added
with typed configuration and startup validation. The original core remains tagged.
The implementation supports the supplied expressions directly rather than adding
an expression engine. Body transforms buffer at most 1 MiB of input and reject
output above that limit; all other forwarding remains streamed. Missing mapped
fields become null, arrays are values rather than indexed paths, and conflicting
destinations fail startup. Representation metadata is rebuilt after body changes.
Health checks, circuit breakers and retries were integrated in subsequent follow-up commits.

## Follow-up circuit breakers

One route-owned breaker counts completed upstream failures in a rolling window. A single recovery probe and generation guards prevent concurrent or stale responses from resetting newer state. Local errors and client cancellation release probes neutrally.

## Follow-up retries and integration

Retries are bounded by total attempts and one deadline. Only idempotent HTTP
methods receive gateway-managed retries; a key on POST/PATCH is not enough to
prove a backend's idempotency implementation. Replay bodies are prepared once,
after transformation, and capped at 1 MiB before the first send. This explicit
413 policy avoids sending a partial body or silently changing retry guarantees.
Routes without replay or transformation continue streaming. Go's transport can
also recover replayable requests internally; the configured count is gateway
attempts and not a guarantee of backend execution count.

One rate-limit charge and one breaker result belong to each logical request.
Discarded retry responses are never transformed. Health probes bypass client
policies and affect only target eligibility; they cannot close a route breaker.
Late breaker outcomes are generation-checked. Upload errors and client cancellation
are neutral; truncated upstream reads count as failures, including buffered JSON
reads. Local transform validation/expansion failures are neutral unless the final
upstream status itself is 5xx. Tests combine all four features on one route and
exercise real connection failures, cancellation, recovery and stalled uploads.

Workers used isolated worktrees for transformations, health checks and breaker
state; an integration owner reviewed and landed each independently, owned retries,
and ran the combined race suite and executable demos. The original tagged
submission remains available for comparison; the follow-up is not represented as
work completed during the original two-hour baseline.

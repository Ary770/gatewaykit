# Follow-up feature implementation

The original core baseline is preserved at tag `core-submission-2026-10-08`
(commit `0f3b250`). This work extends it after the core baseline.

## Design and integration order

Keep the existing small, standard-library HTTP gateway. Add typed configuration
and focused modules, without a framework or new runtime dependencies. Preserve
the existing path, auth, rate-limit, streaming, cancellation, and deadline rules.

1. **Transformations** (`transform.go`, configuration, forwarding integration).
   Apply request transforms once before sending; response transforms only to the
   final upstream response. Header-only rules preserve streaming. JSON rules use
   bounded buffering (1 MiB), preserve numbers, reject malformed/unsupported
   content explicitly, and update representation headers. Validate field paths,
   expressions, header names, and values at startup. Test the exact supplied
   mapping/envelope, missing paths, invalid JSON, oversized bodies, and deadlines.
2. **Active health checks** (`health.go`, balancer, application lifecycle).
   Track health per route target. Check GET at the configured origin-relative path
   immediately and periodically; 2xx/3xx are healthy without following redirects.
   Use bounded timeouts, consecutive-failure thresholds, and recovery on success.
   Start targets eligible, exclude unhealthy targets, return 503 when none remain.
   Keep locks out of network I/O and cancel/join all check loops on shutdown.
3. **Circuit breakers** (`circuitbreaker.go`, forwarding integration).
   One breaker per route; count failed logical requests (transport failure or
   final HTTP 5xx) in a sliding window. Open at threshold, reject with 503 plus
   retry_after and Retry-After, then permit one half-open probe after cooldown.
   Ignore client cancellation and stale results from an earlier breaker generation.
   A successful probe closes; a failed probe reopens. Health probes are independent.
4. **Retries and backoff** (`retry.go`, forwarding integration).
   attempts means total attempts, including the first. Retry configured HTTP
   statuses and transport failures represented as 502/504. Use fixed/exponential
   backoff bounded by one request deadline; close discarded response bodies.
   Automatic retries are limited to GET, HEAD, OPTIONS, TRACE, PUT and DELETE;
   POST/PATCH never receive gateway-managed retries, even with Idempotency-Key.
   Buffer retryable request bodies
   within 1 MiB before sending, and never retry after a response starts. Select
   an eligible target on each attempt. One quota charge and breaker outcome per
   client request, not per attempt.

## Reviewed boundary contracts

- Mapping keys are destination dot paths, values are source dot paths or the
  supplied expressions. Input `{"userId":7,"userName":"Ada"}` becomes
  `{"user":{"id":7,"name":"Ada"},"meta":{...}}`. Mapping replaces the
  original object. Missing source paths become null; arrays are values, not indexed
  paths. Reject conflicting destinations. Timestamps are UTC RFC3339Nano.
- Empty request bodies remain empty (including GET); response HEAD/204/304 bodies
  are not transformed. JSON parsing accepts exactly one value with preserved
  numbers; request mapping requires an object. Unsupported content encodings
  fail explicitly. Bound both input and output to 1 MiB.
- Header rules cannot modify transport/framing or gateway-derived identity fields.
  Rewritten bodies clear stale representation metadata. Invalid client JSON is
  400, unsupported client media/encoding 415, oversized input/output 413; invalid
  or oversized upstream JSON is 502. Ordinary untransformed routes still stream.
- Retry buffering above 1 MiB returns 413 before any attempt. Retry-disabled and
  unsafe-method requests retain streaming. attempts bounds application-level
  RoundTrip calls; Go's transport may internally retry requests it considers replayable; this is not an exactly-once delivery guarantee.
- Breaker success is a completed non-5xx response; truncated upstream bodies are
  failures. Local preparation/no-target errors and client cancellation are neutral.
  Every permit is completed, including neutral half-open release; failure storage
  is bounded by threshold. Late outcomes cannot mutate a newer breaker generation.
- Health probes are origin-relative, have timeout min(route timeout, interval, 5s),
  never overlap for one target, and close without unbounded draining. Constructors
  start no goroutines; run owns cancel/join after binding. Log state transitions.
- Final tests combine all features, plus half-open cancellation, one quota/breaker
  outcome across retries, identical transformed replay bytes, and shared deadlines.

Plan review: APPROVE after incorporating five-lens corrections. Confidence high;
General mode, substantial five-lens tier. Realist 8/10, Failure 8/10, Bloat 9/10,
Maintainer 8/10, End User 7/10 before corrections. The cheapest risk check is a
combined HTTP regression test verifying replay and one logical breaker outcome.

## Work ownership

Workers use separate worktrees. One owns transformations and its integration.
A second owns health checks and balancing. A third owns the isolated breaker
state machine. The integration owner reviews each diff, resolves shared-file
changes, owns retries and combined-feature tests, and updates public docs/demo.
Land each feature separately; no concurrent edits to the integration checkout.

## Gates before each feature lands

- Focused unit and real HTTP tests for changed behavior, errors, boundaries,
  cancellation and fallback; preserve all existing regression tests.
- Full `go test -race -cover ./...`, `go vet ./...`, and diff self-review.
- Keep README/DECISIONS truthful at each commit. Run the real demo after integration.
- Commit with `MINOR:` for each feature and push reviewed commits in order.

## Completion

- [x] Transformations
- [x] Active health checks
- [x] Circuit breakers
- [x] Retries/backoff
- [x] Combined-feature tests, demo, final docs and sequential push

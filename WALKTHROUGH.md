# Presenting GatewayKit

## The 30-minute walkthrough

| Time | What to show | Main point |
|---|---|---|
| 0–3 min | Scope and feature table in README | A complete baseline plus tested stateful features; stretch features are explicitly omitted. |
| 3–10 min | Live demo | Requests really travel through the gateway, and failures have predictable results. |
| 10–20 min | Code in request order | Configuration becomes immutable routing data; isolated components own concurrent state. |
| 20–25 min | Focused tests | Show correctness at boundaries and under concurrent requests. |
| 25–30 min | Decisions and questions | Explain trade-offs, limitations, and what you would implement next. |

Opening explanation, in your own words:

> GatewayKit reads a YAML file, selects a route for each request, applies authentication and rate limits, chooses a backend, and forwards the request. I prioritized the core HTTP contract and failure behavior, then added rate limiting and weighted balancing because they let me demonstrate safe concurrent state. I left retries, transformations, health checks, and circuit breakers out rather than implement their edge cases incompletely.

## Demo: automated proof

From the repository root:

```sh
./scripts/demo.sh
```

This starts the real binaries, checks every expected status, checks weighted distribution, and stops its own processes. Run this once before the interview to catch port conflicts. It requires ports 8081 and 3001–3006 to be free.

## Demo: present each behavior yourself

Terminal 1:

```sh
go run ./cmd/mock
```

Terminal 2:

```sh
go run . -config examples/demo.yaml
```

Terminal 3 runs the requests below. Start a fresh gateway before the rate-limit section so prior demo requests do not consume its quota.

### 1. Health

```sh
curl -i http://localhost:8081/health
```

Expected: 200, `status: healthy`, integer uptime. Explain that this reports process liveness and bypasses upstreams and policies.

### 2. Forwarding, prefix stripping, and body preservation

```sh
curl -i 'http://localhost:8081/echo/hello?name=Ada' \
  -H 'Content-Type: application/json' \
  -d '{"message":"hello"}'
```

Expected: 200. The backend reports `/hello`, query `name=Ada`, method POST, and the original body. `/echo` was removed by configuration. Point out that the demo has entirely different routes from the supplied example.

### 3. Routing errors

```sh
curl -i http://localhost:8081/missing
curl -i -X POST http://localhost:8081/products
```

Expected: 404, then 405 with `Allow: GET`. A path match and a method match are separate decisions.

### 4. Authentication

```sh
curl -i http://localhost:8081/private
curl -i http://localhost:8081/private -H 'X-API-Key: demo-key'
```

Expected: 401, then 200 from backend 3002. Authentication happens before rate limiting and forwarding.

### 5. Rate limiting

```sh
for i in 1 2 3 4; do
  curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8081/limited
done
curl -i http://localhost:8081/limited
```

Expected: 200, 200, 200, 429; the final response includes `Retry-After`. Wait ten seconds from the first accepted request and try again: it returns 200. The automated tests use a controlled clock, so their boundary assertions do not sleep.

### 6. Weighted balancing

```sh
for i in 1 2 3 4 5 6 7 8; do
  curl -s -D - -o /dev/null http://localhost:8081/products/123 | grep -i x-mock-upstream
done
```

Expected: six requests to 3003 and two to 3004. The exact sequence is smooth rather than three requests to one backend followed by one to the other. The stripped upstream path is `/123`.

### 7. Timeout and upstream error handling

```sh
curl -i 'http://localhost:8081/timeout/slow?delay=2s'
curl -i 'http://localhost:8081/echo/error?status=503'
```

Expected: a 504 after roughly 100 milliseconds, then an unchanged upstream 503. No application-level retry is attempted.

To show an unreachable backend, stop the mock process in terminal 1, then request `/echo/hello`: expect 502. `/health` still returns 200. Restart the mocks if continuing.

### 8. The supplied configuration

Stop the demo gateway, then run:

```sh
go run . -config gateway.yaml
curl -i http://localhost:8080/api/users
curl -i http://localhost:8080/api/internal -H 'X-API-Key: sk_live_abc123'
```

Expected: startup warnings identify each omitted feature; the gateway still starts on 8080 and forwards these requests. The key is the example value from the requirements, not a real credential. Be explicit that `/api/legacy` is not transformed and that circuit breakers/active health checking/retries are not active.

## Code tour, in execution order

### `main.go`: lifecycle

`main` creates a signal-cancelled context. `run` resolves a config path, loads and validates it, creates a reusable HTTP transport, opens the listener, and serves the gateway. On SIGINT/SIGTERM it waits for active requests to finish, up to five seconds, before force-closing.

Why one transport? It reuses upstream connections. Creating a transport for every request would waste connections and prevent pooling. Why `RoundTrip`? It performs one HTTP exchange and does not automatically follow redirects like an HTTP client would.

### `config.go`: parse once, validate once

Typed structs reflect the YAML schema. Supported values are validated before listening: methods, route conflicts, URLs, positive durations/limits, auth fields, and weights. Defaults are filled once. The runtime does not repeatedly parse durations or URLs.

The deferred fields are stored as YAML nodes so they can be recognized and warned about without pretending to implement their schema or behavior. Unknown top-level and supported-struct fields fail parsing, which catches typos.

### `gateway.go`: request flow and HTTP semantics

`newGateway` builds route objects and sorts them by descending prefix length. Each route receives its effective limiter and backend selector.

`ServeHTTP` handles health first, then finds a route, verifies method and key, checks quota, chooses an upstream, and calls `forward`. Every failure returns immediately, so rejected requests cannot reach a backend.

`match` uses the decoded path, requires a segment boundary, and prioritizes the most specific path. Ambiguous separators and dot segments are rejected before policy selection, preventing encoded paths from selecting weaker authentication rules. `upstreamURL` joins the configured backend base path to the incoming suffix while preserving encoded characters and the raw query string.

`forward` creates a deadline-bound request clone, clears the server-only `RequestURI`, sets the backend URL/host, cleans headers, and executes it. It copies status and response headers and streams the body. It closes the response body and cancels the context on every exit. Separate downstream socket deadlines bound stalled uploads and clients that stop reading; an upstream context alone cannot stop a blocked downstream operation.

Hop-by-hop headers apply only to one connection. Copying them through a gateway would incorrectly apply downstream connection instructions to the upstream connection. The `Connection` header can name additional hop-by-hop fields, so deleting only a fixed header list is insufficient.

Headers cannot be changed after a response begins. If a streamed body fails after that point, the gateway terminates the response rather than pretending it succeeded.

### `ratelimit.go`: concurrent quota ownership

A route's limiter owns a mutex and a map from identity to bucket. Checking and incrementing happen inside the same critical section; otherwise concurrent requests could all observe spare quota and exceed the limit.

For fixed windows, each bucket stores a start time and accepted-request count. For sliding windows, it stores accepted timestamps and removes timestamps at or before `now - window`. A full sliding bucket tells the client to retry when its oldest accepted request expires. Evaluation time is clamped monotonically under the lock because callers can capture a timestamp and then acquire the mutex out of order.

IP identity comes from the TCP peer. `per: global` replaces the identity with one constant key. Expired buckets are swept lazily, and the identity cap prevents unlimited map growth. The implementation uses in-process state, so it deliberately does not enforce a shared quota across multiple replicas.

### `balancer.go`: smooth weighted selection

Each backend has a configured weight and a current score. For each selection:

1. Add every backend's weight to its current score.
2. Select the highest score.
3. Subtract the sum of all weights from the selected backend.

Equal weights produce round robin. Weights 3:1 produce a 3:1 distribution without constructing a list containing repeated backend entries. The lock protects scores only; the HTTP request happens after releasing it.

### Tests and mock command

`gateway_test.go` sends real HTTP requests to `httptest` upstreams and verifies the proxy contract. `ratelimit_test.go` uses explicit times for boundary tests and launches 50 concurrent requests to prove exactly ten are accepted under a ten-request limit. `balancer_test.go` checks distribution concurrently. `main_test.go` verifies startup configuration, occupied-port errors, and graceful draining.

`cmd/mock` is a demonstration tool, not a runtime dependency. It echoes request details, supports delayed responses and chosen statuses, and binds only to loopback.

## Questions you should be ready to answer

**Why Go?** The standard library has the HTTP primitives, concurrency support, and self-contained server testing needed for this scope. Only YAML parsing required a dependency.

**Why no proxy helper?** The exercise explicitly asks us to build forwarding logic. The code uses a transport for the underlying HTTP exchange, not an existing reverse-proxy implementation.

**What does global rate limit mean?** It supplies each route's default policy. Within a route, `per: global` means one bucket for all clients. This interpretation follows the configuration comment and is documented rather than implicit.

**Why stream bodies?** Buffering arbitrary bodies makes memory proportional to payload size. Streaming keeps ordinary proxying inexpensive, at the cost of not being able to rewrite a response after its headers are sent.

**Why defer retries?** A network failure does not prove a POST failed to execute. Retrying can duplicate side effects. Safe retries need body replay limits, request idempotency rules, and a shared timeout budget.

**Why accept unsupported settings?** The provided YAML contains all stretch features, so rejecting them would prevent the mandatory baseline from starting. Explicit warnings and a feature table disclose the reduced behavior. A production product should offer strict capability validation.

**What happens with multiple gateway instances?** Each has independent counters and balancing state. Shared quotas would require a centralized store and atomic operations, plus a policy for store outages.

**What changes behind a load balancer?** Socket IP becomes the load balancer's IP. A trusted-proxy policy is needed before using forwarded client identity; trusting arbitrary headers would allow limit evasion.

**What would you build next?** Bounded transformations with explicit invalid-input behavior, then idempotency-aware retries, then recovery mechanisms. Before Internet-facing use, prove resource limits and observability under load.

**How was AI used?** AI helped draft the plan, code, tests, and review. The output was checked against the source configuration and real runtime behavior. Explain the actual code and tests rather than claiming it was manually authored.

## Before sending

- Run `go test -race ./...`, `go vet ./...`, and `./scripts/demo.sh`.
- Read `REVIEW.md` for final findings and remaining limitations.
- Confirm `git status --short` is empty and recent commits tell the build story.
- Confirm the ZIP contains `.git/` and can run from a fresh extracted directory.
- Reply all to Jared's original email from `abaldioceda@gmail.com`, attaching the ZIP, before 4 p.m. Eastern. The original recipient list belongs to that email; do not invent or replace it.

## Go syntax you will see in this code

- `type ... struct` groups related state. A method such as `(g *Gateway) ServeHTTP` operates on that object's shared instance.
- `defer` registers cleanup for every function exit: unlocking, canceling contexts, and closing bodies.
- `go func()` starts a concurrent task. Channels and `select` coordinate server completion, cancellation, and shutdown.
- `sync.Mutex` makes checking and updating shared state one indivisible operation. It is released before network I/O.
- `context.Context` carries cancellation and deadlines through the upstream request. Socket deadlines separately bound downstream reads and writes.
- `http.RoundTripper` is the standard-library interface used for the actual HTTP exchange. This keeps forwarding testable without creating a project-specific transport abstraction.
- Go returns errors as values. Startup errors stop the process; request errors map to explicit HTTP responses or abort an already-started stream.

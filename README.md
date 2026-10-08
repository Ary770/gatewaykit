# GatewayKit

A configuration-driven HTTP API gateway built with Go's HTTP server and transport. Forwarding is implemented directly; no reverse-proxy framework or `httputil.ReverseProxy` is used.

## Run

Requires Go 1.26.9 or later. The module pins this patched baseline; Go installations with automatic toolchain selection enabled will download it if needed. The only external runtime dependency is `gopkg.in/yaml.v3`, pinned in `go.mod`/`go.sum`. The first build needs access to the Go module proxy unless the dependency is cached.

```sh
go run ./cmd/mock                    # terminal 1: local upstreams on 3001–3006
go run . -config gateway.yaml        # terminal 2: gateway on 8080
curl -i http://localhost:8080/api/users
```

Configuration path alternatives:

```sh
go run . gateway.yaml
GATEWAY_CONFIG=gateway.yaml go run .
```

An explicit path takes precedence over the environment variable. Supplying both a flag and a positional path is an error. Malformed configuration, unknown supported-schema fields, invalid values, or conflicting route/method pairs stop startup with a useful error. Ports and route values come from the configuration. `gateway.yaml` reproduces the supplied assessment example. All configured features are implemented; unknown fields and invalid feature settings stop startup.

For a binary:

```sh
go build -o bin/gatewaykit .
./bin/gatewaykit -config gateway.yaml
```

The gateway listens on all interfaces. The demo upstreams bind only to loopback. SIGINT/SIGTERM stops accepting new requests and drains active requests for up to five seconds.

## Test

```sh
go test ./...
```

Tests start their own local upstreams on ephemeral ports. No database, credentials, manually started services, or Internet calls are needed after dependencies are downloaded.

Additional checks:

```sh
go test -race -cover ./...
go vet ./...
```

## Demo

```sh
./scripts/demo.sh
```

This builds and starts the real application and mock upstreams, asserts response codes and weighted distribution, and cleans up its processes. Requires Bash, curl, and free local ports 8080 and 3001–3006. It uses `examples/demo.yaml`, which changes routes and values and contains only implemented features. See [WALKTHROUGH.md](WALKTHROUGH.md) for the live presentation and code explanation.

## Feature coverage

| Configuration / behavior | Status |
|---|---|
| YAML path through CLI or environment; configured listen port | Implemented |
| `GET /health`, integer uptime, independent of policies/backends | Implemented |
| Prefix routing, methods, 404/405 with `Allow` | Implemented |
| `strip_prefix`, upstream base paths and query strings | Implemented |
| Request/response body, status, multi-value header forwarding | Implemented |
| Global timeout and `upstream.timeout` override | Implemented |
| Global rate-limit defaults and route overrides | Implemented |
| Fixed-window and exact sliding-window limits | Implemented |
| Per-IP and global buckets; concurrent accounting; `Retry-After` | Implemented |
| API-key authentication | Implemented |
| Round robin and smooth weighted round robin | Implemented |
| Retry and backoff | Implemented for idempotent methods (follow-up) |
| Request/response header and body transformations | Implemented (follow-up) |
| Active upstream health checks | Implemented (follow-up) |
| Circuit breaker | Implemented (follow-up) |

The original core submission is preserved at tag `core-submission-2026-10-08`. The four additional features were implemented as follow-up work, with explicit safety boundaries below.

## Defined behavior

- **Routing:** longest matching path prefix at a segment boundary. `/api/users` matches itself and `/api/users/123`, not `/api/users-extra`. Routes are matched on decoded paths, so encoding ordinary characters cannot bypass a more specific policy. Encoded slashes, backslashes, repeated slashes, and dot segments are rejected with `400` before routing to avoid disagreement with backend normalization. A trailing slash in a configured prefix is normalized. The most specific path owns method filtering; an unsupported method does not fall back to a less specific route. Routes may share a path if their methods do not overlap. HEAD/OPTIONS must be listed explicitly.
- **Prefix stripping:** removes the matched prefix; an empty result becomes `/`. An upstream URL's base path is retained. Unambiguous encoded suffixes and query strings are preserved. Target query parameters precede incoming parameters without decoding/re-encoding.
- **Health:** `GET /health` bypasses routing, authentication, rate limits, and upstream access. It reports process liveness, not backend readiness. Other methods follow ordinary routing.
- **Authentication:** missing, incorrect, or multiple API-key header values return `401`; comparisons use constant-time comparison. Keys are loaded from local configuration and are never logged. The sample keys are illustrative. Valid keys are forwarded to the upstream like other end-to-end headers.
- **Limits:** a gateway limit is a default policy for each route, not an additional gateway-wide quota. Route overrides replace it. `per: global` shares a bucket among all clients of that route; `per: ip` uses the TCP peer IP. Client-provided forwarding headers cannot choose a bucket. Authentication failures and rate-limit rejections do not consume quota. Requests accepted by the limiter consume one quota slot even if later rejected by a circuit breaker or body policy, or if the upstream fails; retries do not consume additional slots.
- **Windows:** fixed windows begin with a bucket's first accepted request. Sliding windows count accepted requests in `(now - window, now]`. Evaluation time is kept nondecreasing inside the lock so delayed concurrent callers cannot reorder sliding timestamps. Denials return `429` with a rounded-up `Retry-After` in seconds. Each route has at most 10,000 identity buckets; expired entries are swept lazily, and new identities receive `503` with `Retry-After` when capacity is exhausted. Existing buckets remain usable.
- **Circuit breaker:** configured per route. Counts completed HTTP 5xx, transport failures, and truncated upstream bodies within `(now - window, now]`. At threshold, requests receive `503` with `error: service_unavailable`, integer `retry_after`, and `Retry-After`. After cooldown one recovery request is allowed; a completed non-5xx response closes the circuit and a failure restarts cooldown. Client cancellation and downstream write failures are neutral and release a recovery probe. Authentication and rate limits run first; breaker rejections consume accepted quota. State resets on restart, and transition logs identify the route.
- **Balancing:** equal weights produce round robin; weighted selection uses a smooth algorithm without allocating an expanded weight list. Selection is synchronized, but network requests run outside the lock. Each attempt selects one currently eligible target.
- **Forwarding:** redirects are returned to the client, not followed. Hop-by-hop headers, including headers named by `Connection`, are removed in both directions. Forwarding identity headers are replaced with values derived from the connection. The backend receives its own host in `Host`. Transparent decompression is disabled.
- **Failures:** an unreachable upstream returns `502`; a transport timeout before response headers returns `504`. Downstream read deadlines also bound incomplete uploads; downstream writes have the route deadline plus a one-second grace period for sending timeout errors. Once a streamed response has begun, a read error/timeout aborts the downstream response rather than returning a misleading completed body. A client disconnect cancels the upstream request.

## Boundaries

State is in-memory and local to one process; restart resets counters and balancing. There is no config reload, distributed quota, TLS listener, trusted-proxy list, admin API, metrics export, HTTP upgrade/WebSocket tunnel, or trailer forwarding. Deploying behind another proxy groups clients by that proxy's socket IP. This submission is a take-home implementation, not a hardened Internet-facing gateway.

See [DECISIONS.md](DECISIONS.md) for prioritization, architectural trade-offs, and next steps.

### Follow-up transformations

`request_transform.headers` and `response_transform.headers` support `add` and
`remove`; framing and gateway-derived identity headers are protected. Header-only
rules retain streaming. Request body `mapping` replaces the original object:
keys are destination dot paths, values are source dot paths or `$literal:...`,
`$request_time`, and `$route_path`. Missing sources become null; arrays are copied
as values. Response `envelope` recursively resolves `$body`, `$response_time`,
`$request_time`, and `$route_path`. Times use UTC RFC3339Nano.

Body transforms require JSON media types and identity encoding, preserve numbers,
and bound both input and output to 1 MiB. Malformed client JSON returns 400,
unsupported media/encoding 415, and oversized bodies 413. Invalid or oversized
upstream bodies return 502; deadline expiry returns 504 before response headers.
Empty requests remain empty; HEAD, 204 and 304 responses retain bodyless semantics.
Rewritten bodies receive JSON content type and discard stale representation metadata,
even when header rules specify those metadata fields.

Active health checks use GET at the target origin plus the configured path, without following redirects. HTTP 2xx/3xx is healthy. Targets start eligible; `unhealthy_threshold` consecutive failures exclude them (default 1), and one successful check restores them. When all targets are excluded the route returns 503. Checks run immediately, then wait the configured interval after each result; timeout is the smallest of the route timeout, interval, and five seconds. Shutdown cancels and joins probes. `/health` remains gateway liveness only.

### Follow-up retries

`retry.attempts` is the total number of gateway attempts, including the first
(1–100). `on` lists HTTP error statuses to retry; transport errors are classified
as 502 or 504 and retried only when listed. Backoff is fixed or exponential from
`initial_delay`, with overflow protection. Buffering, every attempt, backoff and
response reading share the route deadline. Intermediate responses are closed;
only the final response is transformed and returned.

Gateway-managed retries apply to GET, HEAD, OPTIONS, TRACE, PUT and DELETE.
POST/PATCH and custom methods receive one gateway attempt, regardless of
Idempotency-Key. Retry-enabled uploads above 1 MiB return 413 before forwarding;
other routes retain streaming unless a body transform is configured. Implement
PUT/DELETE with their HTTP idempotency semantics in the backend. Attempts count
calls to Go's transport, not an exactly-once delivery guarantee: the transport can
internally recover connections for requests it considers replayable, including
bodyless requests carrying an idempotency key. Body transforms do not provide a
replay function to the transport for unsafe methods.

Run `./scripts/demo-features.sh` to exercise all four follow-up features on
ports 18080, 13001 and 13002 without disturbing the core demo on port 8080.

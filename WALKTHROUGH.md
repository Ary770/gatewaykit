# GatewayKit walkthrough

Use this as speaking notes. Start with the overview, show the demo, then follow one request through the code. The commands and expected responses are in [DEMO.md](DEMO.md).

## How I'd introduce it

> I built a small API gateway that reads its routes and settings from YAML. It checks the request, chooses a backend, and forwards it.
>
> I started with routing and basic proxying. Then I added timeouts, API-key checks, rate limits, and weighted load balancing. I focused on making those work correctly, including when requests arrive together or a backend fails.
>
> I preserved that core baseline, then added bounded transformations, active health checks, circuit breakers, and safe retries in separate reviewed commits. The README explains their safety boundaries.

## What I'd show in 30 minutes

| Time | What to cover |
|---|---|
| First 3 minutes | What it does and what I prioritized |
| Next 7 minutes | The live demo |
| Next 10 minutes | Follow a request through the code |
| Last 10 minutes | Tests, review fixes, trade-offs, and questions |

## Start with the demo

```sh
./scripts/demo.sh
```

> This starts the gateway and mock backends, sends requests, checks the results, and stops the processes when it's done. The demo uses different routes and settings from the supplied config.

The main things to point out:

- The backend receives the request body and query string. Prefix stripping changes `/echo/hello` to `/hello`.
- An unknown route returns `404`. A method that isn't allowed returns `405`.
- A missing API key returns `401`. The configured key lets the request through.
- The limited route accepts three requests and rejects the fourth with `429` and `Retry-After`.
- With weights of 3 and 1, eight requests split six to one backend and two to the other.
- A slow backend returns `504`. An unreachable backend returns `502`.
- `/health` tells us the gateway is running. It still returns `200` when backends are down.

Use [DEMO.md](DEMO.md) to run these individually. Start a fresh gateway before demonstrating the rate limit so earlier requests don't affect the count.

## Follow one request through the code

### 1. Start the app: [main.go](main.go)

> This is the startup and shutdown code. It gets the config path, validates the file, creates the HTTP transport, and starts listening.
>
> I reuse one transport so requests can reuse backend connections. When the app stops, it gives active requests up to five seconds to finish.

A transport sends the HTTP request. Our code still decides where to send it and how to forward the response.

### 2. Read the settings: [config.go](config.go)

> I validate the config before accepting traffic. Bad URLs, invalid durations, conflicting routes, or missing auth settings fail at startup with an error.
>
> Defaults are resolved once. Each request uses the settings we've already loaded.

Point out that the timeout override is under `upstream.timeout`. All feature settings are typed and validated at startup.

### 3. Handle the request: [gateway.go](gateway.go)

Start at `ServeHTTP`.

> This shows the order of the checks. Health comes first. For other requests, I find the route, check the method and API key, check the rate limit, prepare transformations and replay, choose an eligible backend, check the circuit, and forward the request.
>
> If a check fails, we return there. The request never reaches the backend.

Then show `match` and `forward`.

`forward` reads in three stages: clone and sanitize the outgoing request, prepare transformations and retry replay, then run the backend exchange. The shared deadline surrounds all three stages. `forwardPreparedRequest` keeps circuit-breaker accounting around the full exchange, including response streaming, so a broken backend response and a disconnected client are classified differently.

> The most specific matching route wins. `/api/users` matches `/api/users/123`, but it doesn't match `/api/users-extra`.
>
> The forwarding code builds the backend URL, copies the request, and streams the response back. Ordinary routes stream. JSON transformations and retryable uploads buffer at most 1 MiB.

Two details worth explaining if asked:

- Some headers only apply to one connection, such as `Connection` and `Keep-Alive`. We remove those before forwarding. We also remove any extra headers named by `Connection`.
- The backend request and the client socket use a shared deadline. That covers a slow backend, an unfinished upload, and a client that stops reading. Writes get one extra second to allow a timeout response. If a response has already started, a later failure closes it because we can't change the status anymore.

### 4. Count requests safely: [ratelimit.go](ratelimit.go)

> Each route owns its request counts. A lock keeps the quota check and count update together, so simultaneous requests can't all claim the last available slot. We release the lock before contacting the backend.
>
> Fixed windows count requests from the start of a window. Sliding windows keep the accepted request times and remove the ones that are too old.

Start at `allow`: it keeps the whole decision under one lock, makes the evaluation time monotonic, cleans expired buckets, finds the client bucket, and calls the selected window algorithm. `allowFixedWindow` and `allowSlidingWindow` each own their quota rule. The returned `rateLimitDecision` names the outcome and retry delay; `allowRateLimitedRequest` in the gateway translates it into an HTTP response.

`per: ip` uses the client's connection address. `per: global` shares one count across all clients of that route. The gateway-level policy supplies the default for each route; a route override replaces it.

> I don't trust a client-supplied IP header. Otherwise someone could change the header on every request to avoid the limit.

### 5. Choose a backend: [balancer.go](balancer.go)

> Equal weights take turns. Different weights change the share of requests each backend receives.
>
> The algorithm adds each backend's weight to its score, picks the highest score, and subtracts the total weight from the winner. That's how it keeps the configured ratio without creating a list of repeated backend entries.

The lock only protects those scores. Requests to different backends can still run at the same time.

### 6. Follow-up features

- `transform.go` maps JSON and applies the final response envelope, with input/output bounds and protected headers.
- `health.go` starts probes after the server binds, excludes unhealthy targets, restores recovered targets, and joins workers on shutdown.
- `circuitbreaker.go` keeps route state under a lock. One probe tests recovery; late results cannot overwrite newer state.
- `retry.go` prepares replay bytes once, closes discarded responses, and keeps attempts/backoff inside one deadline.

Run `./scripts/demo-features.sh` for these features. The combined tests use all four on the same route and prove that retries count once for quota and breaker state.

## Explain the testing and review

> The tests start their own HTTP servers, so they don't depend on manually running the mocks. They check paths, headers, bodies, errors, authentication, limits, and balancing.
>
> One concurrency test sends 50 requests against a limit of ten and checks that exactly ten get through. The limiter boundary tests use controlled times, so they don't need to wait for a window to expire.

The review found issues worth talking about:

| Issue | What I changed |
|---|---|
| An encoded path could select a less protected route | Match the decoded path and reject ambiguous path forms before choosing a policy |
| Concurrent requests could record timestamps out of order | Keep the limiter's effective time from moving backward while holding the lock |
| A stalled upload could continue past the backend timeout | Add client-socket deadlines as well as the backend deadline |
| Two timeout signals could race and produce `502` instead of `504` | Use one shared deadline to classify the failure consistently |

> I added regression tests for those cases. The final timeout check passed 100 repetitions locally and another 100 in the independent review. I also tested the extracted submission ZIP.

The full results are in [REVIEW.md](REVIEW.md).

## Decisions I'd be ready to explain

**Choosing Go**

> Go's standard library covered the HTTP server, client transport, and testing tools I needed. YAML parsing is the only runtime dependency.

**Keeping the design small**

> Each file has a clear job. The handler coordinates the request. The limiter owns its counts, and the balancer owns its selection state. I used the existing HTTP transport interface rather than adding a custom framework.

**Keeping retries safe**

> A failed connection doesn't tell us whether a POST already changed something. Gateway retries only apply to idempotent methods, within one shared deadline. POST/PATCH receive one gateway attempt. The standard transport's own recovery behavior is documented separately.

**Finishing the configuration carefully**

> The core baseline warned about deferred features. The follow-up implements them with explicit rules for body limits, failed backends, circuit recovery and safe replay. Invalid settings now fail startup.

**Running multiple gateway instances**

> Counts are local to one process. Shared limits would need shared storage and a decision about what happens if that storage is unavailable.

**Running behind another proxy**

> We'd see the proxy's connection address. We'd need a trusted-proxy policy before using a forwarded client IP.

**What I'd do next**

> Before production use, I'd add traffic metrics, a trusted-proxy policy and load tests to establish the operating limits.

**Using AI**

> I used AI to help with the plan, implementation, tests, and review. I checked the result against the supplied config and exercised the running app. The review found bugs, and the regression tests helped verify the fixes.

## Quick Go reference

- `struct` groups related data. `*Gateway` is a pointer to the shared gateway instance.
- `defer` runs cleanup when the function returns, including on error paths.
- `go func()` starts concurrent work. Channels and `select` coordinate completion and cancellation.
- `sync.Mutex` protects shared state while it is checked and updated.
- `context.Context` carries cancellation and deadlines into the backend request.
- `http.RoundTripper` is the standard interface that performs an HTTP exchange.

You don't need to explain every syntax detail up front. Start with what the request does, then use the code to show how it works.

## Architecture in one minute

- `main.go` starts and stops the server and health workers.
- `config.go` turns YAML into validated settings.
- `gateway.go` follows one request from route checks to the backend and back.
- `ratelimit.go`, `balancer.go`, and `circuitbreaker.go` own their separate state and locks.
- `retry.go` owns attempts and backoff under the original deadline.
- `transform.go` owns header and bounded JSON changes.
- `health.go` starts one worker per configured backend. Each worker probes, updates health after the configured failure threshold, and waits until its next check. Shutdown cancels and joins every worker.
- `demo/run.py` builds and starts the browser demo; `demo/web/index.html` sends real requests through the gateway.

There is no plugin framework or middleware registry to explain. The gateway calls these functions directly, and each policy stays in its own file.

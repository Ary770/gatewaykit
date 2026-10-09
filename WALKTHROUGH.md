# GatewayKit project review walkthrough

Use these speaking notes to explain the Go app you submitted. Follow one request, `GET /api/users/123`, from its YAML settings to the backend and back. Show one function at a time and explain what it does, why it is here, and how it is tested.

For shorter file-by-file speaking notes, use [PRESENTATION_NOTES.md](/Users/arybaldioceda/dev/gatewaykit/PRESENTATION_NOTES.md).

Aim for about 20 minutes, leaving time for questions.

**Presentation order:** settings → startup → request checks → backend request → response → additional features → tests → live examples.

## 1 Explain the purpose

**Time: 1 minute.** Show the [README](/Users/arybaldioceda/dev/gatewaykit/README.md), briefly.

> GatewayKit sits between clients and backend services. It receives a request, checks the rules for that route, sends the request to the backend, and returns the backend's response. YAML controls those rules.

> I started with startup, the health endpoint, and basic forwarding. Once those worked, I added the additional features and tested how they interact.

## 2 Explain the settings and one route

**Time: 2 minutes.** Open [gateway.yaml](/Users/arybaldioceda/dev/gatewaykit/gateway.yaml:3).

| Setting | Simple meaning |
|---|---|
| `port: 8080` | Accept requests on port 8080. |
| `global_timeout: "30s"` | Use a default request time limit of 30 seconds. |
| `requests: 100`, `window: "60s"` | Default to 100 accepted requests during a 60-second period. |
| `strategy: "fixed_window"` | Count requests during a fixed period, then reset. Our period starts with the first accepted request. |
| `per: "ip"` | Keep a separate counter for each client IP address. |

> Each route has its own counters. The global rate limit supplies a default for routes without their own limit; it isn't one shared counter across all routes.

Then show [`/api/users`](/Users/arybaldioceda/dev/gatewaykit/gateway.yaml:12):

- `path` and `methods` select which requests this route accepts.
- `upstream.url` names the backend, on port 3001.
- `strip_prefix: false` keeps the full request path.
- This route overrides the default limit with 30 requests per IP in a sliding 60-second window. A sliding window counts requests in the most recent 60 seconds.

> For GET /api/users/123, the gateway selects this route and forwards the request to the service on port 3001. Changing the backend address doesn't require changing gateway code.

Supporting pointers: [configuration structures](/Users/arybaldioceda/dev/gatewaykit/config.go:17) turn YAML into Go fields; [`newGateway`](/Users/arybaldioceda/dev/gatewaykit/gateway.go:77) prepares each route's settings and counters.

**Transition:** Now I'll show how those settings become a running server.

## 3 Show startup

**Time: 2 minutes.** Start at [`run`](/Users/arybaldioceda/dev/gatewaykit/main.go:31).

| Step | Code pointer | Main point |
|---|---|---|
| Find the configuration file | [`resolveConfigPath`](/Users/arybaldioceda/dev/gatewaykit/main.go:57) | Select the command-line path or `GATEWAY_CONFIG`. |
| Read and validate settings | [`loadConfig`](/Users/arybaldioceda/dev/gatewaykit/config.go:97), then [`validate`](/Users/arybaldioceda/dev/gatewaykit/config.go:125) | Read YAML, fill in defaults, and reject invalid settings before accepting requests. |
| Prepare the HTTP sender and gateway | [`newUpstreamTransport`](/Users/arybaldioceda/dev/gatewaykit/main.go:80), then [`newGateway`](/Users/arybaldioceda/dev/gatewaykit/gateway.go:77) | Prepare backend communication and the routes. |
| Start the server | [`serveGateway`](/Users/arybaldioceda/dev/gatewaykit/main.go:92) | Open the configured port and accept requests. |

> The transport sends HTTP requests to backends. We reuse it so requests can reuse existing network connections.

Point to `Handler: gateway` in `serveGateway`:

> This connects the HTTP server to our request-handling code. For every incoming request, Go calls ServeHTTP.

Keep the focus on `run`. Open the helpers when explaining a particular step or answering a question.

## 4 Follow the request checks

**Time: 3 minutes.** Open [`ServeHTTP`](/Users/arybaldioceda/dev/gatewaykit/gateway.go:40).

> This is the main request flow. Each check either allows the request to continue or returns an error.

Follow the code in order:

1. Answer the gateway's `/health` request.
2. Reject paths the gateway and backend could interpret differently.
3. Find the matching route and check its HTTP method.
4. Apply the route's time limit.
5. Check the API key, if required.
6. Check the request limit.
7. Forward the request.

> If one of these checks fails, the backend never receives the request.

| Supporting function | What to explain |
|---|---|
| [`match`](/Users/arybaldioceda/dev/gatewaykit/gateway.go:132) | Check the longest matching route. `/api/users` matches `/api/users/123`, but not `/api/users-extra`. |
| [`authorized`](/Users/arybaldioceda/dev/gatewaykit/gateway.go:367) | Check the configured API key. |
| [`allowRateLimitedRequest`](/Users/arybaldioceda/dev/gatewaykit/gateway.go:111) | Allow the request or return a limit error. |
| [`rateLimiter.allow`](/Users/arybaldioceda/dev/gatewaykit/ratelimit.go:49) | Check and update the count together under a lock. |

Then open [`forward`](/Users/arybaldioceda/dev/gatewaykit/gateway.go:157):

> This prepares a separate backend request. It keeps the original request unchanged and uses one time limit for preparation, sending, and response handling.

A **deadline** is the time by which the work must finish. A **context** carries that deadline and the signal to stop work if the request is canceled.

## 5 Explain the backend request and response

**Time: 3 minutes.** Open [`forwardPreparedRequest`](/Users/arybaldioceda/dev/gatewaykit/gateway.go:229).

| Step | Explanation |
|---|---|
| Choose a backend | Select a backend currently marked healthy. Return 503 if none is available. |
| Track upload errors | Detect failed client uploads so they do not count as backend failures. |
| Check the circuit breaker | Block new backend requests after repeated backend failures. |
| Send the request | `roundTripAttempts` sends the request and handles configured retries. |
| Handle the response | Pass sending errors or backend responses to the functions in `response.go`. |

Point to the deferred call to `permit.finish`:

> Defer means this runs when the function finishes. We record the circuit-breaker result after response handling, because receiving headers doesn't prove that the backend finished sending its body.

Then open [response.go](/Users/arybaldioceda/dev/gatewaykit/response.go):

| Function | One job |
|---|---|
| [`writeUpstreamError`](/Users/arybaldioceda/dev/gatewaykit/response.go:16) | Return an error when sending fails: 503 for no healthy backend, 504 for a timeout, or 502 otherwise. |
| [`relayUpstreamResponse`](/Users/arybaldioceda/dev/gatewaykit/response.go:35) | Apply response changes and determine the backend result. |
| [`copyUpstreamResponse`](/Users/arybaldioceda/dev/gatewaykit/response.go:72) | Send the final headers, status, and body to the client. |

> The main function coordinates the work. The helpers handle individual details. A broken backend response can count against the breaker; a failed client upload or client write should not.

**Important edge case:** once response headers have been sent, the gateway cannot replace the status with an error. If the body fails partway through, it closes the connection so the response is not treated as complete.

## 6 Explain the additional features

**Time: 2 minutes.** Search for `FEATURE ADD-ON:` to find the calls in the main flow.

| Feature | Simple explanation | Call site and implementation |
|---|---|---|
| Retries and backoff | Retry allowed failures, with a delay and the original time limit. | [Prepare retries](/Users/arybaldioceda/dev/gatewaykit/gateway.go:210); [send with retries](/Users/arybaldioceda/dev/gatewaykit/gateway.go:257); [`roundTripAttempts`](/Users/arybaldioceda/dev/gatewaykit/retry.go:142). |
| Request transformations | Change configured headers or JSON fields before forwarding. | [Call site](/Users/arybaldioceda/dev/gatewaykit/gateway.go:201); [`transformRequest`](/Users/arybaldioceda/dev/gatewaykit/transform.go:368). |
| Response transformations | Change headers or wrap the backend's JSON response. | [Call site](/Users/arybaldioceda/dev/gatewaykit/response.go:44); [`transformResponse`](/Users/arybaldioceda/dev/gatewaykit/transform.go:410). |
| Active health checks | Periodically check backends and stop selecting ones that repeatedly fail. | [Start checks](/Users/arybaldioceda/dev/gatewaykit/main.go:105); [`monitorBackend`](/Users/arybaldioceda/dev/gatewaykit/health.go:62). |
| Circuit breaker | Block requests after repeated backend failures, then allow a trial request after waiting. | [Call site](/Users/arybaldioceda/dev/gatewaykit/gateway.go:241); [`admit`](/Users/arybaldioceda/dev/gatewaykit/circuitbreaker.go:62); [`finish`](/Users/arybaldioceda/dev/gatewaykit/circuitbreaker.go:85). |

> Health checks make separate background requests. The circuit breaker learns from failures while handling client requests.

The users route follows the core path. Other routes enable the additional features. Open one feature implementation in detail if asked.

## 7 Show tests and explain AI use

**Time: 3 minutes.** Open [`TestProxyPreservesRequestAndResponse`](/Users/arybaldioceda/dev/gatewaykit/gateway_test.go:32).

> This test starts a local HTTP backend and checks what actually crosses the gateway: the request going in and the response coming back.

Show the setup and assertions. Keep these tests ready for questions:

- [`TestRoutingAndStripping`](/Users/arybaldioceda/dev/gatewaykit/gateway_test.go:78): route selection and prefix removal.
- [`TestUpstreamFailureAndTimeout`](/Users/arybaldioceda/dev/gatewaykit/gateway_test.go:133): backend failures and timeouts.
- [`TestAuthenticationAndRateLimitPipeline`](/Users/arybaldioceda/dev/gatewaykit/gateway_test.go:197): authentication and request limits.
- [`TestDefaultRateLimitAndRouteOverride`](/Users/arybaldioceda/dev/gatewaykit/gateway_test.go:237): default limits and route overrides.
- [Response regression tests](/Users/arybaldioceda/dev/gatewaykit/response_test.go): backend failures, interrupted bodies, client write errors, and response cleanup.

Explain AI use:

> I used Codex for planning, implementation, and tests. I set the priorities and reviewed the resulting changes. Additional features were developed in isolated worktrees and integrated with shared checks. Review findings became regression tests.

Mention specific model names only if you can verify them.

## 8 Run the app

**Time: 3 minutes.** Run these commands from `/Users/arybaldioceda/dev/gatewaykit`. Stop existing processes using ports 8080 and 3001–3006 before starting another copy.

First terminal:

```bash
go run ./cmd/mock
```

Second terminal:

```bash
go run . -config gateway.yaml
```

Use a third terminal for the requests.

### Check the gateway

```bash
curl -i http://localhost:8080/health
```

Expected: **200**.

> This checks the gateway itself. It doesn't guarantee that every backend is healthy.

### Follow the users request

```bash
curl -i 'http://localhost:8080/api/users/123?active=true'
```

Expected: **200**, backend port **3001**, path `/api/users/123`, and query `active=true`.

> The demo backend echoes what it received. Its responses are demo data, but the gateway communicates with it through real HTTP requests.

Code pointer: [`mockHandler`](/Users/arybaldioceda/dev/gatewaykit/cmd/mock/main.go:77).

### Show a rejected method

```bash
curl -i -X DELETE http://localhost:8080/api/users/123
```

Expected: **405**. Code pointer: [`writeRouteRejection`](/Users/arybaldioceda/dev/gatewaykit/gateway.go:100).

### Show authentication

```bash
curl -i http://localhost:8080/api/internal
```

Expected: **401**.

```bash
curl -i http://localhost:8080/api/internal \
  -H 'X-API-Key: sk_live_abc123'
```

Expected: **200**, using the configuration's example key.

### Optional transformation example

```bash
curl -i http://localhost:8080/api/legacy/profile \
  -H 'Content-Type: application/json' \
  -H 'X-Debug: presentation' \
  -d '{"userId":123,"userName":"Ary"}'
```

Point out the backend path `/profile`, changed JSON fields, removed `X-Debug` header, added headers, and response wrapper.

### Optional retry example

```bash
curl -s -o /dev/null \
  -w 'Status: %{http_code}, elapsed: %{time_total}s\n' \
  'http://localhost:8080/api/orders?status=503'
```

Expected: **503 after roughly three seconds of retry delays**, plus processing time. The backend intentionally keeps failing.

### Optional request limit example

Run this last:

```bash
for i in {1..31}; do
  curl -s -o /dev/null -w '%{http_code}\n' \
    http://localhost:8080/api/users
done
```

Expected: **200 responses followed by 429 responses**. Earlier users requests also count toward the allowance. After enough time has passed, old requests stop counting.

### Browser demo

You can finish with the browser UI. Stop the manually started gateway and mocks first, then run:

```bash
python3 demo/run.py
```

Open [the browser demo](http://127.0.0.1:8080/demo/index.html). The launcher builds the application and starts the demo services. It requires Go and Python 3.

> This uses a separate demo configuration, with routes chosen to make each behavior easy to exercise.

See [DEMO.md](/Users/arybaldioceda/dev/gatewaykit/DEMO.md) for the browser and terminal scenarios.

## Close with one tradeoff

> The components are separated so each part can be understood and tested independently. Request counters and circuit-breaker state currently live in memory. Multiple gateway instances would need a coordinated approach if limits must apply across the whole deployment.

For each function, answer: **What does it do? Why is it here? How do we know it works?** Show one function at a time, then return to the main request flow.

## Reference notes for questions

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
- `gateway.go` checks the route and coordinates the request to the backend.
- `response.go` handles backend errors, response changes, and sending the response to the client.
- `ratelimit.go`, `balancer.go`, and `circuitbreaker.go` own their separate state and locks.
- `retry.go` owns attempts and backoff under the original deadline.
- `transform.go` owns header and bounded JSON changes.
- `health.go` starts one worker per configured backend. Each worker probes, updates health after the configured failure threshold, and waits until its next check. Shutdown cancels and joins every worker.
- `demo/run.py` builds and starts the browser demo; `demo/web/index.html` sends real requests through the gateway.

There is no plugin framework or middleware registry to explain. The gateway calls these functions directly, and each policy stays in its own file.

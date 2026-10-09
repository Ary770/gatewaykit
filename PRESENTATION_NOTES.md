# GatewayKit file by file presentation notes

Follow this order. Spend about 15 minutes on the code, then run the demo. Use `GET /api/users/123` as the example throughout.

**One story:** settings → start server → check request → send to backend → return response → test → run.

## 1 README

**Open:** [README.md](/Users/arybaldioceda/dev/gatewaykit/README.md). Show the introduction and feature coverage briefly.

**Say:**

> GatewayKit is a small API gateway controlled by YAML. It sits between clients and backend services, checks the request, forwards it, and returns the response.
>
> I started with the core requirements, then added the four additional features and tested how they interact.

**Next:** “I'll start with the settings for one request.”

## 2 Gateway YAML

**Open:** [gateway.yaml](/Users/arybaldioceda/dev/gatewaykit/gateway.yaml:3). Show the gateway settings, then `/api/users`.

**Point out:**

- Port 8080 accepts requests.
- The default request time limit is 30 seconds.
- The default limit is 100 requests per IP in a fixed 60-second window.
- Each route has its own counters; these defaults can be overridden.
- `/api/users` allows GET and POST, forwards to port 3001, and keeps the full path.
- Its own limit is 30 requests per IP during the most recent 60 seconds.

**Say:**

> GET /api/users/123 goes to the backend on port 3001 with the same path. We can change that destination in YAML without changing the gateway code.

**Next:** “Now let's turn those settings into a running server.”

## 3 Main Go

**Open:** [`run` in main.go](/Users/arybaldioceda/dev/gatewaykit/main.go:31).

**Follow four calls:**

1. `resolveConfigPath`: find the settings file.
2. `loadConfig`: read YAML and validate the settings.
3. `newUpstreamTransport` and `newGateway`: prepare the HTTP sender and routes.
4. `serveGateway`: open the port and accept requests.

**Say:**

> We validate settings before accepting traffic. The transport sends outgoing HTTP requests and reuses backend connections. The HTTP server receives incoming requests.

Point to `Handler: gateway` in [`serveGateway`](/Users/arybaldioceda/dev/gatewaykit/main.go:92):

> This tells Go to call the gateway's ServeHTTP function for each request.

If asked about validation, open [`loadConfig` in config.go](/Users/arybaldioceda/dev/gatewaykit/config.go:97).

**Next:** “The server is running. Here's what happens when a request arrives.”

## 4 Gateway Go

**First show:** [`ServeHTTP`](/Users/arybaldioceda/dev/gatewaykit/gateway.go:40).

**Say:**

> This is the main request flow: health, path checks, route and method matching, API-key checking, request limits, and forwarding. A failed check returns an error before contacting the backend.

**Then show:** [`forward`](/Users/arybaldioceda/dev/gatewaykit/gateway.go:157).

> This makes a separate backend request, applies configured changes, and prepares retries. All the work uses the same time limit.

**Then show:** [`forwardPreparedRequest`](/Users/arybaldioceda/dev/gatewaykit/gateway.go:229).

Follow its main steps:

1. Choose a healthy backend.
2. Track client upload errors.
3. Ask the circuit breaker whether sending is allowed.
4. Send through `roundTripAttempts`.
5. Pass the response to `response.go`.
6. Record the circuit-breaker result when the function finishes.

**Explain the important decision:**

> We wait until response handling finishes before recording the backend result. Receiving headers alone doesn't prove that the response body is complete. Client upload errors and failed client writes shouldn't count as backend failures.

`defer` means “run this when the function finishes.”

**Next:** “Let's look at where the real HTTP request is sent.”

## 5 Retry Go

**Open:** [`roundTripAttempts` in retry.go](/Users/arybaldioceda/dev/gatewaykit/retry.go:142). Find `g.transport.RoundTrip` in this file to show the actual HTTP send.

**Say:**

> This sends the request to the backend. If retries are configured and the failure is allowed, it waits and tries again. All attempts share the original time limit, and only the final response is returned.
>
> We limit retries to HTTP methods intended to have the same effect when repeated. POST and PATCH get one gateway attempt. Retries don't count as extra client requests for the request limit or circuit breaker.

**Next:** “Once we have the final backend response, we send it back to the client.”

## 6 Response Go

**Open:** [response.go](/Users/arybaldioceda/dev/gatewaykit/response.go).

**Show these three functions:**

- [`writeUpstreamError`](/Users/arybaldioceda/dev/gatewaykit/response.go:16): return 503 for no healthy backend, 504 for a timeout, or 502 for another sending failure.
- [`relayUpstreamResponse`](/Users/arybaldioceda/dev/gatewaykit/response.go:35): apply response changes and determine the backend result.
- [`copyUpstreamResponse`](/Users/arybaldioceda/dev/gatewaykit/response.go:72): send the final headers, status, and body.

**Say:**

> These helpers give each part a clear job. The gateway coordinates the request; this file handles the response.

If asked about incomplete responses:

> Once headers are sent, we can't replace the status. If the body fails partway through, we close the connection so the response isn't treated as complete.

**Next:** “Here's how we verify that this works.”

## 7 Gateway tests

**Open:** [`TestProxyPreservesRequestAndResponse`](/Users/arybaldioceda/dev/gatewaykit/gateway_test.go:32).

**Show:** the test backend setup and the final assertions.

**Say:**

> This starts a local HTTP backend and checks the request and response through the gateway. Other tests cover routing, timeouts, authentication, request limits, balancing, and the additional features.

Keep [response_test.go](/Users/arybaldioceda/dev/gatewaykit/response_test.go) ready for questions about backend failures and client write errors.

To run the tests:

```bash
go test -race ./...
```

`-race` checks for unsafe concurrent access to shared data.

**AI explanation:**

> I used Codex for planning, implementation, tests, and review. I set the priorities and reviewed the changes. Additional features used isolated worktrees, and review findings became regression tests.

Only name specific models if you can verify which ones were used.

**Next:** “Now I'll run the request we've been following.”

## Demo after the code

From the repository root, stop any existing gateway and demo services using these ports before starting another copy.

**Terminal 1: backends**

```bash
go run ./cmd/mock
```

**Terminal 2: gateway**

```bash
go run . -config gateway.yaml
```

**Terminal 3: requests**

Check the gateway:

```bash
curl -i http://localhost:8080/health
```

Expected: **200**. “This checks the gateway process itself.”

Follow our example:

```bash
curl -i 'http://localhost:8080/api/users/123?active=true'
```

Expected: **200**, backend port **3001**, the full path, and the query string.

> The backend returns demo data, but the gateway makes a real HTTP request to a separate service. The echo response lets us inspect what the backend received.

Show an early rejection:

```bash
curl -i -X DELETE http://localhost:8080/api/users/123
```

Expected: **405**. “The route doesn't allow DELETE, so the gateway rejects it.”

If time allows, show authentication:

```bash
curl -i http://localhost:8080/api/internal
curl -i http://localhost:8080/api/internal -H 'X-API-Key: sk_live_abc123'
```

Expected: **401**, then **200** using the configuration's example key.

## Additional features if asked

Search for `FEATURE ADD-ON:` to find the calls in the main flow.

| Feature | One sentence | Implementation file |
|---|---|---|
| Retries and backoff | Try allowed failures again, waiting between attempts. | [retry.go](/Users/arybaldioceda/dev/gatewaykit/retry.go) |
| Transformations | Change configured headers or JSON fields. | [transform.go](/Users/arybaldioceda/dev/gatewaykit/transform.go) |
| Active health checks | Check backends in the background and exclude ones that repeatedly fail. | [health.go](/Users/arybaldioceda/dev/gatewaykit/health.go) |
| Circuit breaker | Block backend requests after repeated failures, then allow a trial request after waiting. | [circuitbreaker.go](/Users/arybaldioceda/dev/gatewaykit/circuitbreaker.go) |

> Health checks make separate background requests. The circuit breaker learns from client-request results.

For a transformation example:

```bash
curl -i http://localhost:8080/api/legacy/profile \
  -H 'Content-Type: application/json' \
  -H 'X-Debug: presentation' \
  -d '{"userId":123,"userName":"Ary"}'
```

Point out the changed request fields, removed `X-Debug` header, added headers, and wrapped response.

For the browser UI, stop the manually started gateway and backends first, then run `python3 demo/run.py` and open [the demo](http://127.0.0.1:8080/demo/index.html). It uses a separate demo configuration.

## Close

> State is local to one process. Shared request limits across multiple gateway instances would need shared storage. Before production use, I'd also add metrics, load tests, and a trusted-proxy policy.

For more detail and additional curl examples, use [WALKTHROUGH.md](/Users/arybaldioceda/dev/gatewaykit/WALKTHROUGH.md).

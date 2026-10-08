# GatewayKit demo commands

Use [WALKTHROUGH.md](WALKTHROUGH.md) for what to explain while showing these requests.

Run these commands from the repository root.

## Run all demo checks

From the repository root:

```sh
./scripts/demo.sh
```

This starts the real binaries, checks every expected status, checks weighted distribution, and stops its own processes. Run this once before the interview to catch port conflicts. It requires ports 8080 and 3001–3006 to be free.

## Run the demo one step at a time

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
curl -i http://localhost:8080/health
```

Expected: 200, `status: healthy`, and uptime in whole seconds. This tells us the gateway is running, even if a backend is down.

### 2. Forwarding, prefix stripping, and body preservation

```sh
curl -i 'http://localhost:8080/echo/hello?name=Ada' \
  -H 'Content-Type: application/json' \
  -d '{"message":"hello"}'
```

Expected: 200. The backend reports `/hello`, query `name=Ada`, method POST, and the original body. `/echo` was removed by configuration. Point out that the demo has entirely different routes from the supplied example.

### 3. Routing errors

```sh
curl -i http://localhost:8080/missing
curl -i -X POST http://localhost:8080/products
```

Expected: 404, then 405 with `Allow: GET`. A path match and a method match are separate decisions.

### 4. Authentication

```sh
curl -i http://localhost:8080/private
curl -i http://localhost:8080/private -H 'X-API-Key: demo-key'
```

Expected: 401, then 200 from backend 3002. Authentication happens before rate limiting and forwarding.

### 5. Rate limiting

```sh
for i in 1 2 3 4; do
  curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/limited
done
curl -i http://localhost:8080/limited
```

Expected: 200, 200, 200, 429; the final response includes `Retry-After`. Wait ten seconds from the first accepted request and try again: it returns 200. The automated tests use a controlled clock, so their boundary assertions do not sleep.

### 6. Weighted balancing

```sh
for i in 1 2 3 4 5 6 7 8; do
  curl -s -D - -o /dev/null http://localhost:8080/products/123 | grep -i x-mock-upstream
done
```

Expected: six requests to 3003 and two to 3004. The algorithm spreads requests across the backends while keeping the 3:1 ratio. The stripped upstream path is `/123`.

### 7. Timeout and upstream error handling

```sh
curl -i 'http://localhost:8080/timeout/slow?delay=2s'
curl -i 'http://localhost:8080/echo/error?status=503'
```

Expected: a 504 after roughly 100 milliseconds, then an unchanged upstream 503. These core-demo routes do not configure retries, so each makes one gateway attempt.

To show an unreachable backend, stop the mock process in terminal 1, then request `/echo/hello`: expect 502. `/health` still returns 200. Restart the mocks if continuing.

### 8. The supplied configuration

Stop the demo gateway, then run:

```sh
go run . -config gateway.yaml
curl -i http://localhost:8080/api/users
curl -i http://localhost:8080/api/internal -H 'X-API-Key: sk_live_abc123'
```

Expected: the supplied configuration starts on 8080 without deferred-feature warnings. The key is an illustrative value from the requirements, not a real credential. `/api/legacy` now applies transformations, `/api/products` runs health checks, `/api/orders` uses safe retries, and `/api/internal` has a circuit breaker. See the README for precise boundaries.

## Follow-up features: transformations, retries, health checks, circuit breaker

To exercise the implemented additional features, run:

```sh
./scripts/demo-features.sh
```

This uses `examples/features.yaml`, a gateway on **18080**, and two separate mock
processes on **13001** and **13002**. These ports must be free; the core 8080
demo can remain running. The script builds the binaries and stops only its own
processes when finished. It requires Go, Bash, curl, and ordinary shell utilities.

The assertions show:

1. A POST body with `userId` and `userName` becomes a nested `user` object, removes
   a request header, adds a gateway header, and wraps the response in an envelope.
2. A GET starts at a backend configured to return 503, retries once, and succeeds
   at the second backend. The final response identifies that backend.
3. The script stops its second mock and waits for a health-check transition;
   subsequent requests all reach the remaining healthy backend.
4. Two upstream 503 responses open a circuit. The third request receives the
   gateway's `service_unavailable` JSON, `retry_after`, and `Retry-After` header.

Each feature has its own route so the demonstrated cause and effect is clear.
This is a live smoke demo; unit and integration tests cover additional boundaries.

## Interactive browser demo

Requires Go and Python 3 (standard library only). From the repository root:

```sh
python3 demo/run.py
```

Open http://127.0.0.1:8080/demo/index.html. The launcher builds the current code, starts the gateway, mock backends, and static page server, and stops them when you press Ctrl+C. Ports 8080, 3099, and 3001–3006 must be free. Stop any other demo before starting this one.

For a repeatable functional check that starts and stops its own services:

```sh
python3 demo/run.py --check
```

`demo/web/index.html` contains the browser controls and `demo/web/features.js` contains the four additional scenarios; `demo/gateway.yaml` defines their routes. The page itself is served through GatewayKit: `/demo` forwards to the static server on 3099. Button clicks call the gateway on 8080, which forwards API requests to the mock backends. The Python launcher and test client are development tools, not gateway runtime dependencies.

The browser page also includes four additional-feature controls:

- Transform request and response: shows the original JSON, the mapped JSON received by the backend, header changes, and the response envelope.
- Retry recovery: compares a 503 from backend 3005 with a successful request retried on backend 3006 after 100ms.
- Active health checks: the route includes a permanently offline backend and a healthy backend; eight requests all reach the healthy one after probes exclude the offline target.
- Circuit breaker cycle: two failures open the circuit, a request is rejected without an upstream response, and a successful probe restores traffic after the three-second cooldown. The button takes about seven seconds so repeated demonstrations can start from a recovered circuit.

The automated browser-demo check now covers all 24 core and feature scenarios. Backend shutdown/recovery is additionally covered by the Go health-check tests; the browser health scenario demonstrates exclusion of an already-offline backend.

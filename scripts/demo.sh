#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
work=$(mktemp -d)
gateway_pid=''
mock_pid=''
cleanup() {
  result=$?
  if [ "$result" != 0 ]; then
    printf '\nDemo failed; process logs:\n' >&2
    cat "$work/mock.log" "$work/gateway.log" 2>/dev/null >&2 || true
  fi
  if [ -n "$gateway_pid" ]; then kill "$gateway_pid" 2>/dev/null || true; wait "$gateway_pid" 2>/dev/null || true; fi
  if [ -n "$mock_pid" ]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

go build -o "$work/gatewaykit" .
go build -o "$work/mock" ./cmd/mock
"$work/mock" >"$work/mock.log" 2>&1 &
mock_pid=$!
"$work/gatewaykit" -config examples/demo.yaml >"$work/gateway.log" 2>&1 &
gateway_pid=$!

ready=0
for attempt in $(seq 1 50); do
  if ! kill -0 "$mock_pid" 2>/dev/null || ! kill -0 "$gateway_pid" 2>/dev/null; then
    cat "$work/mock.log" "$work/gateway.log"
    exit 1
  fi
  if curl -fsS --max-time 1 http://127.0.0.1:3001/healthz >/dev/null 2>&1 && curl -fsS --max-time 1 http://127.0.0.1:8080/health >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.1
done
if [ "$ready" != 1 ]; then cat "$work/mock.log" "$work/gateway.log"; exit 1; fi

check() {
  label=$1
  expected=$2
  path=$3
  shift 3
  printf '\n%s\n' "$label"
  status=$(curl -sS --max-time 5 -D "$work/headers" -o "$work/response" -w '%{http_code}' "http://127.0.0.1:8080$path" "$@")
  printf 'HTTP %s\n' "$status"
  cat "$work/response"
  printf '\n'
  if [ "$status" != "$expected" ]; then printf 'Expected HTTP %s\n' "$expected" >&2; exit 1; fi
}

check '1. Health is independent of backends' 200 /health
check '2. Prefix stripping, query and JSON body forwarding' 200 '/echo/hello?name=Ada' -H 'Content-Type: application/json' -d '{"message":"hello"}'
check '3. Unknown route' 404 /missing
check '4. Method filtering' 405 /products -X POST
check '5. Authentication rejects missing keys' 401 /private
check '6. Authentication accepts the configured key' 200 /private -H 'X-API-Key: demo-key'
for attempt in 1 2 3; do check "7.$attempt Rate limit accepts request $attempt" 200 /limited; done
check '7.4 Rate limit rejects request four' 429 /limited
if ! tr -d '\r' <"$work/headers" | grep -qi '^Retry-After: '; then printf 'Missing Retry-After\n' >&2; exit 1; fi

printf '\n8. Weighted balancing: eight requests must produce a 6:2 distribution\n'
a=0
b=0
for attempt in $(seq 1 8); do
  status=$(curl -sS --max-time 5 -D "$work/headers" -o "$work/response" -w '%{http_code}' http://127.0.0.1:8080/products/123)
  if [ "$status" != 200 ]; then cat "$work/response"; exit 1; fi
  if grep -q '127.0.0.1:3003' "$work/headers"; then a=$((a+1)); elif grep -q '127.0.0.1:3004' "$work/headers"; then b=$((b+1)); else cat "$work/headers"; exit 1; fi
done
printf 'Backend 3003: %s; backend 3004: %s\n' "$a" "$b"
if [ "$a" != 6 ] || [ "$b" != 2 ]; then printf 'Unexpected backend distribution\n' >&2; exit 1; fi

check '9. Slow upstream becomes a gateway timeout' 504 '/timeout/slow?delay=2s'
check '10. Upstream errors are preserved without hidden retries' 503 '/echo/error?status=503'
printf '\nAll demo checks passed. Processes will now stop.\n'

#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
work=$(mktemp -d)
gateway_pid=''
mock_a_pid=''
mock_b_pid=''
cleanup() {
  result=$?
  if [ "$result" != 0 ]; then
    printf '\nFeature demo failed; process logs:\n' >&2
    cat "$work"/*.log 2>/dev/null >&2 || true
  fi
  for pid in "$gateway_pid" "$mock_a_pid" "$mock_b_pid"; do
    if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  done
  rm -rf "$work"
}
trap cleanup EXIT

go build -o "$work/gatewaykit" .
go build -o "$work/mock" ./cmd/mock
"$work/mock" -ports 13001 >"$work/mock-a.log" 2>&1 &
mock_a_pid=$!
"$work/mock" -ports 13002 >"$work/mock-b.log" 2>&1 &
mock_b_pid=$!
"$work/gatewaykit" -config examples/features.yaml >"$work/gateway.log" 2>&1 &
gateway_pid=$!
ready=0
for attempt in $(seq 1 50); do
  for pid in "$gateway_pid" "$mock_a_pid" "$mock_b_pid"; do kill -0 "$pid" 2>/dev/null || exit 1; done
  if curl -fsS --max-time 1 http://127.0.0.1:13001/healthz >/dev/null 2>&1 &&
     curl -fsS --max-time 1 http://127.0.0.1:13002/healthz >/dev/null 2>&1 &&
     curl -fsS --max-time 1 http://127.0.0.1:18080/health >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.1
done
[ "$ready" = 1 ] || exit 1

check() {
  label=$1; expected=$2; path=$3; shift 3
  printf '\n%s\n' "$label"
  status=$(curl -sS --max-time 5 -D "$work/headers" -o "$work/body" -w '%{http_code}' "http://127.0.0.1:18080$path" "$@")
  printf 'HTTP %s\n' "$status"
  cat "$work/body"
  printf '\n'
  [ "$status" = "$expected" ] || { printf 'Expected HTTP %s\n' "$expected" >&2; exit 1; }
}
contains() { grep -Fq -- "$1" "$work/body" || { printf 'Missing body value: %s\n' "$1" >&2; exit 1; }; }

check '1. POST JSON mapping and response envelope' 200 /transform/echo \
  -H 'Content-Type: application/json' -H 'X-Debug: remove-me' -d '{"userId":7,"userName":"Ada"}'
contains '"body":"{\"user\":{\"id\":7,\"name\":\"Ada\"}}"'
contains '"data":{'
contains '"route":"/transform"'
contains '"X-Gateway":["gatewaykit"]'
if grep -Fq 'remove-me' "$work/body"; then printf 'Request header was not removed\n' >&2; exit 1; fi
tr -d '\r' <"$work/headers" | grep -qi '^X-Served-By: gatewaykit$'

# A fresh route starts at target A (503); the successful final response must be B.
check '2. Retry a 503 on the next backend' 200 /retry/echo
contains '"upstream":"127.0.0.1:13002"'

# Stop only our second mock; wait for the health transition before checking routing.
health_log_start=$(( $(wc -l <"$work/gateway.log") + 1 ))
kill "$mock_b_pid"
wait "$mock_b_pid" || true
mock_b_pid=''
excluded=0
for attempt in $(seq 1 50); do
  if tail -n "+$health_log_start" "$work/gateway.log" | grep 'upstream health changed' | grep 'target=127.0.0.1:13002' | grep -q 'healthy=false'; then excluded=1; break; fi
  sleep 0.1
done
[ "$excluded" = 1 ] || { printf 'Backend was not marked unhealthy\n' >&2; exit 1; }
for attempt in 1 2 3 4; do
  check "3.$attempt Health checks exclude the stopped backend" 200 /healthy/echo
  contains '"upstream":"127.0.0.1:13001"'
done

check '4.1 First upstream failure' 503 /breaker/echo
contains '"upstream":"127.0.0.1:13001"'
check '4.2 Second upstream failure opens the circuit' 503 /breaker/echo
contains '"upstream":"127.0.0.1:13001"'
check '4.3 Open circuit rejects without forwarding' 503 /breaker/echo
contains '"error":"service_unavailable"'
contains '"retry_after":'
if grep -Fq '"upstream"' "$work/body"; then printf 'Open circuit reached upstream\n' >&2; exit 1; fi
tr -d '\r' <"$work/headers" | grep -Eqi '^Retry-After: [1-9][0-9]*$'
printf '\nAll follow-up feature demo checks passed. Processes will now stop.\n'

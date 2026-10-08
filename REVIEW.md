# Final code and application review

## THE VERDICT: SHIP

Confidence: high · Mode: General · Bar: two-hour take-home submission

**The call in one line:** Submit the approved feature set; the independent follow-up reviews found no remaining defect that warrants delaying submission.

**Why:** The gateway implements and tests the baseline plus authentication, both rate-limit strategies, timeouts, and weighted balancing. The first review found concrete policy, concurrency, and socket-lifecycle defects; those were corrected with regression tests. A second independent code review and a separate application review both passed the corrected implementation.

**Scorecard:** Initial, pre-correction review: Architect 8/10 · Data 8/10 · Security 7/10 · Craft 8.5/10 · Performance 8/10 · Operability 8/10. The final follow-up uses a pass/fail release decision, not invented revised scores.

**Blockers:** None outstanding for the documented take-home scope. This is not a production-readiness certification.

**The single highest-leverage fix:** Authentication policy now follows the decoded path, with ambiguous path forms rejected before routing. Encoded ordinary characters can no longer select a less specific, unprotected route while reaching a protected backend path.

**Quick wins completed:** categorized upstream failure logs without bodies/keys/query strings; bounded failing-test observations; demo logs printed on failed assertions; connection framing reset between downstream and upstream; patched Go toolchain pinned.

**Deep work before production:** distributed quota policy, trusted-proxy identity, traffic metrics, load-tested operating limits, and complete contracts for the deliberately deferred features.

**What was not verified:** distributed deployment, sustained load/resource ceilings, real external upstreams, TLS termination, and the omitted features. No claim is made about those behaviors. Dependency scanning is point-in-time evidence, not a guarantee against future advisories.

## Review target and independence

The first panel reviewed source at `d0ece7f` through six lenses: architecture, state/data modeling, security, code quality, performance, and operability. Review agents were read-only; the implementing agent owned all fixes and commits.

The second full code reviewer and the second application reviewer inspected `a49fa17` after the corrections. They were separate from the implementing agent and independently ran checks. Subsequent commits add documentation only.

Covered source: `main.go`, `config.go`, `gateway.go`, `ratelimit.go`, `balancer.go`, `cmd/mock`, the corresponding tests, the example configurations, and the demo script. README, DECISIONS, and WALKTHROUGH were checked against application behavior.

## Findings and resolutions

| Finding | Resolution | Regression evidence |
|---|---|---|
| Encoded path could select a public route while backend interpreted a protected path | Match decoded paths; reject encoded slashes, backslashes, repeated slashes, and dot segments; preserve unambiguous escaped suffixes | `TestEncodedPathsCannotBypassAuthentication`, `TestEncodedUnicodePrefix` |
| Concurrent callers could append sliding-window timestamps out of order and expire live quota | Clamp effective evaluation time monotonically inside the limiter lock | `TestSlidingWindowReorderedConcurrentTimestamps` |
| Canceling an outbound context did not unblock a stalled inbound upload | Apply downstream read deadlines and a bounded write grace independently of the upstream context | `TestStalledUploadIsBounded`, `TestSlowDownstreamReaderIsBounded` |
| Proxy contract test could hang when forwarding failed before reaching its mock | Bounded channel observation with response diagnostics | `TestProxyPreservesRequestAndResponse` |
| Downstream connection state could leak into upstream framing/pooling | Clear `Close`, trailers, and incoming transfer-encoding metadata before the transport creates its own framing | `TestProxyPreservesRequestAndResponse` |
| Upstream failures lacked target/category diagnostics; demo failures discarded logs | Sanitized route/target/category logging; print child-process logs on demo failure | Code review and live demo |
| Installed Go 1.26.3 had standard-library advisories reported by the scanner | Pin Go 1.26.9 in the project; system-wide Go installation is unchanged | Fresh vulnerability scan: `No vulnerabilities found.` |

## Verification results

- `go test -race -cover ./...`: PASS. Gateway statement coverage **93.1%**; mock command **87.5%**.
- `go vet ./...`: PASS.
- `go mod verify`: PASS; all modules verified.
- `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`: PASS on Go 1.26.9, **No vulnerabilities found.** The scanner is an external verification tool, not a project/runtime dependency.
- `bash -n scripts/demo.sh`: PASS in the independent second review.
- `./scripts/demo.sh`: PASS in both the implementation pass and independent application review. All ten scenarios passed, including exact 6:2 backend selection.
- Independent fresh-binary stalled-upload probe: `504` in **0.104 seconds** with a 100ms timeout. The pre-fix binary did not respond within one second.
- Independent keep-alive probe: unreachable backend `502`, health `200`, then backend `502` on a reused client across deadline boundaries.
- Independent shutdown probe: exit status 0; the unit suite additionally verifies active-request draining.
- Focused uncached regressions for stalled uploads, slow readers, truncated responses, and encoded Unicode paths: PASS in the application review.

## Accepted scope limitations

Retries, transformations, active health checking, and circuit breakers emit warnings and do not run. Quotas and balancing are process-local. IP identity comes from the socket, not a trusted-proxy policy. There is no listener TLS, upgrade tunnel, trailer forwarding, configuration reload, or metrics endpoint. Route lookup is linear; exact sliding-window memory grows with accepted requests retained within a window, within the configured identity-table cap. Graceful shutdown drains for five seconds, which can be shorter than an active route timeout.

These limitations are called out in the README and demo guide. The omitted features are not represented as complete or partially working.

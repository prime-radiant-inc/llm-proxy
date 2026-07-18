# PRI-2655 residual 2 — mantle producer-side capture stamps

## What changed

`mantle.go`:

- Added `path string` parameter to both `logMantleResponseObservation` and
  `logMantleAbortedResponseObservation`, threaded from the pre-existing local
  `targetPath` (the `/openai/...` path actually sent upstream — same value
  already stamped as `request.upstream_path` in the request record) at all
  six call sites inside `serveMantleForPathWithAttribution`.
- Both builders now stamp four top-level keys on the entry map, unconditionally,
  alongside the existing nested `response.chunks`/`response.body` payload
  (left untouched) and the existing `termination` stamping (left untouched):
  - `path` — the threaded `targetPath`.
  - `upstream` — via a new small helper `(p *Proxy) mantleUpstream() string`
    (extracted from the identical inline logic that `writeMantleObservation`
    already had, to avoid duplicating the nil-bedrock-guard three times).
  - `metering_provider` — `meteringProviderFromUpstream(upstream)` (capture.go,
    unmodified).
  - `capture_version` — `CaptureVersion` (1).
- `writeMantleObservation` now calls the new `p.mantleUpstream()` helper
  instead of inlining the same region-guard logic; behavior is unchanged
  (verified byte-identical via the full existing test suite).
- No changes to `capture.go`, the Loki label allowlist, or any other builder
  (`logMantleRequestObservation`, `logMantlePreUpstreamError` untouched).

## Tests (TDD)

Added two new tests in `mantle_contract_test.go`, following the existing
`newTestBedrockProxy` harness pattern used by
`TestServeMantleWritesTelemetryContractObservationNonStreaming` and
`TestServeMantleLogsUpstreamTransportErrorObservation`:

- `TestServeMantleResponseObservationStampsCaptureFacts` — non-streaming
  success path (`logMantleResponseObservation`).
- `TestServeMantleAbortedResponseObservationStampsCaptureFacts` — dial-error
  abort path (`logMantleAbortedResponseObservation`).

RED confirmed before implementing (both failed with `path = <nil>, want
/openai/v1/responses`, i.e. the stamp key was absent). GREEN after
implementing.

## A discrepancy I found and resolved with judgment, not invention

The task brief's example for `upstream` said "chatgpt.com on the reroute."
I verified against the actual code and that is **not** correct for mantle:
mantle's real network host is always `bedrock-mantle.<region>.api.aws`
(`mantleUpstreamHost`, region validated to `us-east-1`/`us-east-2`/`us-west-2`
by `ValidateBedrockRegion`) — there is no JWT/chatgpt.com reroute anywhere in
`mantle.go`; that reroute only exists in the generic proxy (`proxy.go`).

Consequence: `meteringProviderFromUpstream("bedrock-mantle....api.aws")`
returns `""` — none of its cases (`openrouter`, `bedrock-runtime`,
`anthropic`, `chatgpt.com`, `openai`) match a `bedrock-mantle` host. I did
**not** add a case for it, since that function is a shared, documented,
4-door host→provider map and extending it was outside this task's literal
scope.

I checked whether this leaves mantle money unmeterable (the ticket's stated
problem) and confirmed it does not, by reading the consumer side
(`superpowers-cloud-server` `internal/metering/capture.go`,
`ResolveCaptureContext`): even for a `capture_version > 0` line, when
`MeteringProvider` resolves empty, resolution falls through to the documented
legacy-shim chain, whose second tier is `_meta.provider` — and mantle already
stamps `_meta.provider = "openai"` unconditionally (`newMantleMeta`,
pre-existing). The doc comment there literally says "mantle writes a real
metering provider there." The consumer's own `ProviderFromHost`
(`internal/metering/pricing.go`) has the identical gap for `bedrock-mantle`
hosts, confirming this is a known, intentional shared design, not a bug I
introduced.

So: `metering_provider` is present (unconditionally stamped, per the task's
"carry the four... keys" instruction) but empty for real mantle traffic
today. The two new tests assert its value equals
`meteringProviderFromUpstream(wantUpstream)` computed independently (not a
hardcoded `"openai"`), with a comment explaining why, so the test stays
honest and still catches wiring bugs (e.g. accidentally passing `path`
instead of `upstream` into `meteringProviderFromUpstream` would flip the
result to `"openai"` since `/openai/v1/responses` contains `"openai"` —
the test would catch that swap).

**Flag for Drew**: if you want `metering_provider` itself (not just the
`_meta.provider` fallback) to read `"openai"` for mantle, a follow-up would
add a `bedrock-mantle` case to `meteringProviderFromUpstream` (capture.go)
mirrored on the consumer's `ProviderFromHost` (`internal/metering/pricing.go`
in `superpowers-cloud-server`) — a two-repo, two-function change, deliberately
not done here since it wasn't literally in scope and the consumer fallback
already covers meterability.

## Commands run

```
go build ./...                                                    # clean
go test -run 'TestServeMantleResponseObservationStampsCaptureFacts|TestServeMantleAbortedResponseObservationStampsCaptureFacts' -v .
                                                                    # RED before impl, GREEN after
go test -run 'TestServeMantle|TestMantle' -v .                     # 33 tests PASS, 1 SKIP (fixture writer, gated on env var)
go vet ./...                                                       # clean
gofmt -l .                                                         # only the 4 known pre-existing drifted files
                                                                    # (fingerprint_test.go, loki_exporter.go,
                                                                    # loki_exporter_test.go, parser.go) + their
                                                                    # duplicates inside pre-existing .worktrees/*
go test ./... -count=1                                             # ok (24.985s), no failures
go test -run TestServeMantleNonStreamingPassThroughBeforeDrain -v -count=1 .
                                                                    # isolated re-run: PASS, no flake this run
```

## Files touched

- `/Users/drewritter/prime-rad/llm-proxy/mantle.go`
- `/Users/drewritter/prime-rad/llm-proxy/mantle_contract_test.go`

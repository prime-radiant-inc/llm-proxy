---
runId: 825bce
feature: loki-export
created: 2026-01-23
status: ready
---

# Execution Plan: Loki Export for llm-proxy

## Summary

Add real-time log export to Grafana Loki from llm-proxy, enabling centralized observability for LLM API calls. Implementation follows TDD methodology per @docs/constitutions/current/.

## Phases Overview

| Phase | Type | Tasks | Est. Time |
|-------|------|-------|-----------|
| 1 | Sequential | 1 task (Config) | 3h |
| 2 | Sequential | 1 task (LokiExporter) | 5h |
| 3 | Sequential | 1 task (MultiWriter) | 4h |
| 4 | Sequential | 2 tasks (Integration + Main) | 4h |

**Total Sequential Time:** 16h
**Total Parallel Time:** 16h (all sequential phases)
**Parallelization Savings:** 0h (linear dependency chain)

---

## Phase 1: Configuration Foundation

### Task 1: Add Loki Configuration

**Size:** M (3h)
**Branch:** `825bce/01-loki-config`

**Description:**
Add Loki configuration struct and environment variable loading to config.go. This establishes the configuration foundation all other tasks depend on.

**Files to Modify:**
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/config.go`
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/config_test.go`
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/config.toml.example`

**Implementation Details:**
1. Add `LokiConfig` struct with fields: Enabled, URL, BatchSize, BatchWaitStr, RetryMax, UseGzip, Environment
2. Add `Loki LokiConfig` field to `Config` struct
3. Add defaults in `DefaultConfig()`: Enabled=false, BatchSize=1000, BatchWaitStr="5s", RetryMax=5, UseGzip=true, Environment="development"
4. Add environment variable loading in `LoadConfigFromEnv()` for `LLM_PROXY_LOKI_*` prefix
5. Update config.toml.example with Loki section

**Acceptance Criteria:**
- [ ] LokiConfig struct defined with all fields from FR3
- [ ] DefaultConfig returns sensible defaults
- [ ] Environment variables override TOML config
- [ ] TOML parsing works for `[loki]` section
- [ ] All existing tests pass
- [ ] New tests cover: defaults, env loading, TOML parsing

**Test Cases:**
- `TestDefaultConfig_LokiDefaults` - verify defaults match FR3
- `TestLoadConfigFromEnv_LokiEnabled` - test boolean env var
- `TestLoadConfigFromEnv_LokiURL` - test string env var
- `TestLoadConfigFromEnv_LokiBatchSize` - test int env var
- `TestLoadConfigFromTOML_LokiSection` - test TOML parsing

**Constitution Reference:** @docs/constitutions/current/ for testing patterns

---

## Phase 2: LokiExporter Component

### Task 2: Create LokiExporter with Async Batching

**Size:** L (5h)
**Branch:** `825bce/02-loki-exporter`
**Depends on:** Task 1

**Description:**
Create the core async Loki client (FR1) that batches log entries and pushes to Loki's HTTP API with retry logic and gzip compression.

**Files to Create:**
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/loki_exporter.go`
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/loki_exporter_test.go`

**Implementation Details:**
1. Define `LokiExporterConfig` struct with URL, BatchSize, BatchWait, RetryMax, RetryWait, Environment, BufferSize
2. Define `LokiExporter` struct with buffered channel (10,000 capacity), stats counters (atomic)
3. Implement `NewLokiExporter(cfg)` - validates config, starts background worker
4. Implement `Push(entry, provider)` - non-blocking send to channel, drops if full
5. Implement `run()` - background worker that batches by size (1000) or time (5s)
6. Implement `sendBatch()` - groups by labels, formats Loki push request, retries with exponential backoff
7. Implement `doSend()` - HTTP POST with gzip compression
8. Implement `Close()` - signals shutdown, drains channel, flushes final batch (30s timeout)
9. Implement `Stats()` - returns entries_sent, entries_failed, entries_dropped, batches_sent

**Loki Push Format (FR6):**
```json
{
  "streams": [{
    "stream": {
      "app": "llm-proxy",
      "provider": "anthropic",
      "environment": "production",
      "machine": "drew@macbook",
      "log_type": "request"
    },
    "values": [["1706054400000000000", "{...}"]]
  }]
}
```

**Acceptance Criteria:**
- [ ] Non-blocking Push method (channel send with default case)
- [ ] Batches by size (1000) OR time (5s), whichever comes first
- [ ] Retry with exponential backoff (100ms base, max 10s, 5 retries)
- [ ] Gzip compression enabled by default
- [ ] Graceful shutdown drains channel and flushes (30s timeout)
- [ ] Stats accurately track sent/failed/dropped/batches
- [ ] Labels follow FR6 (low cardinality only)

**Test Cases:**
- `TestNewLokiExporter_RequiresURL` - error if URL empty
- `TestNewLokiExporter_DefaultValues` - validates sensible defaults
- `TestPush_AddsToChannel` - entry queued successfully
- `TestPush_DropsWhenChannelFull` - increments dropped counter
- `TestPush_ExtractsTimestamp` - parses _meta.ts or uses now
- `TestPush_ExtractsLogType` - extracts type field for label
- `TestSendBatch_GroupsByLabels` - groups entries with same labels
- `TestSendBatch_RetriesOnError` - retries on 5xx errors
- `TestSendBatch_ExponentialBackoff` - verifies backoff timing
- `TestClose_FlushesRemaining` - drains channel on close
- `TestClose_TimesOut` - returns error after 30s
- `TestStats_Accurate` - counters match actual behavior

**Constitution Reference:** @docs/constitutions/current/ for concurrency patterns (channel-based async)

---

## Phase 3: MultiWriter Component

### Task 3: Create MultiWriter Fan-out Wrapper

**Size:** M (4h)
**Branch:** `825bce/03-multi-writer`
**Depends on:** Task 2

**Description:**
Create MultiWriter component (FR2) that wraps FileLogger and LokiExporter, providing the same interface as Logger but fanning out to both destinations.

**Files to Create:**
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/multi_writer.go`
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/multi_writer_test.go`

**Files to Modify:**
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/proxy.go` - add ProxyLogger interface

**Implementation Details:**
1. Define `ProxyLogger` interface in proxy.go with methods: RegisterUpstream, LogSessionStart, LogRequest, LogResponse, LogFork
2. Ensure existing `*Logger` satisfies `ProxyLogger` interface
3. Define `MultiWriter` struct with `file *Logger` and `loki *LokiExporter`
4. Implement `NewMultiWriter(file, loki)` constructor
5. Implement all ProxyLogger methods:
   - Call file logger first (primary)
   - If loki != nil, build entry map and call loki.Push()
   - Return file error (Loki errors are logged but don't fail)
6. Implement `Close()` - close Loki first (flushes), then file logger

**Acceptance Criteria:**
- [ ] ProxyLogger interface defined in proxy.go
- [ ] *Logger implements ProxyLogger
- [ ] *MultiWriter implements ProxyLogger
- [ ] File errors returned to caller (primary logging)
- [ ] Loki errors logged but don't fail the operation (graceful degradation FR2)
- [ ] Nil LokiExporter handled gracefully
- [ ] Close() flushes Loki before closing file logger

**Test Cases:**
- `TestMultiWriter_LogSessionStart_BothCalled` - verify both destinations receive entry
- `TestMultiWriter_LogRequest_BothCalled` - verify both destinations receive entry
- `TestMultiWriter_LogResponse_BothCalled` - verify both destinations receive entry
- `TestMultiWriter_LogFork_BothCalled` - verify both destinations receive entry
- `TestMultiWriter_NilLoki_NoError` - works with nil LokiExporter
- `TestMultiWriter_FileError_Returned` - file errors propagate
- `TestMultiWriter_Close_ClosesLokiFirst` - verify close order

**Constitution Reference:** @docs/constitutions/current/ for error handling patterns (graceful degradation)

---

## Phase 4: Integration and Wiring

### Task 4: Integrate Loki into Server

**Size:** M (3h)
**Branch:** `825bce/04-server-integration`
**Depends on:** Task 3

**Description:**
Wire LokiExporter and MultiWriter into the Server, update Proxy to use ProxyLogger interface, and add /health/loki endpoint (FR4).

**Files to Modify:**
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/server.go`
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/server_test.go`
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/proxy.go`
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/streaming.go` (if needed for MultiWriter)

**Implementation Details:**
1. Update `Server` struct to add `fileLogger *Logger`, `lokiExporter *LokiExporter`
2. Update `NewServer()`:
   - Create file logger as before
   - If cfg.Loki.Enabled && cfg.Loki.URL != "", create LokiExporter
   - Log warning if LokiExporter creation fails (continue without Loki)
   - Create MultiWriter wrapping both
   - Update Proxy constructor to accept ProxyLogger
3. Add `/health/loki` endpoint handler returning JSON:
   ```json
   {
     "status": "ok|disabled|error",
     "entries_sent": 1234,
     "entries_dropped": 0,
     "last_error": null,
     "last_error_time": null
   }
   ```
4. Update `Server.Close()` to close MultiWriter (which closes Loki then file)
5. Update Proxy to use ProxyLogger interface instead of *Logger

**Acceptance Criteria:**
- [ ] LokiExporter created when config.Loki.Enabled && URL set
- [ ] Server continues working if LokiExporter creation fails (log warning)
- [ ] /health/loki endpoint returns status and stats
- [ ] Proxy uses ProxyLogger interface
- [ ] Server.Close() properly shuts down Loki with flush
- [ ] All existing tests pass

**Test Cases:**
- `TestNewServer_LokiDisabled` - no LokiExporter created
- `TestNewServer_LokiEnabled` - LokiExporter created and wired
- `TestNewServer_LokiInvalidURL` - continues without Loki, logs warning
- `TestHealthLoki_Disabled` - returns disabled status
- `TestHealthLoki_Enabled` - returns stats

**Constitution Reference:** @docs/constitutions/current/ for layer boundaries

---

### Task 5: Update Main for Startup Logging and Shutdown

**Size:** S (1h)
**Branch:** `825bce/05-main-updates`
**Depends on:** Task 4

**Description:**
Add Loki status to startup logging and verify graceful shutdown properly flushes Loki (FR5).

**Files to Modify:**
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/main.go`
- `/Users/drewritter/prime-rad/sen/llm-proxy/.worktrees/825bce-main/main_test.go`

**Implementation Details:**
1. Add startup log line for Loki status:
   ```go
   if cfg.Loki.Enabled {
       log.Printf("Loki export: enabled (%s)", cfg.Loki.URL)
   } else {
       log.Printf("Loki export: disabled")
   }
   ```
2. Verify existing shutdown code calls srv.Close() which cascades to MultiWriter.Close()
3. Existing 30s shutdown timeout already matches ECS stopTimeout (FR5)

**Acceptance Criteria:**
- [ ] Startup logs Loki status (enabled with URL or disabled)
- [ ] Graceful shutdown waits for Loki flush
- [ ] Timeout warning if flush takes >30s
- [ ] All existing tests pass

**Test Cases:**
- Existing main_test.go tests should continue passing
- Manual verification of startup logs with Loki enabled/disabled

**Constitution Reference:** @docs/constitutions/current/

---

## Dependency Graph

```
Task 1 (Config)
    │
    ▼
Task 2 (LokiExporter)
    │
    ▼
Task 3 (MultiWriter)
    │
    ▼
Task 4 (Server Integration)
    │
    ▼
Task 5 (Main Updates)
```

All tasks are sequential due to direct dependencies. Each task builds on the previous.

---

## Time Estimates

| Task | Size | Estimate | Cumulative |
|------|------|----------|------------|
| 1. Config | M | 3h | 3h |
| 2. LokiExporter | L | 5h | 8h |
| 3. MultiWriter | M | 4h | 12h |
| 4. Server Integration | M | 3h | 15h |
| 5. Main Updates | S | 1h | 16h |

**Total:** 16h

---

## Verification Checklist

After all tasks complete:

- [ ] `go test ./...` passes
- [ ] `go test -v -coverprofile=coverage.out ./...` shows >80% coverage for new code
- [ ] Build succeeds: `go build -o llm-proxy .`
- [ ] Manual test with local Loki (docker)
- [ ] Graceful shutdown flushes logs to Loki
- [ ] Proxy continues working if Loki unavailable

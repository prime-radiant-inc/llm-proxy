---
runId: 825bce
feature: loki-export
created: 2026-01-23
status: ready
---

# Feature: Loki Export for llm-proxy

## Overview

Add real-time log export to Grafana Loki from llm-proxy, enabling centralized observability for LLM API calls. Logs are pushed directly to Loki's HTTP API with batching, retry logic, and gzip compression.

## Goals

1. **Real-time export** - Push logs to Loki as they're written (not batch replay)
2. **Non-blocking** - Loki export must not add latency to proxied requests
3. **Graceful degradation** - If Loki unavailable, continue logging to files
4. **No log loss** - Buffer and retry failed pushes; flush on shutdown
5. **Session correlation** - Enable querying all logs for a single coding session

## Non-Goals

- Replacing file-based JSONL logging (kept as primary, Loki is secondary)
- Cost tracking or billing integration (future work)
- Multi-tenant isolation (single Loki instance for now)
- Promtail pattern (using direct push instead)

## Architecture

### Target State

```
Client → llm-proxy → Upstream API
              │
              ├──────────────────┐
              ▼                  ▼
         JSONL files      Loki (async)
         (unchanged)      http://sen-monitoring:3100
```

### Component Diagram

```
┌─────────────────────────────────────────────────────────────┐
│                        llm-proxy                             │
│                                                              │
│  ┌──────────┐     ┌─────────────────────────────────────┐  │
│  │  Proxy   │────▶│           MultiWriter                │  │
│  │ Handler  │     │                                      │  │
│  └──────────┘     │  ┌─────────────┐  ┌──────────────┐  │  │
│                   │  │ FileLogger  │  │ LokiExporter │  │  │
│                   │  │ (sync)      │  │ (async)      │  │  │
│                   │  └──────┬──────┘  └──────┬───────┘  │  │
│                   └─────────┼────────────────┼──────────┘  │
│                             │                │              │
└─────────────────────────────┼────────────────┼──────────────┘
                              │                │
                              ▼                ▼
                        JSONL files      Loki HTTP API
                                         /loki/api/v1/push
```

## Functional Requirements

### FR1: LokiExporter Component
- Async client that batches and pushes log entries to Loki
- Buffered channel with 10,000 entry capacity
- Batch by size (1000 entries) or time (5 seconds)
- Retry with exponential backoff (100ms base, max 10s, 5 retries)
- Add 25% jitter to prevent thundering herd
- Gzip compression for JSON payloads
- Track stats: entries_sent, entries_failed, entries_dropped, batches_sent

### FR2: MultiWriter Component
- Wraps FileLogger and LokiExporter with identical interface
- File errors are returned (primary logging)
- Loki errors are logged but don't fail the operation
- Handles nil LokiExporter gracefully

### FR3: Configuration
New config fields:
- `loki_enabled` (bool, default: false)
- `loki_url` (string)
- `loki_batch_size` (int, default: 1000)
- `loki_batch_wait` (string, default: "5s")
- `loki_retry_max` (int, default: 5)
- `loki_use_gzip` (bool, default: true)
- `loki_environment` (string, default: "development")

Environment variables with `LLM_PROXY_LOKI_` prefix.

### FR4: Health Endpoint
- Add `/health/loki` endpoint
- Returns: status, entries_sent, entries_dropped, last_error, last_error_time

### FR5: Graceful Shutdown
- On shutdown signal, drain channel and flush remaining entries
- 30-second timeout (matches ECS stopTimeout)
- Log warning if timeout exceeded

### FR6: Loki Push Format
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
    "values": [
      ["1706054400000000000", "{\"type\":\"request\",...}"]
    ]
  }]
}
```

Labels (low cardinality only):
- `app`: "llm-proxy" (static)
- `provider`: anthropic, openai, chatgpt (~3)
- `environment`: dev, staging, prod (~3)
- `machine`: user@hostname (~50)
- `log_type`: session_start, request, response, fork (~4)

High-cardinality fields (session_id, request_id) go in log body, not labels.

## Technical Decisions

1. **Direct Loki push** (not Promtail) - Lower latency, full label control
2. **Full bodies** - No truncation, we need complete prompt data for debugging
3. **Tailscale + VPC isolation** - Sufficient security for Phase 1
4. **Gzip compression** - Enabled by default for bandwidth savings

## Files to Create

| File | Purpose |
|------|---------|
| `loki_exporter.go` | Async Loki client (~250 LOC) |
| `loki_exporter_test.go` | Unit tests (~200 LOC) |
| `multi_writer.go` | Fan-out wrapper (~100 LOC) |
| `multi_writer_test.go` | Unit tests (~100 LOC) |

## Files to Modify

| File | Changes |
|------|---------|
| `config.go` | Add Loki config fields (+30 LOC) |
| `config_test.go` | Test Loki config loading (+50 LOC) |
| `server.go` | Initialize LokiExporter, create MultiWriter (+20 LOC) |
| `main.go` | Startup logging, shutdown handling (+10 LOC) |

## Success Criteria

1. Logs appear in Grafana within 10 seconds of API call
2. Session correlation works (all logs for session queryable via LogQL)
3. No measurable latency impact on proxied requests (<1ms)
4. Graceful shutdown flushes all buffered entries
5. Proxy continues working if Loki is unavailable
6. All existing tests pass
7. New tests have >80% coverage for new code

## Constitution Reference

All implementation must follow @docs/constitutions/current/:
- Layer boundaries per architecture.md
- Error handling per patterns.md (graceful degradation)
- Concurrency per patterns.md (channel-based async)
- Testing per testing.md (table-driven, 80%+ coverage)

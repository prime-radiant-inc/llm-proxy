# LLM Proxy Loki Export - Design Document

**Author:** Drew Ritter
**Status:** Draft
**Created:** 2026-01-23
**Related:** [Coding Container Logging & Telemetry Design](https://github.com/prime-radiant-inc/specs/blob/main/designs/we-need-a-coding-container-configuration-that-is-g-design.md)

---

## Overview

Add real-time log export to Grafana Loki from llm-proxy, enabling centralized observability for LLM API calls across ephemeral coding containers. This is part of the larger Coding Container Logging & Telemetry initiative.

### Goals

1. **Real-time export** - Push logs to Loki as they're written (not batch replay)
2. **Non-blocking** - Loki export must not add latency to proxied requests
3. **Graceful degradation** - If Loki is unavailable, continue logging to files
4. **No log loss** - Buffer and retry failed pushes; flush on shutdown
5. **Session correlation** - Enable querying all logs for a single coding session

### Non-Goals

- Replacing file-based JSONL logging (kept as primary, Loki is secondary)
- Cost tracking or billing integration (future work)
- Multi-tenant isolation (single Loki instance for now)

---

## Background: Why Not LiteLLM?

We evaluated LiteLLM as an alternative to modifying llm-proxy. Key findings:

| Concern | Details |
|---------|---------|
| **Claude Max auth broken** | Feature #13380 (Anthropic OAuth passthrough) not implemented. As of Jan 2026, Anthropic restricts Max tokens to official Claude Code only. |
| **Auth header bugs** | Issue #12090: Headers forwarded even when `forward_client_headers_to_llm_api: false` |
| **Scale issues** | DB bottleneck at 1M logs (~10 days at 100K req/day). Performance collapse at 300+ RPS. |
| **Memory leaks** | Requires periodic restarts under sustained load |
| **Python GIL** | Fundamental concurrency limitation for middleware |

**Decision:** Modify llm-proxy (Go, minimal dependencies, already works with Claude Max) rather than adopt LiteLLM.

---

## Architecture

### Current State

```
Client → llm-proxy → Upstream API
              │
              ▼
         JSONL files
         (~/.llm-provider-logs/)
```

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

---

## Detailed Design

### 1. LokiExporter (`loki_exporter.go`)

Async client that batches and pushes log entries to Loki.

```go
type LokiExporter struct {
    url         string
    labels      map[string]string  // Static labels for all entries
    entries     chan lokiEntry     // Buffered channel (capacity: 10000)
    client      *http.Client
    batchSize   int                // Default: 1000 (optimized for Loki efficiency)
    batchWait   time.Duration      // Default: 5s
    retryMax    int                // Default: 5
    retryWait   time.Duration      // Default: 100ms (doubles each retry, max 10s)
    useGzip     bool               // Default: true (compress JSON payloads)
    wg          sync.WaitGroup
    shutdown    chan struct{}
    mu          sync.Mutex
    stats       exporterStats      // Metrics: sent, failed, dropped
}

type lokiEntry struct {
    timestamp time.Time
    labels    map[string]string  // Per-entry labels (provider, etc.)
    line      string             // JSON-encoded log entry
}

type exporterStats struct {
    entriesSent    uint64
    entriesFailed  uint64
    entriesDropped uint64
    batchesSent    uint64
    lastError      error
    lastErrorTime  time.Time
}
```

**Key Methods:**

```go
// NewLokiExporter creates exporter with config
func NewLokiExporter(cfg LokiConfig) (*LokiExporter, error)

// Push adds entry to channel (non-blocking, drops if full)
func (l *LokiExporter) Push(entry map[string]interface{}, extraLabels map[string]string) error

// Close flushes remaining entries and shuts down (blocks up to 30s)
func (l *LokiExporter) Close() error

// Stats returns current export statistics
func (l *LokiExporter) Stats() exporterStats
```

**Background Worker:**

```go
func (l *LokiExporter) run() {
    ticker := time.NewTicker(l.batchWait)
    batch := make([]lokiEntry, 0, l.batchSize)

    for {
        select {
        case entry := <-l.entries:
            batch = append(batch, entry)
            if len(batch) >= l.batchSize {
                l.sendBatch(batch)
                batch = batch[:0]
            }
        case <-ticker.C:
            if len(batch) > 0 {
                l.sendBatch(batch)
                batch = batch[:0]
            }
        case <-l.shutdown:
            // Drain channel and send final batch
            l.drainAndFlush(batch)
            return
        }
    }
}
```

### 2. Loki Push Format

```json
{
  "streams": [
    {
      "stream": {
        "app": "llm-proxy",
        "provider": "anthropic",
        "environment": "production",
        "machine": "drew@macbook"
      },
      "values": [
        ["1706054400000000000", "{\"type\":\"request\",\"seq\":1,...}"],
        ["1706054401000000000", "{\"type\":\"response\",\"seq\":1,...}"]
      ]
    }
  ]
}
```

**Label Strategy (Low Cardinality):**

| Label | Source | Cardinality |
|-------|--------|-------------|
| `app` | Static | 1 |
| `provider` | Request | ~3 (anthropic, openai, chatgpt) |
| `environment` | Config | ~3 (dev, staging, prod) |
| `machine` | Runtime | ~10-50 |
| `log_type` | Entry | ~4 (session_start, request, response, fork) |

**NOT labels (in log body instead):**
- `session_id` - Too high cardinality
- `request_id` - Too high cardinality
- `path` - High cardinality
- `status` - Queryable via JSON parsing

### 3. MultiWriter (`multi_writer.go`)

Wraps FileLogger and LokiExporter with identical interface.

```go
type MultiWriter struct {
    file *Logger
    loki *LokiExporter
}

func NewMultiWriter(file *Logger, loki *LokiExporter) *MultiWriter

// All Logger methods implemented, forwarding to both
func (m *MultiWriter) LogSessionStart(sessionID, provider, upstream string) error
func (m *MultiWriter) LogRequest(...) error
func (m *MultiWriter) LogResponse(...) error
func (m *MultiWriter) LogFork(...) error
func (m *MultiWriter) RegisterUpstream(sessionID, upstream string)
func (m *MultiWriter) Close() error
```

**Error Handling:**
- File errors are returned (primary logging)
- Loki errors are logged but don't fail the operation
- Stats tracked for monitoring

### 4. Configuration (`config.go`)

```go
type Config struct {
    // Existing
    Port        int    `toml:"port"`
    LogDir      string `toml:"log_dir"`

    // New: Loki export
    LokiEnabled     bool   `toml:"loki_enabled"`
    LokiURL         string `toml:"loki_url"`
    LokiBatchSize   int    `toml:"loki_batch_size"`
    LokiBatchWait   string `toml:"loki_batch_wait"`  // Duration string
    LokiRetryMax    int    `toml:"loki_retry_max"`
    LokiUseGzip     bool   `toml:"loki_use_gzip"`
    LokiEnvironment string `toml:"loki_environment"`
}

func DefaultConfig() Config {
    return Config{
        Port:            8080,
        LogDir:          "./logs",
        LokiEnabled:     false,
        LokiURL:         "",
        LokiBatchSize:   1000,       // Optimized for Loki efficiency
        LokiBatchWait:   "5s",
        LokiRetryMax:    5,          // More resilience for transient failures
        LokiUseGzip:     true,       // Compress JSON payloads
        LokiEnvironment: "development",
    }
}
```

**Environment Variables:**

| Variable | Description | Default |
|----------|-------------|---------|
| `LLM_PROXY_LOKI_ENABLED` | Enable Loki export | `false` |
| `LLM_PROXY_LOKI_URL` | Loki push endpoint | (none) |
| `LLM_PROXY_LOKI_BATCH_SIZE` | Entries per batch | `1000` |
| `LLM_PROXY_LOKI_BATCH_WAIT` | Max wait before flush | `5s` |
| `LLM_PROXY_LOKI_RETRY_MAX` | Retries on failure | `5` |
| `LLM_PROXY_LOKI_USE_GZIP` | Compress JSON payloads | `true` |
| `LLM_PROXY_LOKI_ENVIRONMENT` | Environment label | `development` |

**Example config.toml:**

```toml
port = 8080
log_dir = "~/.llm-provider-logs"

[loki]
enabled = true
url = "http://sen-monitoring:3100/loki/api/v1/push"
batch_size = 1000
batch_wait = "5s"
retry_max = 5
use_gzip = true
environment = "production"
```

### 5. Server Integration (`server.go`)

```go
func NewServer(cfg Config) (*Server, error) {
    fileLogger, err := NewLogger(cfg.LogDir)
    if err != nil {
        return nil, err
    }

    var lokiExporter *LokiExporter
    if cfg.LokiEnabled && cfg.LokiURL != "" {
        lokiCfg := LokiConfig{
            URL:         cfg.LokiURL,
            BatchSize:   cfg.LokiBatchSize,
            BatchWait:   parseDuration(cfg.LokiBatchWait),
            RetryMax:    cfg.LokiRetryMax,
            Environment: cfg.LokiEnvironment,
        }
        lokiExporter, err = NewLokiExporter(lokiCfg)
        if err != nil {
            // Log warning but continue without Loki
            log.Printf("WARNING: Failed to create Loki exporter: %v", err)
        }
    }

    logger := NewMultiWriter(fileLogger, lokiExporter)
    // ... rest unchanged
}
```

### 6. Graceful Shutdown (`main.go`)

```go
go func() {
    <-ctx.Done()
    log.Println("Shutting down gracefully...")

    // Create shutdown context with 30s timeout (matches ECS stopTimeout)
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // Close server (which closes MultiWriter → flushes Loki)
    done := make(chan struct{})
    go func() {
        srv.Close()
        close(done)
    }()

    select {
    case <-done:
        log.Println("Shutdown complete")
    case <-shutdownCtx.Done():
        log.Println("Shutdown timed out")
    }

    listener.Close()
}()
```

---

## Loki Infrastructure

### Existing Setup

- **Host:** EC2 instance `sen-monitoring`
- **Access:**
  - Tailscale: `http://sen-monitoring:3100`
  - VPC internal: `10.x.x.x:3100` (port 3100 open to VPC CIDR)
- **Auth:** None (`auth_enabled: false`)
- **Retention:** 30 days
- **Storage:** Local filesystem (`/data/loki/`)

### Network Path for Containers

```
ECS Container (VPC) → Security Group → sen-monitoring:3100
                      (port 3100 allowed from VPC CIDR)
```

**Loki URL for containers:** `http://<monitoring-private-ip>:3100/loki/api/v1/push`

Or via Tailscale if containers have Tailscale: `http://sen-monitoring:3100/loki/api/v1/push`

### Future: S3 Backend

Current Loki uses local filesystem. Design doc mentions migrating to S3 for:
- Durability
- Cost (S3 tiering: Standard → IA → Glacier)
- Scalability

This is out of scope for Phase 1 but the llm-proxy changes are compatible.

---

## Testing Strategy

### Unit Tests

1. **LokiExporter**
   - `TestPush_AddsToChannel`
   - `TestPush_DropsWhenFull`
   - `TestBatching_SizeThreshold`
   - `TestBatching_TimeThreshold`
   - `TestRetry_ExponentialBackoffWithJitter`
   - `TestRetry_MaxBackoffCap`
   - `TestGzipCompression`
   - `TestClose_FlushesRemaining`

2. **MultiWriter**
   - `TestMultiWriter_BothCalled`
   - `TestMultiWriter_FileErrorReturned`
   - `TestMultiWriter_LokiErrorIgnored`
   - `TestMultiWriter_NilLoki`

3. **Config**
   - `TestLoadConfig_LokiDefaults`
   - `TestLoadConfig_LokiFromEnv`
   - `TestLoadConfig_LokiFromTOML`

### Integration Tests

1. **Mock Loki Server**
   - Verify correct push format
   - Verify labels and timestamps
   - Test retry behavior on 5xx
   - Test behavior on 429 (rate limit)

2. **Real Loki**
   - Start local Loki via Docker
   - Push entries, query back
   - Verify session correlation works

### Load Tests

1. **Latency Impact**
   - Measure proxy latency with/without Loki
   - Target: <1ms additional latency

2. **Throughput**
   - 1000 requests/second for 60 seconds
   - Verify no dropped entries
   - Verify memory stable

3. **Loki Unavailable**
   - Proxy continues working
   - Entries buffered up to capacity
   - Stats show dropped count

---

## Rollout Plan

### Phase 1: Development (This PR)

1. Implement `loki_exporter.go`
2. Implement `multi_writer.go`
3. Modify `config.go`
4. Modify `server.go`
5. Modify `main.go` (graceful shutdown)
6. Add unit tests
7. Add integration tests

### Phase 2: Local Testing

1. Run locally with Loki in Docker
2. Verify logs appear in Grafana
3. Test session correlation queries
4. Load test

### Phase 3: Staging Deploy

1. Deploy to single container
2. Verify connectivity to sen-monitoring
3. Monitor for errors
4. Check Loki ingestion rate

### Phase 4: Production

1. Enable for all coding containers
2. Create Grafana dashboards
3. Set up alerts

---

## Grafana Queries

Once deployed, these LogQL queries will work:

```logql
# All logs for a session
{app="llm-proxy"} | json | session_id="20260123-150405-a7f3"

# All requests to Anthropic in last hour
{app="llm-proxy", provider="anthropic"} | json | type="request"

# Errors only
{app="llm-proxy"} | json | status >= 400

# Slow responses (>5s)
{app="llm-proxy"} | json | type="response" | timing_total_ms > 5000

# Requests by machine
sum by (machine) (count_over_time({app="llm-proxy"} | json | type="request" [1h]))
```

---

## Resolved Questions

1. **Body truncation for Loki?**
   - **Decision:** Full bodies (no truncation). We need complete prompt data for debugging.
   - Loki 3.0 default max line size is 256KB, our 100KB bodies fit comfortably.

2. **Health endpoint for Loki connectivity?**
   - **Decision:** Add `/health/loki` endpoint showing connection status + stats.
   - Returns: `status`, `entries_sent`, `entries_dropped`, `last_error`, `last_error_time`

3. **Metrics export?**
   - **Decision:** Out of scope for Phase 1. Stats available via health endpoint.

4. **Security (TLS/Auth)?**
   - **Decision:** Tailscale + VPC isolation is sufficient for now.
   - Local engineers use Tailscale; containers use VPC-internal access.
   - Loki stays at `auth_enabled: false` for Phase 1.

5. **Retry strategy improvements?**
   - **Decision:** Increase retries from 3 to 5, add jitter to prevent thundering herd.
   - Backoff: 100ms base, doubles each retry, max 10s, +25% random jitter.

6. **Compression?**
   - **Decision:** Enable gzip compression by default for JSON payloads.
   - Significant bandwidth savings for 100KB+ response bodies.

7. **Direct Loki Push vs. Promtail?**
   - **Decision:** Direct Loki push (not Promtail pattern).
   - Reasoning:
     - Lower latency (~5s vs ~15s with Promtail)
     - Full label control for session correlation
     - Can use Loki 3.0 structured metadata
     - llm-proxy is infrastructure, not application - different pattern is justified
     - JSONL files remain primary; memory buffer loss on crash is acceptable
   - Trade-off: Inconsistent with PA/scheduler/dashboard (which use Promtail), but llm-proxy is a special case.

---

## Files Changed

| File | Change | Lines |
|------|--------|-------|
| `loki_exporter.go` | NEW | ~250 |
| `loki_exporter_test.go` | NEW | ~200 |
| `multi_writer.go` | NEW | ~100 |
| `multi_writer_test.go` | NEW | ~100 |
| `config.go` | MODIFY | +30 |
| `config_test.go` | MODIFY | +50 |
| `server.go` | MODIFY | +20 |
| `main.go` | MODIFY | +10 |
| `go.mod` | NO CHANGE | 0 |

**Total new code:** ~750 lines
**No new dependencies** (uses Go stdlib `net/http`)

---

## Success Criteria

1. Logs appear in Grafana within 10 seconds of API call
2. Session correlation works (all logs for session queryable)
3. No measurable latency impact on proxied requests (<1ms)
4. Graceful shutdown flushes all buffered entries
5. Proxy continues working if Loki is unavailable
6. All existing tests pass
7. New tests have >80% coverage for new code

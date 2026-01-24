# LLM Proxy Loki Export - Implementation Plan

**Author:** Drew Ritter
**Created:** 2026-01-23
**Design Doc:** [2026-01-23-loki-export-design.md](./2026-01-23-loki-export-design.md)

---

## Implementation Tasks

### Task 1: Add Loki Configuration to Config

**File:** `config.go`

**Changes:**
```go
type LokiConfig struct {
    Enabled     bool          `toml:"enabled"`
    URL         string        `toml:"url"`
    BatchSize   int           `toml:"batch_size"`
    BatchWait   time.Duration `toml:"-"`
    BatchWaitStr string       `toml:"batch_wait"`
    RetryMax    int           `toml:"retry_max"`
    Environment string        `toml:"environment"`
}

type Config struct {
    Port        int        `toml:"port"`
    LogDir      string     `toml:"log_dir"`
    Loki        LokiConfig `toml:"loki"`
    // ... existing fields
}
```

**Environment variable loading in `LoadConfigFromEnv`:**
```go
if v := os.Getenv("LLM_PROXY_LOKI_ENABLED"); v != "" {
    cfg.Loki.Enabled = v == "true" || v == "1"
}
if v := os.Getenv("LLM_PROXY_LOKI_URL"); v != "" {
    cfg.Loki.URL = v
}
if v := os.Getenv("LLM_PROXY_LOKI_BATCH_SIZE"); v != "" {
    if n, err := strconv.Atoi(v); err == nil {
        cfg.Loki.BatchSize = n
    }
}
if v := os.Getenv("LLM_PROXY_LOKI_BATCH_WAIT"); v != "" {
    cfg.Loki.BatchWaitStr = v
}
if v := os.Getenv("LLM_PROXY_LOKI_RETRY_MAX"); v != "" {
    if n, err := strconv.Atoi(v); err == nil {
        cfg.Loki.RetryMax = n
    }
}
if v := os.Getenv("LLM_PROXY_LOKI_ENVIRONMENT"); v != "" {
    cfg.Loki.Environment = v
}
```

**Default values:**
```go
func DefaultConfig() Config {
    return Config{
        Port:   8080,
        LogDir: "./logs",
        Loki: LokiConfig{
            Enabled:      false,
            URL:          "",
            BatchSize:    100,
            BatchWaitStr: "5s",
            RetryMax:     3,
            Environment:  "development",
        },
    }
}
```

**Tests to add (`config_test.go`):**
- `TestDefaultConfig_LokiDefaults`
- `TestLoadConfigFromEnv_LokiEnabled`
- `TestLoadConfigFromEnv_LokiURL`
- `TestLoadConfigFromEnv_LokiBatchSize`
- `TestLoadConfigFromTOML_LokiSection`

---

### Task 2: Create LokiExporter

**File:** `loki_exporter.go`

**Structs:**

```go
package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "net/http"
    "sync"
    "sync/atomic"
    "time"
)

// LokiConfig holds configuration for the Loki exporter
type LokiExporterConfig struct {
    URL         string
    BatchSize   int
    BatchWait   time.Duration
    RetryMax    int
    RetryWait   time.Duration
    Environment string
    BufferSize  int  // Channel buffer size, default 10000
}

// LokiExporter sends log entries to Grafana Loki asynchronously
type LokiExporter struct {
    config    LokiExporterConfig
    client    *http.Client
    entries   chan lokiEntry
    shutdown  chan struct{}
    wg        sync.WaitGroup
    machineID string

    // Stats (atomic)
    entriesSent    uint64
    entriesFailed  uint64
    entriesDropped uint64
    batchesSent    uint64
}

type lokiEntry struct {
    timestamp time.Time
    labels    map[string]string
    line      string
}

// lokiPushRequest is the Loki HTTP API format
type lokiPushRequest struct {
    Streams []lokiStream `json:"streams"`
}

type lokiStream struct {
    Stream map[string]string `json:"stream"`
    Values [][]string        `json:"values"`
}
```

**Constructor:**

```go
func NewLokiExporter(cfg LokiExporterConfig) (*LokiExporter, error) {
    if cfg.URL == "" {
        return nil, fmt.Errorf("loki URL is required")
    }
    if cfg.BatchSize <= 0 {
        cfg.BatchSize = 100
    }
    if cfg.BatchWait <= 0 {
        cfg.BatchWait = 5 * time.Second
    }
    if cfg.RetryMax <= 0 {
        cfg.RetryMax = 3
    }
    if cfg.RetryWait <= 0 {
        cfg.RetryWait = 1 * time.Second
    }
    if cfg.BufferSize <= 0 {
        cfg.BufferSize = 10000
    }

    l := &LokiExporter{
        config:    cfg,
        client:    &http.Client{Timeout: 30 * time.Second},
        entries:   make(chan lokiEntry, cfg.BufferSize),
        shutdown:  make(chan struct{}),
        machineID: getMachineID(),
    }

    l.wg.Add(1)
    go l.run()

    return l, nil
}
```

**Push method (non-blocking):**

```go
func (l *LokiExporter) Push(entry map[string]interface{}, provider string) {
    // Serialize entry to JSON
    line, err := json.Marshal(entry)
    if err != nil {
        atomic.AddUint64(&l.entriesDropped, 1)
        return
    }

    // Extract timestamp from entry._meta.ts or use now
    ts := time.Now()
    if meta, ok := entry["_meta"].(map[string]interface{}); ok {
        if tsStr, ok := meta["ts"].(string); ok {
            if parsed, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
                ts = parsed
            }
        }
    }

    // Extract log type
    logType := "unknown"
    if t, ok := entry["type"].(string); ok {
        logType = t
    }

    le := lokiEntry{
        timestamp: ts,
        labels: map[string]string{
            "app":         "llm-proxy",
            "provider":    provider,
            "environment": l.config.Environment,
            "machine":     l.machineID,
            "log_type":    logType,
        },
        line: string(line),
    }

    // Non-blocking send
    select {
    case l.entries <- le:
        // Sent successfully
    default:
        // Channel full, drop entry
        atomic.AddUint64(&l.entriesDropped, 1)
    }
}
```

**Background worker:**

```go
func (l *LokiExporter) run() {
    defer l.wg.Done()

    ticker := time.NewTicker(l.config.BatchWait)
    defer ticker.Stop()

    batch := make([]lokiEntry, 0, l.config.BatchSize)

    for {
        select {
        case entry := <-l.entries:
            batch = append(batch, entry)
            if len(batch) >= l.config.BatchSize {
                l.sendBatch(batch)
                batch = batch[:0]
            }

        case <-ticker.C:
            if len(batch) > 0 {
                l.sendBatch(batch)
                batch = batch[:0]
            }

        case <-l.shutdown:
            // Drain remaining entries from channel
            draining := true
            for draining {
                select {
                case entry := <-l.entries:
                    batch = append(batch, entry)
                default:
                    draining = false
                }
            }
            // Send final batch
            if len(batch) > 0 {
                l.sendBatch(batch)
            }
            return
        }
    }
}
```

**Send batch with retry:**

```go
func (l *LokiExporter) sendBatch(entries []lokiEntry) {
    if len(entries) == 0 {
        return
    }

    // Group entries by label set
    streams := make(map[string]*lokiStream)
    for _, e := range entries {
        // Create label key for grouping
        key := fmt.Sprintf("%s|%s|%s", e.labels["provider"], e.labels["environment"], e.labels["log_type"])

        stream, ok := streams[key]
        if !ok {
            stream = &lokiStream{
                Stream: e.labels,
                Values: make([][]string, 0),
            }
            streams[key] = stream
        }

        // Timestamp in nanoseconds as string
        tsNano := fmt.Sprintf("%d", e.timestamp.UnixNano())
        stream.Values = append(stream.Values, []string{tsNano, e.line})
    }

    // Build request
    req := lokiPushRequest{
        Streams: make([]lokiStream, 0, len(streams)),
    }
    for _, stream := range streams {
        req.Streams = append(req.Streams, *stream)
    }

    body, err := json.Marshal(req)
    if err != nil {
        atomic.AddUint64(&l.entriesFailed, uint64(len(entries)))
        return
    }

    // Retry loop
    var lastErr error
    wait := l.config.RetryWait
    for attempt := 0; attempt <= l.config.RetryMax; attempt++ {
        if attempt > 0 {
            time.Sleep(wait)
            wait *= 2 // Exponential backoff
        }

        err := l.doSend(body)
        if err == nil {
            atomic.AddUint64(&l.entriesSent, uint64(len(entries)))
            atomic.AddUint64(&l.batchesSent, 1)
            return
        }
        lastErr = err
    }

    // All retries failed
    atomic.AddUint64(&l.entriesFailed, uint64(len(entries)))
    log.Printf("loki: failed to send batch after %d retries: %v", l.config.RetryMax+1, lastErr)
}

func (l *LokiExporter) doSend(body []byte) error {
    req, err := http.NewRequest("POST", l.config.URL, bytes.NewReader(body))
    if err != nil {
        return err
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := l.client.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode >= 200 && resp.StatusCode < 300 {
        return nil
    }

    // Read error body for debugging
    var errBody bytes.Buffer
    errBody.ReadFrom(resp.Body)
    return fmt.Errorf("loki returned %d: %s", resp.StatusCode, errBody.String())
}
```

**Close method:**

```go
func (l *LokiExporter) Close() error {
    close(l.shutdown)

    // Wait for worker to finish (with timeout)
    done := make(chan struct{})
    go func() {
        l.wg.Wait()
        close(done)
    }()

    select {
    case <-done:
        return nil
    case <-time.After(30 * time.Second):
        return fmt.Errorf("loki exporter shutdown timed out")
    }
}
```

**Stats method:**

```go
type LokiStats struct {
    EntriesSent    uint64
    EntriesFailed  uint64
    EntriesDropped uint64
    BatchesSent    uint64
}

func (l *LokiExporter) Stats() LokiStats {
    return LokiStats{
        EntriesSent:    atomic.LoadUint64(&l.entriesSent),
        EntriesFailed:  atomic.LoadUint64(&l.entriesFailed),
        EntriesDropped: atomic.LoadUint64(&l.entriesDropped),
        BatchesSent:    atomic.LoadUint64(&l.batchesSent),
    }
}
```

**Tests to write (`loki_exporter_test.go`):**
- `TestNewLokiExporter_RequiresURL`
- `TestNewLokiExporter_DefaultValues`
- `TestPush_AddsToChannel`
- `TestPush_DropsWhenChannelFull`
- `TestPush_ExtractsTimestamp`
- `TestPush_ExtractsLogType`
- `TestSendBatch_GroupsByLabels`
- `TestSendBatch_RetriesOnError`
- `TestSendBatch_ExponentialBackoff`
- `TestClose_FlushesRemaining`
- `TestClose_TimesOut`
- `TestStats_Accurate`

---

### Task 3: Create MultiWriter

**File:** `multi_writer.go`

```go
package main

import "net/http"

// MultiWriter wraps file logger and optional Loki exporter
type MultiWriter struct {
    file *Logger
    loki *LokiExporter
}

func NewMultiWriter(file *Logger, loki *LokiExporter) *MultiWriter {
    return &MultiWriter{file: file, loki: loki}
}

func (m *MultiWriter) RegisterUpstream(sessionID, upstream string) {
    m.file.RegisterUpstream(sessionID, upstream)
}

func (m *MultiWriter) LogSessionStart(sessionID, provider, upstream string) error {
    err := m.file.LogSessionStart(sessionID, provider, upstream)

    if m.loki != nil {
        entry := map[string]interface{}{
            "type":     "session_start",
            "provider": provider,
            "upstream": upstream,
            "_meta": map[string]interface{}{
                "ts":      timeNowRFC3339Nano(),
                "machine": m.file.machineID,
                "host":    upstream,
                "session": sessionID,
            },
        }
        m.loki.Push(entry, provider)
    }

    return err
}

func (m *MultiWriter) LogRequest(sessionID, provider string, seq int, method, path string, headers http.Header, body []byte, requestID string) error {
    err := m.file.LogRequest(sessionID, provider, seq, method, path, headers, body, requestID)

    if m.loki != nil {
        upstream := m.file.upstreams[sessionID]
        entry := map[string]interface{}{
            "type":    "request",
            "seq":     seq,
            "method":  method,
            "path":    path,
            "headers": ObfuscateHeaders(headers),
            "body":    string(body),
            "size":    len(body),
            "_meta": map[string]interface{}{
                "ts":         timeNowRFC3339Nano(),
                "machine":    m.file.machineID,
                "host":       upstream,
                "session":    sessionID,
                "request_id": requestID,
            },
        }
        m.loki.Push(entry, provider)
    }

    return err
}

func (m *MultiWriter) LogResponse(sessionID, provider string, seq int, status int, headers http.Header, body []byte, chunks []StreamChunk, timing ResponseTiming, requestID string) error {
    err := m.file.LogResponse(sessionID, provider, seq, status, headers, body, chunks, timing, requestID)

    if m.loki != nil {
        upstream := m.file.upstreams[sessionID]
        entry := map[string]interface{}{
            "type":    "response",
            "seq":     seq,
            "status":  status,
            "headers": headers,
            "timing":  timing,
            "size":    len(body),
            "_meta": map[string]interface{}{
                "ts":         timeNowRFC3339Nano(),
                "machine":    m.file.machineID,
                "host":       upstream,
                "session":    sessionID,
                "request_id": requestID,
            },
        }

        if chunks != nil {
            entry["chunks"] = chunks
        } else {
            entry["body"] = string(body)
        }

        m.loki.Push(entry, provider)
    }

    return err
}

func (m *MultiWriter) LogFork(sessionID, provider string, fromSeq int, parentSession string) error {
    err := m.file.LogFork(sessionID, provider, fromSeq, parentSession)

    if m.loki != nil {
        upstream := m.file.upstreams[sessionID]
        entry := map[string]interface{}{
            "type":           "fork",
            "from_seq":       fromSeq,
            "parent_session": parentSession,
            "reason":         "message_history_diverged",
            "_meta": map[string]interface{}{
                "ts":      timeNowRFC3339Nano(),
                "machine": m.file.machineID,
                "host":    upstream,
                "session": sessionID,
            },
        }
        m.loki.Push(entry, provider)
    }

    return err
}

func (m *MultiWriter) Close() error {
    var err error

    // Close Loki first (flushes remaining entries)
    if m.loki != nil {
        if lokiErr := m.loki.Close(); lokiErr != nil {
            err = lokiErr
        }
    }

    // Then close file logger
    if fileErr := m.file.Close(); fileErr != nil && err == nil {
        err = fileErr
    }

    return err
}

func timeNowRFC3339Nano() string {
    return time.Now().UTC().Format(time.RFC3339Nano)
}
```

**Tests to write (`multi_writer_test.go`):**
- `TestMultiWriter_LogSessionStart_BothCalled`
- `TestMultiWriter_LogRequest_BothCalled`
- `TestMultiWriter_LogResponse_BothCalled`
- `TestMultiWriter_LogFork_BothCalled`
- `TestMultiWriter_NilLoki_NoError`
- `TestMultiWriter_FileError_Returned`
- `TestMultiWriter_Close_ClosesLokiFirst`

---

### Task 4: Integrate into Server

**File:** `server.go`

**Changes:**

```go
type Server struct {
    config         Config
    mux            *http.ServeMux
    proxy          *Proxy
    fileLogger     *Logger        // Keep reference for direct access if needed
    lokiExporter   *LokiExporter  // Keep reference for stats
    logger         *MultiWriter   // Used by proxy
    sessionManager *SessionManager
}

func NewServer(cfg Config) (*Server, error) {
    fileLogger, err := NewLogger(cfg.LogDir)
    if err != nil {
        return nil, err
    }

    var lokiExporter *LokiExporter
    if cfg.Loki.Enabled && cfg.Loki.URL != "" {
        batchWait, err := time.ParseDuration(cfg.Loki.BatchWaitStr)
        if err != nil {
            batchWait = 5 * time.Second
        }

        lokiCfg := LokiExporterConfig{
            URL:         cfg.Loki.URL,
            BatchSize:   cfg.Loki.BatchSize,
            BatchWait:   batchWait,
            RetryMax:    cfg.Loki.RetryMax,
            Environment: cfg.Loki.Environment,
        }

        lokiExporter, err = NewLokiExporter(lokiCfg)
        if err != nil {
            log.Printf("WARNING: Failed to create Loki exporter: %v (continuing without Loki)", err)
        } else {
            log.Printf("Loki export enabled: %s", cfg.Loki.URL)
        }
    }

    logger := NewMultiWriter(fileLogger, lokiExporter)

    sessionManager, err := NewSessionManager(cfg.LogDir, fileLogger)
    if err != nil {
        logger.Close()
        return nil, err
    }

    s := &Server{
        config:         cfg,
        mux:            http.NewServeMux(),
        fileLogger:     fileLogger,
        lokiExporter:   lokiExporter,
        logger:         logger,
        sessionManager: sessionManager,
    }

    s.proxy = NewProxyWithMultiWriter(logger, sessionManager)
    s.mux.HandleFunc("/health", s.handleHealth)

    return s, nil
}

func (s *Server) Close() error {
    var err error
    if s.sessionManager != nil {
        err = s.sessionManager.Close()
    }
    if s.logger != nil {
        if logErr := s.logger.Close(); logErr != nil && err == nil {
            err = logErr
        }
    }
    return err
}
```

**Also update proxy.go to use MultiWriter interface:**

```go
// Define interface for logging (so proxy doesn't care about implementation)
type ProxyLogger interface {
    RegisterUpstream(sessionID, upstream string)
    LogSessionStart(sessionID, provider, upstream string) error
    LogRequest(sessionID, provider string, seq int, method, path string, headers http.Header, body []byte, requestID string) error
    LogResponse(sessionID, provider string, seq int, status int, headers http.Header, body []byte, chunks []StreamChunk, timing ResponseTiming, requestID string) error
    LogFork(sessionID, provider string, fromSeq int, parentSession string) error
}

type Proxy struct {
    client         *http.Client
    logger         ProxyLogger  // Interface instead of concrete type
    sessionManager *SessionManager
}

func NewProxyWithMultiWriter(logger *MultiWriter, sm *SessionManager) *Proxy {
    return &Proxy{
        client:         createPassthroughClient(),
        logger:         logger,
        sessionManager: sm,
    }
}
```

---

### Task 5: Update Main for Graceful Shutdown

**File:** `main.go`

The existing graceful shutdown already calls `srv.Close()` which will now close the MultiWriter, which flushes Loki. Just verify the shutdown timeout is sufficient (30s matches ECS).

**Verify this code exists:**
```go
// Setup graceful shutdown
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer stop()

// ... server startup ...

// Run shutdown handler in background
go func() {
    <-ctx.Done()
    log.Println("Shutting down gracefully...")
    srv.Close()  // This now closes MultiWriter → flushes Loki
    listener.Close()
}()
```

**Add startup log for Loki status:**
```go
log.Printf("Starting llm-proxy on :%d", actualPort)
log.Printf("Log directory: %s", cfg.LogDir)
if cfg.Loki.Enabled {
    log.Printf("Loki export: enabled (%s)", cfg.Loki.URL)
} else {
    log.Printf("Loki export: disabled")
}
```

---

## Testing Plan

### Unit Test Commands

```bash
# From project root:

# Run all tests
go test -v ./...

# Run with coverage
go test -v -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html

# Run specific test file
go test -v -run TestLoki ./...
go test -v -run TestMultiWriter ./...
```

### Integration Test with Local Loki

```bash
# Start local Loki
docker run -d --name loki -p 3100:3100 grafana/loki:2.9.2

# Run proxy with Loki enabled
LLM_PROXY_LOKI_ENABLED=true \
LLM_PROXY_LOKI_URL=http://localhost:3100/loki/api/v1/push \
go run .

# Make a test request (in another terminal)
curl -X POST http://localhost:8080/anthropic/api.anthropic.com/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: sk-ant-test" \
  -d '{"model":"claude-3-sonnet","messages":[{"role":"user","content":"test"}]}'

# Query Loki
curl -G http://localhost:3100/loki/api/v1/query \
  --data-urlencode 'query={app="llm-proxy"}' | jq .

# Cleanup
docker rm -f loki
```

### Load Test

```bash
# Install hey (HTTP load generator)
brew install hey

# Run load test (1000 requests, 10 concurrent)
hey -n 1000 -c 10 -m POST \
  -H "Content-Type: application/json" \
  -H "x-api-key: sk-ant-test" \
  -d '{"model":"claude-3-sonnet","messages":[{"role":"user","content":"test"}]}' \
  http://localhost:8080/anthropic/api.anthropic.com/v1/messages

# Check Loki stats (add endpoint if implemented)
curl http://localhost:8080/health/loki
```

---

## Checklist

### Implementation
- [ ] Task 1: Config changes
- [ ] Task 2: LokiExporter
- [ ] Task 3: MultiWriter
- [ ] Task 4: Server integration
- [ ] Task 5: Main.go updates

### Testing
- [ ] Unit tests for LokiExporter
- [ ] Unit tests for MultiWriter
- [ ] Unit tests for Config
- [ ] Integration test with local Loki
- [ ] Load test (verify no latency impact)
- [ ] Test graceful shutdown flushes Loki

### Documentation
- [ ] Update README with Loki configuration
- [ ] Add example config.toml with Loki section

### Review
- [ ] Code review
- [ ] Test in staging (single container)
- [ ] Verify logs in Grafana

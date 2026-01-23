# Patterns

## Error Handling

### Graceful Degradation
Secondary features must not break primary functionality:

```go
// Good: Loki failure doesn't break request proxying
lokiExporter, err = NewLokiExporter(cfg)
if err != nil {
    log.Printf("WARNING: Loki init failed: %v (continuing without Loki)", err)
    // Continue with lokiExporter = nil
}

// Bad: Failing the entire server
if err != nil {
    return nil, err  // Don't do this for optional features
}
```

### Error Propagation
- Primary features (file logging): Return errors to caller
- Secondary features (Loki export): Log errors, don't propagate
- CLI operations: Log fatal and exit with non-zero code

## Concurrency

### Channel-Based Async
Use buffered channels for non-blocking operations:

```go
type Exporter struct {
    entries chan entry     // Buffered channel
    shutdown chan struct{} // Signal channel
}

// Non-blocking send with drop on full
func (e *Exporter) Push(entry entry) {
    select {
    case e.entries <- entry:
        // Success
    default:
        // Channel full, drop and count
        atomic.AddUint64(&e.stats.dropped, 1)
    }
}
```

### Mutex Usage
- Use `sync.Mutex` for protecting shared maps
- Keep critical sections small
- Always use defer for unlock

```go
func (l *Logger) getFile(sessionID string) (*os.File, error) {
    l.mu.Lock()
    defer l.mu.Unlock()

    // Critical section: map access only
    if f, ok := l.files[sessionID]; ok {
        return f, nil
    }
    // ... file creation
}
```

## Configuration

### Layered Config Loading
Config sources in priority order (highest wins):

1. CLI flags
2. Environment variables
3. TOML config file
4. Default values

```go
cfg := DefaultConfig()              // Defaults
cfg, _ = LoadConfigFromTOML(data)   // TOML overrides
cfg = LoadConfigFromEnv(cfg)        // Env overrides
cfg = MergeConfig(cfg, flags)       // CLI overrides
```

### Environment Variable Naming
Prefix with `LLM_PROXY_`:
- `LLM_PROXY_PORT`
- `LLM_PROXY_LOG_DIR`
- `LLM_PROXY_LOKI_ENABLED`

## Logging

### JSONL Log Format
Each log entry is a single JSON line with required fields:

```json
{
  "type": "request|response|session_start|fork",
  "_meta": {
    "ts": "2026-01-23T15:04:05.123456789Z",
    "machine": "user@hostname",
    "session": "20260123-150405-a1b2",
    "request_id": "uuid-string"
  }
}
```

### Log Entry Types
- `session_start`: New session, includes provider and upstream
- `request`: Full request body, obfuscated headers, size
- `response`: Full response body OR streaming chunks, status, timing
- `fork`: Session diverged, includes parent reference

### Header Obfuscation
Always obfuscate sensitive headers before logging:

```go
headers := ObfuscateHeaders(r.Header)
// Authorization: Bearer sk-ant-api03-xxx...xxx → sk-ant-api03-xxx...1234
```

## HTTP Proxy

### URL Structure
Proxy URLs must follow: `/{provider}/{upstream}/{path}`

```go
// Parse: /anthropic/api.anthropic.com/v1/messages
provider = "anthropic"
upstream = "api.anthropic.com"
path = "/v1/messages"
```

### Header Copying
Copy all headers except hop-by-hop:

```go
func copyHeaders(dst, src http.Header) {
    for key, values := range src {
        for _, value := range values {
            dst.Add(key, value)
        }
    }
}
```

### Streaming Response Handling
For SSE responses (`Content-Type: text/event-stream`):

1. Wrap ResponseWriter with StreamingResponseWriter
2. Read line-by-line from upstream
3. Write and flush immediately to client
4. Accumulate chunks for logging
5. Log complete response after stream ends

## Session Tracking

### Session ID Format
`YYYYMMDD-HHMMSS-{4-byte-random-hex}`

Example: `20260123-150405-a1b2c3d4`

### Client Session ID Extraction
Extract from request body based on provider:

```go
// Claude Code: metadata.user_id
// OpenAI: Custom header or body field
clientSessionID := ExtractClientSessionID(body, provider, headers, path)
```

### Sequence Numbering
- Sequence 1: First request in session (triggers session_start log)
- Sequence N+1: Subsequent requests increment

## Graceful Shutdown

### Signal Handling
```go
ctx, stop := signal.NotifyContext(context.Background(),
    syscall.SIGINT, syscall.SIGTERM)
defer stop()

go func() {
    <-ctx.Done()
    // Shutdown sequence:
    // 1. Stop accepting new requests
    // 2. Flush buffers (Loki, etc.)
    // 3. Close file handles
    // 4. Close listener
}()
```

### Shutdown Timeout
Match container orchestrator timeout (e.g., 30s for ECS):

```go
shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
```

## Testing

### Test File Naming
- Unit tests: `{file}_test.go` (e.g., `logger_test.go`)
- Integration tests: `integration_test.go`
- E2E tests: `e2e_test.go`

### Test Isolation
- Use `t.TempDir()` for file-based tests
- Clean up resources in defer blocks
- Don't rely on test execution order

### Live API Tests
Gate behind `-short` flag:

```go
func TestLiveAPI(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping live API test")
    }
    // ... test with real API
}
```

# Implementation Plan Revision Notes

These fixes must be applied to `2026-01-13-transparent-agent-logger-implementation.md` before execution.

## Critical Bug #1: Session Continuation Detection Broken

### Problem
`RecordResponse(sessionID, seq, requestBody)` only has request body. It fingerprints `[user:"hello"]` and stores that.

Next request has `messages=[user:"hello", assistant:"hi", user:"new"]`. It computes prior as `[user:"hello", assistant:"hi"]` which **doesn't match** stored fingerprint. Creates new session instead of continuing.

### Fix
```go
// OLD - broken
func (sm *SessionManager) RecordResponse(sessionID string, seq int, requestBody []byte) error

// NEW - fixed
func (sm *SessionManager) RecordResponse(sessionID string, seq int, requestBody, responseBody []byte, provider string) error {
    // Extract assistant's reply from API response
    assistantMsg, err := ExtractAssistantMessage(responseBody, provider)
    if err != nil {
        return err
    }

    // Get original messages from request
    messages, _ := ExtractMessages(requestBody, provider)

    // Build complete state: request messages + assistant reply
    fullState := append(messages, assistantMsg)

    // Fingerprint the full state - this is what next request's prior will match
    stateJSON, _ := json.Marshal(fullState)
    fingerprint := FingerprintMessages(stateJSON)

    return sm.db.UpdateSessionFingerprint(sessionID, seq, fingerprint)
}

// NEW function needed
func ExtractAssistantMessage(responseBody []byte, provider string) (map[string]interface{}, error) {
    var resp map[string]interface{}
    json.Unmarshal(responseBody, &resp)

    if provider == "anthropic" {
        // Anthropic: {"content": [{"type": "text", "text": "..."}], ...}
        content := resp["content"].([]interface{})
        if len(content) > 0 {
            block := content[0].(map[string]interface{})
            return map[string]interface{}{
                "role":    "assistant",
                "content": block["text"],
            }, nil
        }
    } else if provider == "openai" {
        // OpenAI: {"choices": [{"message": {"role": "assistant", "content": "..."}}]}
        choices := resp["choices"].([]interface{})
        if len(choices) > 0 {
            choice := choices[0].(map[string]interface{})
            return choice["message"].(map[string]interface{}), nil
        }
    }
    return nil, fmt.Errorf("could not extract assistant message")
}
```

### Update Call Sites
In `proxy.go ServeHTTP`:
```go
// After non-streaming response
if p.sessionManager != nil && isConversationEndpoint(path) {
    p.sessionManager.RecordResponse(sessionID, seq, reqBody, respBody, provider)
}

// After streaming response - need to accumulate chunks to get full response
// Either reconstruct from chunks or parse final message
```

### Update Tests
```go
func TestSessionManagerContinuation(t *testing.T) {
    // ... setup ...

    body1 := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
    sessionID1, _, _, _ := sm.GetOrCreateSession(body1, "anthropic", "api.anthropic.com")

    // Mock API response with assistant reply
    response1 := []byte(`{"content":[{"type":"text","text":"hi"}]}`)
    sm.RecordResponse(sessionID1, 1, body1, response1, "anthropic")

    // Now continuation will work because we stored fingerprint of [user:hello, assistant:hi]
    body2 := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"how are you"}]}`)
    sessionID2, seq2, isNew, _ := sm.GetOrCreateSession(body2, "anthropic", "api.anthropic.com")

    // assertions...
}
```

---

## Critical Bug #2: copyLogToForkPoint Copies Entire File

### Problem
```go
// Current broken implementation
func (sm *SessionManager) copyLogToForkPoint(srcPath, dstPath string, forkSeq int) error {
    // ...
    _, err = io.Copy(dst, src)  // WRONG: copies everything
    return err
}
```

### Fix
```go
func (sm *SessionManager) copyLogToForkPoint(srcPath, dstPath string, forkSeq int) error {
    srcFullPath := filepath.Join(sm.baseDir, srcPath)
    dstFullPath := filepath.Join(sm.baseDir, dstPath)

    if err := os.MkdirAll(filepath.Dir(dstFullPath), 0755); err != nil {
        return err
    }

    src, err := os.Open(srcFullPath)
    if err != nil {
        return nil // Source doesn't exist, nothing to copy
    }
    defer src.Close()

    dst, err := os.Create(dstFullPath)
    if err != nil {
        return err
    }
    defer dst.Close()

    scanner := bufio.NewScanner(src)
    for scanner.Scan() {
        line := scanner.Bytes()

        var entry map[string]interface{}
        if err := json.Unmarshal(line, &entry); err != nil {
            continue
        }

        // Always copy session_start
        if entry["type"] == "session_start" {
            dst.Write(line)
            dst.Write([]byte("\n"))
            continue
        }

        // For request/response, check seq
        if seq, ok := entry["seq"].(float64); ok {
            if int(seq) > forkSeq {
                break // Stop at fork point
            }
        }

        dst.Write(line)
        dst.Write([]byte("\n"))
    }

    return scanner.Err()
}
```

---

## Bug #3: Missing Fork Log Entry

### Add to logger.go
```go
func (l *Logger) LogFork(sessionID, provider string, fromSeq int, parentSession string) error {
    entry := map[string]interface{}{
        "type":           "fork",
        "ts":             time.Now().UTC().Format(time.RFC3339Nano),
        "from_seq":       fromSeq,
        "parent_session": parentSession,
        "reason":         "message_history_diverged",
    }
    return l.writeEntry(sessionID, provider, entry)
}
```

### Call in createForkSession
```go
func (sm *SessionManager) createForkSession(...) {
    // ... after copying file ...

    // Need logger reference - add to SessionManager struct
    if sm.logger != nil {
        sm.logger.LogFork(sessionID, provider, forkSeq, parentSession)
    }

    // ...
}
```

---

## Bug #4: isLocalhost Panics on Short Strings

### Current (broken)
```go
func isLocalhost(host string) bool {
    return len(host) >= 9 && host[:9] == "127.0.0.1" ||
        len(host) >= 9 && host[:9] == "localhost"
}
```

### Fixed
```go
func isLocalhost(host string) bool {
    return strings.HasPrefix(host, "127.0.0.1") ||
           strings.HasPrefix(host, "localhost")
}
```

---

## Bug #5: streaming.go Reinvents String Functions

### Current (buggy)
```go
func contains(body []byte, substr string) bool { ... }
func indexOf(s, substr string) int { ... }
```

### Fixed
```go
import "strings"

func isStreamingRequest(body []byte) bool {
    s := string(body)
    return strings.Contains(s, `"stream":true`) ||
           strings.Contains(s, `"stream": true`)
}
```

---

## Bug #6: No Graceful Shutdown

### Add to main.go
```go
import (
    "context"
    "os/signal"
    "syscall"
)

func main() {
    // ... flag parsing, config loading ...

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    srv := NewServer(cfg)

    go func() {
        <-ctx.Done()
        log.Println("Shutting down...")
        srv.Close()
    }()

    addr := fmt.Sprintf(":%d", cfg.Port)
    log.Printf("Starting on %s", addr)

    if err := http.ListenAndServe(addr, srv); err != nil && err != http.ErrServerClosed {
        log.Fatalf("Server error: %v", err)
    }
}
```

---

## Bug #7: Streaming Response Fingerprinting

For streaming responses, we need to accumulate the assistant's text from `content_block_delta` events to build the full response for fingerprinting.

### Add to streaming.go
```go
type StreamingResponseWriter struct {
    // ... existing fields ...
    accumulatedText strings.Builder  // NEW: accumulate assistant response
}

func (s *StreamingResponseWriter) Write(data []byte) (int, error) {
    // ... existing chunk capture ...

    // Parse SSE event and accumulate text deltas
    if strings.Contains(string(data), "content_block_delta") {
        // Extract delta text and append to accumulatedText
        var event map[string]interface{}
        // Parse "data: {...}" line
        // ...
        if delta, ok := event["delta"].(map[string]interface{}); ok {
            if text, ok := delta["text"].(string); ok {
                s.accumulatedText.WriteString(text)
            }
        }
    }

    return s.ResponseWriter.Write(data)
}

func (s *StreamingResponseWriter) AccumulatedText() string {
    return s.accumulatedText.String()
}
```

### Update streamResponse to pass accumulated text
```go
func streamResponse(...) error {
    // ... existing streaming logic ...

    // After streaming complete, build mock response for fingerprinting
    if sessionManager != nil {
        mockResponse := fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`,
            sw.AccumulatedText())
        sessionManager.RecordResponse(sessionID, seq, reqBody, []byte(mockResponse), provider)
    }

    return nil
}
```

---

## Missing: OpenAI Provider

Add note that OpenAI support is Phase 8 or future work. The key differences:
- Response format: `response.choices[0].message.content` vs `response.content[0].text`
- Streaming format: Different SSE event structure
- `ExtractAssistantMessage` already handles both providers

---

## Task Dependencies

After fixing, tasks must be reordered:
1. Task 16 (fingerprint.go) must add `ExtractAssistantMessage`
2. Task 17 (session.go) must update `RecordResponse` signature
3. Task 17 tests must include mock response bodies
4. Task 13 (streaming.go) must add text accumulation
5. Task 18 (proxy integration) must pass response body to RecordResponse
6. Task 10 (logger.go) must add `LogFork` method

---

## Files to Update

1. `fingerprint.go` - Add `ExtractAssistantMessage`
2. `session.go` - Fix `RecordResponse`, `copyLogToForkPoint`, add logger field
3. `session_test.go` - Fix tests to include response bodies
4. `streaming.go` - Fix string functions, add text accumulation
5. `proxy.go` - Fix `isLocalhost`, pass response to `RecordResponse`
6. `logger.go` - Add `LogFork`
7. `main.go` - Add graceful shutdown
8. `server.go` - Pass logger to SessionManager

# Testing Standards

## Test Categories

### Unit Tests (`go test -short`)
- Test individual functions in isolation
- Mock external dependencies (HTTP, filesystem, database)
- Fast execution (< 1 second per test)
- Run on every commit

### Integration Tests (`integration_test.go`)
- Test component interactions
- Use real SQLite (in temp directory)
- Use real filesystem (in temp directory)
- Mock only external HTTP APIs

### E2E Tests (`e2e_test.go`, `*_e2e_test.go`)
- Test full request flow through proxy
- Start actual HTTP server
- Use mock upstream or real APIs (gated)

### Live API Tests (`TestLive*`)
- Test against real LLM provider APIs
- Require API keys in `~/.amplifier/keys.env`
- Skip with `-short` flag
- Run manually before releases

## Test Structure

### Table-Driven Tests
Use for testing multiple cases:

```go
func TestParseProxyURL(t *testing.T) {
    tests := []struct {
        name     string
        input    string
        provider string
        upstream string
        path     string
        wantErr  error
    }{
        {
            name:     "valid anthropic",
            input:    "/anthropic/api.anthropic.com/v1/messages",
            provider: "anthropic",
            upstream: "api.anthropic.com",
            path:     "/v1/messages",
        },
        {
            name:    "invalid provider",
            input:   "/invalid/api.example.com/v1/test",
            wantErr: ErrUnknownProvider,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            provider, upstream, path, err := ParseProxyURL(tt.input)
            if tt.wantErr != nil {
                if err != tt.wantErr {
                    t.Errorf("got error %v, want %v", err, tt.wantErr)
                }
                return
            }
            if err != nil {
                t.Fatalf("unexpected error: %v", err)
            }
            if provider != tt.provider {
                t.Errorf("provider = %q, want %q", provider, tt.provider)
            }
            // ... more assertions
        })
    }
}
```

### Test Helpers
Create helpers for common setup:

```go
func setupTestLogger(t *testing.T) (*Logger, string) {
    t.Helper()
    dir := t.TempDir()
    logger, err := NewLogger(dir)
    if err != nil {
        t.Fatalf("failed to create logger: %v", err)
    }
    t.Cleanup(func() { logger.Close() })
    return logger, dir
}
```

### HTTP Test Servers
Use `httptest` for mock servers:

```go
func TestProxy(t *testing.T) {
    upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        w.Write([]byte(`{"response": "ok"}`))
    }))
    defer upstream.Close()

    // Test proxy against mock upstream
}
```

## Assertions

### Use Standard Library
Prefer simple assertions over assertion libraries:

```go
// Good
if got != want {
    t.Errorf("got %v, want %v", got, want)
}

// Also good for fatal errors
if err != nil {
    t.Fatalf("unexpected error: %v", err)
}
```

### Error Checking
Check for specific errors when possible:

```go
if !errors.Is(err, ErrInvalidProxyPath) {
    t.Errorf("got error %v, want ErrInvalidProxyPath", err)
}
```

## Test Data

### Temp Directories
Always use `t.TempDir()`:

```go
func TestLogger(t *testing.T) {
    dir := t.TempDir()  // Auto-cleaned after test
    logger, _ := NewLogger(dir)
    // ...
}
```

### Test Fixtures
Place in `testdata/` directory:

```
testdata/
├── sample_request.json
├── sample_response.json
└── sample_session.jsonl
```

Load with:
```go
data, err := os.ReadFile("testdata/sample_request.json")
```

## Coverage

### Minimum Coverage
- New code: 80%+
- Critical paths (proxy, logging): 90%+

### Check Coverage
```bash
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

## Naming Conventions

### Test Functions
- `TestFunctionName` - Basic functionality
- `TestFunctionName_EdgeCase` - Specific scenarios
- `TestFunctionName_Error` - Error conditions

### Subtest Names
Use descriptive lowercase with spaces:

```go
t.Run("valid input returns expected output", func(t *testing.T) { ... })
t.Run("empty input returns error", func(t *testing.T) { ... })
```

## Parallel Tests

Mark independent tests as parallel:

```go
func TestIndependent(t *testing.T) {
    t.Parallel()
    // ...
}
```

**Do not parallelize** tests that:
- Share global state
- Use the same temp files
- Depend on test execution order

## Mocking

### Interface-Based Mocking
Define interfaces for external dependencies:

```go
type HTTPClient interface {
    Do(req *http.Request) (*http.Response, error)
}

// In tests:
type mockClient struct {
    response *http.Response
    err      error
}

func (m *mockClient) Do(req *http.Request) (*http.Response, error) {
    return m.response, m.err
}
```

### Avoid Over-Mocking
- Mock external services (HTTP, databases)
- Don't mock internal components unless necessary
- Prefer integration tests over heavily mocked unit tests

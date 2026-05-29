// proxy_test.go
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProxyBasicRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("expected path /v1/messages, got %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}

		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"test":"data"}` {
			t.Errorf("unexpected body: %s", body)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"response":"ok"}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	proxy := NewProxy()

	reqPath := "/anthropic/" + upstreamHost + "/v1/messages"
	req := httptest.NewRequest("POST", reqPath, strings.NewReader(`{"test":"data"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "sk-ant-test-key")

	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if w.Body.String() != `{"response":"ok"}` {
		t.Errorf("unexpected response: %s", w.Body.String())
	}
}

func TestProxyForwardsHeaders(t *testing.T) {
	var receivedHeaders http.Header

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")
	proxy := NewProxy()

	reqPath := "/anthropic/" + upstreamHost + "/v1/messages"
	req := httptest.NewRequest("POST", reqPath, nil)
	req.Header.Set("X-Api-Key", "sk-ant-test-key")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "messages-2024-01-01")

	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if receivedHeaders.Get("X-Api-Key") != "sk-ant-test-key" {
		t.Error("X-Api-Key header not forwarded")
	}
	if receivedHeaders.Get("Anthropic-Version") != "2023-06-01" {
		t.Error("Anthropic-Version header not forwarded")
	}
}

func TestProxyLogsRequests(t *testing.T) {
	tmpDir := t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":"logged"}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	logger, _ := NewLogger(tmpDir)
	defer logger.Close()

	proxy := NewProxyWithLogger(logger)

	reqPath := "/anthropic/" + upstreamHost + "/v1/messages"
	req := httptest.NewRequest("POST", reqPath, strings.NewReader(`{"messages":[]}`))
	req.Header.Set("X-Api-Key", "sk-ant-test123456")

	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	// Give async logging a moment
	time.Sleep(50 * time.Millisecond)

	// Check that log file was created - new path: <upstream>/<date>/*.jsonl
	today := time.Now().Format("2006-01-02")
	files, _ := filepath.Glob(filepath.Join(tmpDir, upstreamHost, today, "*.jsonl"))
	if len(files) == 0 {
		t.Error("Expected log file to be created")
	}

	// Read and verify content
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), `"type":"request"`) {
		t.Error("Log should contain request entry")
	}
	if !strings.Contains(string(data), `"type":"response"`) {
		t.Error("Log should contain response entry")
	}
}

// TestProxyDecompressesGzipResponseForLogging verifies that when an upstream
// returns a gzip-encoded response, the proxy decompresses it before writing
// to the JSONL log so the response body is readable JSON we can jq.
// Regression test for PRI-1800.
func TestProxyDecompressesGzipResponseForLogging(t *testing.T) {
	tmpDir := t.TempDir()

	responseJSON := `{"id":"msg_01","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":42,"output_tokens":7,"cache_creation_input_tokens":100,"cache_read_input_tokens":50}}`

	// Build a gzip-encoded body
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write([]byte(responseJSON)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	gzipped := gzBuf.Bytes()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		w.Write(gzipped)
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	logger, _ := NewLogger(tmpDir)
	defer logger.Close()

	proxy := NewProxyWithLogger(logger)

	reqPath := "/anthropic/" + upstreamHost + "/v1/messages"
	req := httptest.NewRequest("POST", reqPath, strings.NewReader(`{"messages":[],"model":"claude-sonnet-4"}`))
	req.Header.Set("X-Api-Key", "sk-ant-test123456")
	req.Header.Set("Accept-Encoding", "gzip")

	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	// Give async logging a moment
	time.Sleep(50 * time.Millisecond)

	today := time.Now().Format("2006-01-02")
	files, _ := filepath.Glob(filepath.Join(tmpDir, upstreamHost, today, "*.jsonl"))
	if len(files) == 0 {
		t.Fatal("expected log file to be created")
	}

	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}

	// Find the response line and assert its body is parseable JSON
	// with the expected usage field.
	var foundResponse bool
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
		}
		if entry["type"] != "response" {
			continue
		}
		foundResponse = true

		bodyStr, ok := entry["body"].(string)
		if !ok {
			t.Fatalf("response entry missing string body field: %v", entry)
		}

		// The body field must be parseable JSON (not corrupted gzip bytes)
		var bodyObj map[string]interface{}
		if err := json.Unmarshal([]byte(bodyStr), &bodyObj); err != nil {
			t.Fatalf("logged response body is not valid JSON: %v\nbody (hex prefix): %x", err, []byte(bodyStr)[:min(32, len(bodyStr))])
		}

		usage, ok := bodyObj["usage"].(map[string]interface{})
		if !ok {
			t.Fatalf("logged response body missing usage field: %v", bodyObj)
		}
		if got := usage["cache_creation_input_tokens"]; got != float64(100) {
			t.Errorf("cache_creation_input_tokens = %v, want 100", got)
		}
		if got := usage["cache_read_input_tokens"]; got != float64(50) {
			t.Errorf("cache_read_input_tokens = %v, want 50", got)
		}
		if got := usage["input_tokens"]; got != float64(42) {
			t.Errorf("input_tokens = %v, want 42", got)
		}
	}
	if !foundResponse {
		t.Fatal("did not find response entry in log")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestIsJWTAuth(t *testing.T) {
	tests := []struct {
		name     string
		auth     string
		expected bool
	}{
		{"empty", "", false},
		{"api key", "Bearer sk-abc123xyz", false},
		{"api key proj", "Bearer sk-proj-abc123xyz", false},
		{"jwt token", "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c", true},
		{"no bearer prefix", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig", false},
		{"two parts only", "Bearer abc.def", false},
		{"four parts", "Bearer a.b.c.d", false},
		{"empty part", "Bearer abc..def", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := http.Header{}
			if tt.auth != "" {
				headers.Set("Authorization", tt.auth)
			}
			got := isJWTAuth(headers)
			if got != tt.expected {
				t.Errorf("isJWTAuth(%q) = %v, want %v", tt.auth, got, tt.expected)
			}
		})
	}
}

func TestIsConversationEndpoint(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		// Anthropic
		{"/v1/messages", true},

		// OpenAI conversation endpoints
		{"/v1/chat/completions", true},
		{"/v1/responses", true},
		{"/v1/completions", true},
		{"/v1/threads/thread_abc123/messages", true},
		{"/v1/threads/thread_abc123/runs", true},
		{"/v1/threads/thread_abc123/runs/run_xyz/steps", true},

		// ChatGPT backend API (OAuth authentication)
		{"/backend-api/codex/v1/responses", true},
		{"/backend-api/responses", true},
		{"/backend-api/v1/responses", true},

		// Non-conversation endpoints (should NOT log)
		{"/v1/messages/count_tokens", false},
		{"/v1/models", false},
		{"/v1/embeddings", false},
		{"/v1/images/generations", false},
		{"/v1/audio/transcriptions", false},
		{"/v1/files", false},
		{"/v1/threads", false},       // Creating thread, not a conversation
		{"/v1/conversations", false}, // CRUD operations only
		{"/v1/assistants", false},
		{"/v1/vector_stores", false},
		{"/backend-api/codex/v1/models", false}, // Non-conversation ChatGPT backend endpoint
	}

	for _, tt := range tests {
		got := isConversationEndpoint(tt.path)
		if got != tt.expected {
			t.Errorf("isConversationEndpoint(%q) = %v, want %v", tt.path, got, tt.expected)
		}
	}
}

func proxyWithSub(t *testing.T, scriptBody string) *Proxy {
	t.Helper()
	p := NewProxy()
	p.tokenSub = newSub(t, writeScript(t, scriptBody))
	return p
}

func TestSubstitutionReplacesKeyOnEveryEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("X-Api-Key")))
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")
	p := proxyWithSub(t, `read -r _; echo REAL-KEY`)
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/models"} {
		req := httptest.NewRequest("POST", "/anthropic/"+host+path, strings.NewReader("{}"))
		req.Header.Set("X-Api-Key", "nonce-not-real")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if got := rec.Body.String(); got != "REAL-KEY" {
			t.Errorf("%s: upstream saw key %q, want REAL-KEY", path, got)
		}
	}
}

func TestSubstitutionStripsBothAuthHeaders(t *testing.T) {
	var seenAuth, seenKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenKey = r.Header.Get("X-Api-Key")
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")
	p := proxyWithSub(t, `read -r _; echo REAL-KEY`)
	req := httptest.NewRequest("POST", "/anthropic/"+host+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("X-Api-Key", "nonce")
	req.Header.Set("Authorization", "Bearer nonce")
	p.ServeHTTP(httptest.NewRecorder(), req)
	if seenKey != "REAL-KEY" || seenAuth != "" {
		t.Errorf("x-api-key=%q authorization=%q; want REAL-KEY and empty", seenKey, seenAuth)
	}
}

func TestSubstitutionFailClosedNoUpstream(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")
	p := proxyWithSub(t, `exit 1`)
	req := httptest.NewRequest("POST", "/anthropic/"+host+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("X-Api-Key", "nonce")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("status=%d, want 401", rec.Code)
	}
	if called {
		t.Error("upstream was contacted on a fail-closed resolution")
	}
}

func TestSubstitutionDisabledIsPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("X-Api-Key")))
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")
	p := NewProxy() // tokenSub nil
	req := httptest.NewRequest("POST", "/anthropic/"+host+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("X-Api-Key", "client-key-verbatim")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if got := rec.Body.String(); got != "client-key-verbatim" {
		t.Errorf("passthrough changed the key: %q", got)
	}
}

// TestSubstitutionResolvedKeyNeverLogged is the FR6 regression test: the resolved
// (real) API key must never appear in log files or in error strings returned to the
// client, regardless of whether substitution succeeds or fails.
func TestSubstitutionResolvedKeyNeverLogged(t *testing.T) {
	const resolvedKey = "SECRET-REAL-KEY-9f3a"

	// --- success path: resolved key must not appear in JSONL logs ---
	t.Run("not in logs on success", func(t *testing.T) {
		tmpDir := t.TempDir()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"type":"message","role":"assistant","content":[]}`))
		}))
		defer upstream.Close()
		host := strings.TrimPrefix(upstream.URL, "http://")

		logger, err := NewLogger(tmpDir)
		if err != nil {
			t.Fatalf("NewLogger: %v", err)
		}
		defer logger.Close()

		p := NewProxyWithLogger(logger)
		p.tokenSub = newSub(t, writeScript(t, "read -r _; echo "+resolvedKey))

		req := httptest.NewRequest("POST", "/anthropic/"+host+"/v1/messages",
			strings.NewReader(`{"model":"claude-x","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "inbound-nonce-zzz")

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		// Flush and close logger before reading files.
		logger.Close()

		// Walk every .jsonl file under tmpDir and assert the resolved key is absent.
		var logFiles []string
		filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && filepath.Ext(path) == ".jsonl" {
				logFiles = append(logFiles, path)
			}
			return nil
		})
		if len(logFiles) == 0 {
			t.Fatal("no .jsonl log files were created — check logging setup")
		}
		for _, f := range logFiles {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("reading %s: %v", f, err)
			}
			if strings.Contains(string(data), resolvedKey) {
				t.Errorf("SECURITY: resolved key %q found in log file %s", resolvedKey, f)
			}
		}
	})

	// --- fail-closed path: resolved key must not appear in the error response ---
	t.Run("not in error response on failure", func(t *testing.T) {
		called := false
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		}))
		defer upstream.Close()
		host := strings.TrimPrefix(upstream.URL, "http://")

		p := NewProxy()
		p.tokenSub = newSub(t, writeScript(t, "exit 1"))

		req := httptest.NewRequest("POST", "/anthropic/"+host+"/v1/messages", strings.NewReader("{}"))
		req.Header.Set("X-Api-Key", "inbound-nonce-zzz")

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		if called {
			t.Error("upstream was contacted on a fail-closed resolution failure")
		}
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 on resolver failure, got %d", rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, resolvedKey) {
			t.Errorf("SECURITY: resolved key %q found in error response: %s", resolvedKey, body)
		}
		// Confirm the fixed error string is what the client sees (not an internal error that
		// might inadvertently include key material in a future refactor).
		if !strings.Contains(body, "api token substitution failed") {
			t.Errorf("expected fixed error string, got: %s", body)
		}
	})
}

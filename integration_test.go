// integration_test.go
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProxySessionTracking(t *testing.T) {
	tmpDir := t.TempDir()

	// Track requests received by upstream
	var receivedRequests []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedRequests = append(receivedRequests, string(body))

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_123","content":[{"type":"text","text":"response"}]}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// First request
	body1 := `{"messages":[{"role":"user","content":"hello"}]}`
	req1 := httptest.NewRequest("POST", "/anthropic/"+upstreamHost+"/v1/messages", strings.NewReader(body1))
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, req1)

	// Second request (continuation)
	body2 := `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"response"},{"role":"user","content":"how are you"}]}`
	req2 := httptest.NewRequest("POST", "/anthropic/"+upstreamHost+"/v1/messages", strings.NewReader(body2))
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)

	// Give logger time
	time.Sleep(100 * time.Millisecond)

	// Check that we have session files - new path: <upstream>/<date>/*.jsonl
	today := time.Now().Format("2006-01-02")
	logFiles, _ := filepath.Glob(filepath.Join(tmpDir, upstreamHost, today, "*.jsonl"))
	if len(logFiles) == 0 {
		t.Fatal("Expected at least one log file")
	}

	// Verify sessions.db exists
	dbPath := filepath.Join(tmpDir, "sessions.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("sessions.db should exist")
	}
}

func TestProxySessionContinuationWithClientSessionID(t *testing.T) {
	tmpDir := t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_123","content":[{"type":"text","text":"hi there"}]}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// First request - new session with client session ID (Anthropic format)
	body1 := `{"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"user_abc_session_test-session-456"}}`
	req1 := httptest.NewRequest("POST", "/anthropic/"+upstreamHost+"/v1/messages", strings.NewReader(body1))
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, req1)

	if w1.Code != 200 {
		t.Fatalf("Expected 200, got %d: %s", w1.Code, w1.Body.String())
	}

	// Give logger time to flush
	time.Sleep(50 * time.Millisecond)

	// Second request with same client session ID - should continue same session
	body2 := `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi there"}]},{"role":"user","content":"how are you"}],"metadata":{"user_id":"user_abc_session_test-session-456"}}`
	req2 := httptest.NewRequest("POST", "/anthropic/"+upstreamHost+"/v1/messages", strings.NewReader(body2))
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)

	if w2.Code != 200 {
		t.Fatalf("Expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	// Wait for logging
	time.Sleep(50 * time.Millisecond)

	// Check log files - should have exactly 1 session file (same client session ID)
	today := time.Now().Format("2006-01-02")
	logFiles, err := filepath.Glob(filepath.Join(tmpDir, upstreamHost, today, "*.jsonl"))
	if err != nil {
		t.Fatalf("Failed to glob: %v", err)
	}
	if len(logFiles) != 1 {
		t.Errorf("Expected 1 log file (same session), got %d", len(logFiles))
	}
}

func TestProxyNoClientSessionIDCreatesNewSessions(t *testing.T) {
	tmpDir := t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_123","content":[{"type":"text","text":"hi there"}]}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// First request without client session ID
	body1 := `{"messages":[{"role":"user","content":"hello"}]}`
	req1 := httptest.NewRequest("POST", "/anthropic/"+upstreamHost+"/v1/messages", strings.NewReader(body1))
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, req1)

	if w1.Code != 200 {
		t.Fatalf("Expected 200, got %d: %s", w1.Code, w1.Body.String())
	}

	// Give logger time to flush
	time.Sleep(50 * time.Millisecond)

	// Second request also without client session ID - should NOT be merged
	body2 := `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi there"},{"role":"user","content":"how are you"}]}`
	req2 := httptest.NewRequest("POST", "/anthropic/"+upstreamHost+"/v1/messages", strings.NewReader(body2))
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)

	if w2.Code != 200 {
		t.Fatalf("Expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	// Wait for logging
	time.Sleep(50 * time.Millisecond)

	// Check log files - should have 2 separate session files (no merging without client session ID)
	today := time.Now().Format("2006-01-02")
	logFiles, err := filepath.Glob(filepath.Join(tmpDir, upstreamHost, today, "*.jsonl"))
	if err != nil {
		t.Fatalf("Failed to glob: %v", err)
	}
	if len(logFiles) != 2 {
		t.Errorf("Expected 2 log files (separate sessions without client ID), got %d", len(logFiles))
	}
}

func TestProxyNonConversationEndpointSkipsLogging(t *testing.T) {
	tmpDir := t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"count":100}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// Token counting endpoint - not a conversation, should not be logged
	req := httptest.NewRequest("POST", "/anthropic/"+upstreamHost+"/v1/messages/count_tokens", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	// Should NOT create log files for non-conversation endpoints
	time.Sleep(50 * time.Millisecond)
	today := time.Now().Format("2006-01-02")
	logFiles, _ := filepath.Glob(filepath.Join(tmpDir, upstreamHost, today, "*.jsonl"))
	if len(logFiles) != 0 {
		t.Errorf("Expected no log files for non-conversation endpoints, got %d", len(logFiles))
	}
}

func TestProxyOpenAIChatCompletionsLogging(t *testing.T) {
	tmpDir := t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello!"}}]}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// OpenAI Chat Completions request
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"user":"test-user-123"}`
	req := httptest.NewRequest("POST", "/openai/"+upstreamHost+"/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("Expected 200, got %d: %s", w.Code, w.Body.String())
	}

	time.Sleep(50 * time.Millisecond)

	// Should create log file - new path: <upstream>/<date>/*.jsonl
	today := time.Now().Format("2006-01-02")
	logFiles, _ := filepath.Glob(filepath.Join(tmpDir, upstreamHost, today, "*.jsonl"))
	if len(logFiles) != 1 {
		t.Errorf("Expected 1 log file, got %d", len(logFiles))
	}
}

func TestProxyOpenAIResponsesAPILogging(t *testing.T) {
	tmpDir := t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_abc123","output":[{"content":"Hi!"}]}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// OpenAI Responses API request with conversation field
	body := `{"model":"gpt-4o","input":[{"role":"user","content":"hello"}],"conversation":"conv_test123"}`
	req := httptest.NewRequest("POST", "/openai/"+upstreamHost+"/v1/responses", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("Expected 200, got %d: %s", w.Code, w.Body.String())
	}

	time.Sleep(50 * time.Millisecond)

	today := time.Now().Format("2006-01-02")
	logFiles, _ := filepath.Glob(filepath.Join(tmpDir, upstreamHost, today, "*.jsonl"))
	if len(logFiles) != 1 {
		t.Errorf("Expected 1 log file, got %d", len(logFiles))
	}
}

func TestProxyOpenAISessionContinuation(t *testing.T) {
	tmpDir := t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"response"}}]}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// First request
	body1 := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"user":"session-test"}`
	req1 := httptest.NewRequest("POST", "/openai/"+upstreamHost+"/v1/chat/completions", strings.NewReader(body1))
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, req1)

	time.Sleep(50 * time.Millisecond)

	// Second request (same user = same session)
	body2 := `{"model":"gpt-4o","messages":[{"role":"user","content":"followup"}],"user":"session-test"}`
	req2 := httptest.NewRequest("POST", "/openai/"+upstreamHost+"/v1/chat/completions", strings.NewReader(body2))
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)

	time.Sleep(50 * time.Millisecond)

	// Should have 1 log file (same session) - new path: <upstream>/<date>/*.jsonl
	today := time.Now().Format("2006-01-02")
	logFiles, _ := filepath.Glob(filepath.Join(tmpDir, upstreamHost, today, "*.jsonl"))
	if len(logFiles) != 1 {
		t.Errorf("Expected 1 log file (same session), got %d", len(logFiles))
	}
}

func TestProxyOpenAIEmbeddingsSkipped(t *testing.T) {
	tmpDir := t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"embedding":[0.1,0.2]}]}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// Embeddings request - should NOT be logged
	body := `{"model":"text-embedding-ada-002","input":"hello"}`
	req := httptest.NewRequest("POST", "/openai/"+upstreamHost+"/v1/embeddings", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	time.Sleep(50 * time.Millisecond)

	// Should NOT create log files - new path: <upstream>/<date>/*.jsonl
	today := time.Now().Format("2006-01-02")
	logFiles, _ := filepath.Glob(filepath.Join(tmpDir, upstreamHost, today, "*.jsonl"))
	if len(logFiles) != 0 {
		t.Errorf("Expected no log files for embeddings, got %d", len(logFiles))
	}
}

func TestProxyOpenAIHeaderSessionID(t *testing.T) {
	tmpDir := t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hi"}}]}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// Request with X-Session-ID header
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/openai/"+upstreamHost+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Session-ID", "header-sess-abc")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	time.Sleep(50 * time.Millisecond)

	// Second request with same header session ID
	req2 := httptest.NewRequest("POST", "/openai/"+upstreamHost+"/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("X-Session-ID", "header-sess-abc")
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)

	time.Sleep(50 * time.Millisecond)

	// Should have 1 log file (same session via header) - new path: <upstream>/<date>/*.jsonl
	today := time.Now().Format("2006-01-02")
	logFiles, _ := filepath.Glob(filepath.Join(tmpDir, upstreamHost, today, "*.jsonl"))
	if len(logFiles) != 1 {
		t.Errorf("Expected 1 log file (same session via header), got %d", len(logFiles))
	}
}

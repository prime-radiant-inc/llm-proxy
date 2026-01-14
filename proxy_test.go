// proxy_test.go
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

	// Check that log file was created
	files, _ := filepath.Glob(filepath.Join(tmpDir, "anthropic", "*.jsonl"))
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

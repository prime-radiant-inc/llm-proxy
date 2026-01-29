package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthEndpoint(t *testing.T) {
	tmpDir := t.TempDir()
	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", w.Body.String())
	}
}

func TestServerProxiesRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_123"}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	tmpDir := t.TempDir()
	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	reqPath := "/anthropic/" + upstreamHost + "/v1/messages"
	req := httptest.NewRequest("POST", reqPath, strings.NewReader(`{"messages":[]}`))
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestNewServer_LokiDisabled verifies that when Loki is disabled, no LokiExporter is created
func TestNewServer_LokiDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Config{
		Port:   8080,
		LogDir: tmpDir,
		Loki: LokiConfig{
			Enabled: false,
			URL:     "http://loki:3100/loki/api/v1/push",
		},
	}

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// LokiExporter should be nil when disabled
	if srv.lokiExporter != nil {
		t.Error("expected lokiExporter to be nil when Loki is disabled")
	}

	// File logger should still work
	if srv.fileLogger == nil {
		t.Error("expected fileLogger to be created")
	}
}

// TestNewServer_LokiEnabled verifies that when Loki is enabled with valid URL, LokiExporter is created
func TestNewServer_LokiEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Config{
		Port:   8080,
		LogDir: tmpDir,
		Loki: LokiConfig{
			Enabled:      true,
			URL:          "http://loki:3100/loki/api/v1/push",
			BatchSize:    100,
			BatchWaitStr: "1s",
			RetryMax:     3,
			UseGzip:      true,
			Environment:  "test",
		},
	}

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// LokiExporter should be created when enabled
	if srv.lokiExporter == nil {
		t.Error("expected lokiExporter to be created when Loki is enabled")
	}

	// File logger should still work
	if srv.fileLogger == nil {
		t.Error("expected fileLogger to be created")
	}
}

// TestNewServer_LokiInvalidURL verifies graceful degradation when Loki URL is empty
func TestNewServer_LokiInvalidURL(t *testing.T) {
	tmpDir := t.TempDir()

	// Capture log output
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(nil)

	cfg := Config{
		Port:   8080,
		LogDir: tmpDir,
		Loki: LokiConfig{
			Enabled: true,
			URL:     "", // Invalid - empty URL
		},
	}

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("Server creation should succeed with invalid Loki config: %v", err)
	}
	defer srv.Close()

	// LokiExporter should be nil due to invalid URL
	if srv.lokiExporter != nil {
		t.Error("expected lokiExporter to be nil with invalid URL")
	}

	// Warning should be logged
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "WARNING") && !strings.Contains(logOutput, "Loki") {
		t.Errorf("expected warning about Loki failure, got: %s", logOutput)
	}

	// File logger should still work (graceful degradation)
	if srv.fileLogger == nil {
		t.Error("expected fileLogger to be created despite Loki failure")
	}
}

// TestHealthLoki_Disabled verifies /health/loki returns disabled status when Loki is not configured
func TestHealthLoki_Disabled(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Config{
		Port:   8080,
		LogDir: tmpDir,
		Loki: LokiConfig{
			Enabled: false,
		},
	}

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	req := httptest.NewRequest("GET", "/health/loki", nil)
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse JSON response: %v", err)
	}

	if response["status"] != "disabled" {
		t.Errorf("expected status 'disabled', got %q", response["status"])
	}
}

// TestNewServer_EventEmitterWiredUp verifies that when Loki is enabled, the proxy has an event emitter
func TestNewServer_EventEmitterWiredUp(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Config{
		Port:   8080,
		LogDir: tmpDir,
		Loki: LokiConfig{
			Enabled:      true,
			URL:          "http://loki:3100/loki/api/v1/push",
			BatchSize:    100,
			BatchWaitStr: "1s",
			RetryMax:     3,
			UseGzip:      true,
			Environment:  "test",
		},
	}

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// The proxy should have an event emitter when Loki is enabled
	if srv.proxy.eventEmitter == nil {
		t.Error("expected proxy to have eventEmitter when Loki is enabled")
	}

	// The proxy should have a machineID set
	if srv.proxy.machineID == "" {
		t.Error("expected proxy to have machineID set when event emitter is configured")
	}
}

// TestNewServer_EventEmitterNilWhenLokiDisabled verifies no event emitter when Loki is disabled
func TestNewServer_EventEmitterNilWhenLokiDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Config{
		Port:   8080,
		LogDir: tmpDir,
		Loki: LokiConfig{
			Enabled: false,
		},
	}

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	// The proxy should NOT have an event emitter when Loki is disabled
	if srv.proxy.eventEmitter != nil {
		t.Error("expected proxy to NOT have eventEmitter when Loki is disabled")
	}
}

// TestHealthLoki_Enabled verifies /health/loki returns stats when Loki is configured
func TestHealthLoki_Enabled(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Config{
		Port:   8080,
		LogDir: tmpDir,
		Loki: LokiConfig{
			Enabled:      true,
			URL:          "http://loki:3100/loki/api/v1/push",
			BatchSize:    100,
			BatchWaitStr: "1s",
			RetryMax:     3,
			UseGzip:      true,
			Environment:  "test",
		},
	}

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	req := httptest.NewRequest("GET", "/health/loki", nil)
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse JSON response: %v", err)
	}

	// Should have status "ok" when Loki is enabled
	if response["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", response["status"])
	}

	// Should include stats
	if _, ok := response["entries_sent"]; !ok {
		t.Error("expected entries_sent in response")
	}
	if _, ok := response["entries_dropped"]; !ok {
		t.Error("expected entries_dropped in response")
	}
}

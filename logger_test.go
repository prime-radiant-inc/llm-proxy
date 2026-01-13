// logger_test.go
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoggerWritesJSONL(t *testing.T) {
	tmpDir := t.TempDir()

	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close()

	sessionID := "20260113-102345-a7f3"
	provider := "anthropic"

	// Log a session start
	err = logger.LogSessionStart(sessionID, provider, "api.anthropic.com")
	if err != nil {
		t.Fatalf("Failed to log session start: %v", err)
	}

	// Log a request
	headers := http.Header{"X-Api-Key": []string{"sk-ant-secret123456"}}
	err = logger.LogRequest(sessionID, provider, 1, "POST", "/v1/messages", headers, []byte(`{"test":"data"}`))
	if err != nil {
		t.Fatalf("Failed to log request: %v", err)
	}

	// Verify file was created
	logPath := filepath.Join(tmpDir, provider, sessionID+".jsonl")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("Expected 2 lines, got %d", len(lines))
	}

	// Verify session_start entry
	var startEntry map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &startEntry); err != nil {
		t.Fatalf("Failed to parse session_start: %v", err)
	}
	if startEntry["type"] != "session_start" {
		t.Errorf("Expected type session_start, got %v", startEntry["type"])
	}

	// Verify request entry
	var reqEntry map[string]interface{}
	if err := json.Unmarshal([]byte(lines[1]), &reqEntry); err != nil {
		t.Fatalf("Failed to parse request: %v", err)
	}
	if reqEntry["type"] != "request" {
		t.Errorf("Expected type request, got %v", reqEntry["type"])
	}

	// Verify API key was obfuscated
	reqHeaders := reqEntry["headers"].(map[string]interface{})
	apiKey := reqHeaders["X-Api-Key"].([]interface{})[0].(string)
	if strings.Contains(apiKey, "secret") {
		t.Error("API key was not obfuscated in log")
	}
}

func TestLoggerResponseWithTiming(t *testing.T) {
	tmpDir := t.TempDir()

	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close()

	sessionID := "20260113-102345-test"
	provider := "anthropic"

	logger.LogSessionStart(sessionID, provider, "api.anthropic.com")

	timing := ResponseTiming{
		TTFBMs:  150,
		TotalMs: 1200,
	}

	err = logger.LogResponse(sessionID, provider, 1, 200, http.Header{}, []byte(`{"response":"ok"}`), nil, timing)
	if err != nil {
		t.Fatalf("Failed to log response: %v", err)
	}

	// Read and verify
	logPath := filepath.Join(tmpDir, provider, sessionID+".jsonl")
	data, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	var respEntry map[string]interface{}
	json.Unmarshal([]byte(lines[1]), &respEntry)

	timingData := respEntry["timing"].(map[string]interface{})
	if timingData["ttfb_ms"].(float64) != 150 {
		t.Errorf("TTFB not logged correctly")
	}
}

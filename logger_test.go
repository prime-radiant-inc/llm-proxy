// logger_test.go
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	upstream := "api.anthropic.com"

	// Log a session start
	err = logger.LogSessionStart(sessionID, provider, upstream)
	if err != nil {
		t.Fatalf("Failed to log session start: %v", err)
	}

	// Log a request
	headers := http.Header{"X-Api-Key": []string{"sk-ant-secret123456"}}
	err = logger.LogRequest(sessionID, provider, 1, "POST", "/v1/messages", headers, []byte(`{"test":"data"}`), "test-request-id")
	if err != nil {
		t.Fatalf("Failed to log request: %v", err)
	}

	// Verify file was created - new path structure: <upstream>/<date>/<sessionID>.jsonl
	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(tmpDir, upstream, today, sessionID+".jsonl")
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

func TestLogPathStructure(t *testing.T) {
	tmpDir := t.TempDir()
	logger, _ := NewLogger(tmpDir)
	defer logger.Close()

	sessionID := "20260114-091523-abcd1234"
	upstream := "api.anthropic.com"

	logger.LogSessionStart(sessionID, "anthropic", upstream)

	// Wait for async write
	time.Sleep(50 * time.Millisecond)

	// Expect: tmpDir/api.anthropic.com/2026-01-14/20260114-091523-abcd1234.jsonl
	today := time.Now().Format("2006-01-02")
	expectedPath := filepath.Join(tmpDir, upstream, today, sessionID+".jsonl")

	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Errorf("Expected log at %s", expectedPath)
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
	upstream := "api.anthropic.com"

	logger.LogSessionStart(sessionID, provider, upstream)

	timing := ResponseTiming{
		TTFBMs:  150,
		TotalMs: 1200,
	}

	err = logger.LogResponse(sessionID, provider, 1, 200, http.Header{}, []byte(`{"response":"ok"}`), nil, timing, "test-request-id")
	if err != nil {
		t.Fatalf("Failed to log response: %v", err)
	}

	// Read and verify - new path structure: <upstream>/<date>/<sessionID>.jsonl
	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(tmpDir, upstream, today, sessionID+".jsonl")
	data, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	var respEntry map[string]interface{}
	json.Unmarshal([]byte(lines[1]), &respEntry)

	timingData := respEntry["timing"].(map[string]interface{})
	if timingData["ttfb_ms"].(float64) != 150 {
		t.Errorf("TTFB not logged correctly")
	}
}

func TestLogEntryHasMeta(t *testing.T) {
	tmpDir := t.TempDir()

	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close()

	sessionID := "test-session-meta"
	upstream := "api.anthropic.com"

	logger.LogSessionStart(sessionID, "anthropic", upstream)
	logger.LogRequest(sessionID, "anthropic", 1, "POST", "/v1/messages", nil, []byte(`{}`), "test-request-id")

	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(tmpDir, upstream, today, sessionID+".jsonl")
	data, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	// Check request entry has _meta
	var reqEntry map[string]interface{}
	json.Unmarshal([]byte(lines[1]), &reqEntry)

	meta, ok := reqEntry["_meta"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected _meta block in log entry")
	}

	// Verify required fields
	if _, ok := meta["ts"]; !ok {
		t.Error("_meta missing ts field")
	}
	if _, ok := meta["machine"]; !ok {
		t.Error("_meta missing machine field")
	}
	if _, ok := meta["host"]; !ok {
		t.Error("_meta missing host field")
	}
	if _, ok := meta["session"]; !ok {
		t.Error("_meta missing session field")
	}
	if _, ok := meta["request_id"]; !ok {
		t.Error("_meta missing request_id field")
	}

	// Verify machine format is user@host
	machine := meta["machine"].(string)
	if !strings.Contains(machine, "@") {
		t.Errorf("Expected machine format user@host, got %s", machine)
	}
}

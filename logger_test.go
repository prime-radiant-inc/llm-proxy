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

func TestLoggerLogObservation(t *testing.T) {
	tmpDir := t.TempDir()

	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close()

	sessionID := "mantle-session-123"
	provider := "openai"
	upstream := "bedrock-mantle.us-west-2.api.aws"
	logger.RegisterUpstream(sessionID, upstream)

	entry := map[string]any{
		"type": "request",
		"_meta": map[string]any{
			"schema_version":     "telemetry-contract-v0",
			"cloud_build_run_id": "run-test",
			"provider":           provider,
			"provider_route":     "bedrock-mantle",
			"wire_api":           "openai-responses",
		},
		"request": map[string]any{
			"ingress_path":  "/cbrun/run-test/mantle/v1/responses",
			"raw_query":     "trace=1",
			"upstream_host": "bedrock-mantle.us-west-2.api.aws",
		},
	}

	if err := logger.LogObservation(sessionID, provider, entry); err != nil {
		t.Fatalf("Failed to log observation: %v", err)
	}

	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(tmpDir, upstream, today, sessionID+".jsonl")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("Expected 1 line, got %d", len(lines))
	}

	var logged map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &logged); err != nil {
		t.Fatalf("Failed to parse observation entry: %v", err)
	}

	request := logged["request"].(map[string]any)
	if got := request["ingress_path"]; got != "/cbrun/run-test/mantle/v1/responses" {
		t.Fatalf("request.ingress_path = %v, want /cbrun/run-test/mantle/v1/responses", got)
	}
	if got := request["raw_query"]; got != "trace=1" {
		t.Fatalf("request.raw_query = %v, want trace=1", got)
	}
	if got := request["upstream_host"]; got != "bedrock-mantle.us-west-2.api.aws" {
		t.Fatalf("request.upstream_host = %v, want bedrock-mantle.us-west-2.api.aws", got)
	}

	meta := logged["_meta"].(map[string]any)
	if got := meta["cloud_build_run_id"]; got != "run-test" {
		t.Fatalf("_meta.cloud_build_run_id = %v, want run-test", got)
	}
	if got := meta["provider_route"]; got != "bedrock-mantle" {
		t.Fatalf("_meta.provider_route = %v, want bedrock-mantle", got)
	}
	if got := meta["wire_api"]; got != "openai-responses" {
		t.Fatalf("_meta.wire_api = %v, want openai-responses", got)
	}
}

func TestRequestLogContextWritesNeutralRunMetadata(t *testing.T) {
	meta := map[string]interface{}{}
	addRequestLogContextMeta(meta, RequestLogContext{
		RunID:         "proj-20260628-run",
		ResolvedRunID: "resolved-other-run-id",
		ClientFPHash:  strings.Repeat("a", 64),
		Project:       "proj",
		ProviderRoute: "aws-bedrock",
		WireAPI:       "messages",
	})

	if got := meta["run_id"]; got != "proj-20260628-run" {
		t.Fatalf("run_id = %v, want proj-20260628-run", got)
	}
	if got := meta["resolved_run_id"]; got != "resolved-other-run-id" {
		t.Fatalf("resolved_run_id = %v, want resolved-other-run-id", got)
	}
	if _, ok := meta["cloud_build_run_id"]; ok {
		t.Fatalf("legacy cloud_build_run_id present in neutral context: %#v", meta)
	}
}

func TestRequestLogContextWritesLegacyRunMetadata(t *testing.T) {
	meta := map[string]interface{}{}
	addRequestLogContextMeta(meta, RequestLogContext{
		LegacyRunID: "legacy-run",
	})

	if got := meta["cloud_build_run_id"]; got != "legacy-run" {
		t.Fatalf("cloud_build_run_id = %v, want legacy-run", got)
	}
	if _, ok := meta["run_id"]; ok {
		t.Fatalf("run_id unexpectedly present in legacy context: %#v", meta)
	}
	if _, ok := meta["resolved_run_id"]; ok {
		t.Fatalf("resolved_run_id unexpectedly present in legacy context: %#v", meta)
	}
}

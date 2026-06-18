// multi_writer_test.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

// mockFileLogger is a test double for *Logger that tracks method calls
type mockFileLogger struct {
	mu                    sync.Mutex
	registerUpstreamCalls []registerUpstreamCall
	sessionStartCalls     []sessionStartCall
	requestCalls          []requestCall
	responseCalls         []responseCall
	observationCalls      []observationCall
	forkCalls             []forkCall
	closeCalls            int
	closeError            error

	// Configurable errors for testing error propagation
	sessionStartError error
	requestError      error
	responseError     error
	observationError  error
	forkError         error
}

type registerUpstreamCall struct {
	sessionID string
	upstream  string
}

type sessionStartCall struct {
	sessionID string
	provider  string
	upstream  string
}

type requestCall struct {
	sessionID string
	provider  string
	seq       int
	method    string
	path      string
	headers   http.Header
	body      []byte
	requestID string
}

type responseCall struct {
	sessionID string
	provider  string
	seq       int
	status    int
	headers   http.Header
	body      []byte
	chunks    []StreamChunk
	timing    ResponseTiming
	requestID string
}

type observationCall struct {
	sessionID string
	provider  string
	entry     map[string]interface{}
}

type forkCall struct {
	sessionID     string
	provider      string
	fromSeq       int
	parentSession string
}

func newMockFileLogger() *mockFileLogger {
	return &mockFileLogger{}
}

func (m *mockFileLogger) RegisterUpstream(sessionID, upstream string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registerUpstreamCalls = append(m.registerUpstreamCalls, registerUpstreamCall{sessionID, upstream})
}

func (m *mockFileLogger) LogSessionStart(sessionID, provider, upstream string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionStartCalls = append(m.sessionStartCalls, sessionStartCall{sessionID, provider, upstream})
	return m.sessionStartError
}

func (m *mockFileLogger) LogRequest(sessionID, provider string, seq int, method, path string, headers http.Header, body []byte, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requestCalls = append(m.requestCalls, requestCall{sessionID, provider, seq, method, path, headers, body, requestID})
	return m.requestError
}

func (m *mockFileLogger) LogResponse(sessionID, provider string, seq int, status int, headers http.Header, body []byte, chunks []StreamChunk, timing ResponseTiming, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responseCalls = append(m.responseCalls, responseCall{sessionID, provider, seq, status, headers, body, chunks, timing, requestID})
	return m.responseError
}

func (m *mockFileLogger) LogObservation(sessionID, provider string, entry map[string]interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observationCalls = append(m.observationCalls, observationCall{sessionID, provider, entry})
	return m.observationError
}

func (m *mockFileLogger) LogFork(sessionID, provider string, fromSeq int, parentSession string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forkCalls = append(m.forkCalls, forkCall{sessionID, provider, fromSeq, parentSession})
	return m.forkError
}

func (m *mockFileLogger) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCalls++
	return m.closeError
}

// mockLokiExporter is a test double for *LokiExporter that tracks Push calls
type mockLokiExporter struct {
	mu         sync.Mutex
	pushCalls  []lokiPushCall
	closeCalls int
	closeOrder *[]string // shared slice to track close ordering
}

type lokiPushCall struct {
	entry    map[string]interface{}
	provider string
}

func newMockLokiExporter(closeOrder *[]string) *mockLokiExporter {
	return &mockLokiExporter{closeOrder: closeOrder}
}

func (m *mockLokiExporter) Push(entry map[string]interface{}, provider string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pushCalls = append(m.pushCalls, lokiPushCall{entry, provider})
}

func (m *mockLokiExporter) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCalls++
	if m.closeOrder != nil {
		*m.closeOrder = append(*m.closeOrder, "loki")
	}
	return nil
}

// mockFileLoggerWithCloseOrder wraps mockFileLogger to track close ordering
type mockFileLoggerWithCloseOrder struct {
	*mockFileLogger
	closeOrder *[]string
}

func (m *mockFileLoggerWithCloseOrder) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCalls++
	if m.closeOrder != nil {
		*m.closeOrder = append(*m.closeOrder, "file")
	}
	return m.closeError
}

func TestMultiWriter_LogSessionStart_BothCalled(t *testing.T) {
	fileLogger := newMockFileLogger()
	closeOrder := []string{}
	lokiExporter := newMockLokiExporter(&closeOrder)

	mw := NewMultiWriter(fileLogger, lokiExporter)

	sessionID := "test-session-123"
	provider := "anthropic"
	upstream := "api.anthropic.com"

	err := mw.LogSessionStart(sessionID, provider, upstream)
	if err != nil {
		t.Fatalf("LogSessionStart returned error: %v", err)
	}

	// Verify file logger was called
	if len(fileLogger.sessionStartCalls) != 1 {
		t.Errorf("Expected 1 session start call to file logger, got %d", len(fileLogger.sessionStartCalls))
	}
	if len(fileLogger.sessionStartCalls) > 0 {
		call := fileLogger.sessionStartCalls[0]
		if call.sessionID != sessionID || call.provider != provider || call.upstream != upstream {
			t.Errorf("File logger called with wrong args: got (%s, %s, %s), want (%s, %s, %s)",
				call.sessionID, call.provider, call.upstream, sessionID, provider, upstream)
		}
	}

	// Verify Loki exporter was called
	if len(lokiExporter.pushCalls) != 1 {
		t.Errorf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}
	if len(lokiExporter.pushCalls) > 0 {
		call := lokiExporter.pushCalls[0]
		if call.provider != provider {
			t.Errorf("Loki exporter called with wrong provider: got %s, want %s", call.provider, provider)
		}
		if call.entry["type"] != "session_start" {
			t.Errorf("Loki entry has wrong type: got %v, want session_start", call.entry["type"])
		}
	}
}

func TestMultiWriter_LogRequest_BothCalled(t *testing.T) {
	fileLogger := newMockFileLogger()
	closeOrder := []string{}
	lokiExporter := newMockLokiExporter(&closeOrder)

	mw := NewMultiWriter(fileLogger, lokiExporter)

	sessionID := "test-session-123"
	provider := "anthropic"
	seq := 1
	method := "POST"
	path := "/v1/messages"
	headers := http.Header{"Content-Type": {"application/json"}}
	body := []byte(`{"test":"data"}`)
	requestID := "req-123"

	err := mw.LogRequest(sessionID, provider, seq, method, path, headers, body, requestID)
	if err != nil {
		t.Fatalf("LogRequest returned error: %v", err)
	}

	// Verify file logger was called
	if len(fileLogger.requestCalls) != 1 {
		t.Errorf("Expected 1 request call to file logger, got %d", len(fileLogger.requestCalls))
	}
	if len(fileLogger.requestCalls) > 0 {
		call := fileLogger.requestCalls[0]
		if call.sessionID != sessionID || call.provider != provider || call.seq != seq {
			t.Errorf("File logger called with wrong args")
		}
	}

	// Verify Loki exporter was called
	if len(lokiExporter.pushCalls) != 1 {
		t.Errorf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}
	if len(lokiExporter.pushCalls) > 0 {
		call := lokiExporter.pushCalls[0]
		if call.provider != provider {
			t.Errorf("Loki exporter called with wrong provider: got %s, want %s", call.provider, provider)
		}
		if call.entry["type"] != "request" {
			t.Errorf("Loki entry has wrong type: got %v, want request", call.entry["type"])
		}
	}
}

func TestMultiWriter_LogResponse_BothCalled(t *testing.T) {
	fileLogger := newMockFileLogger()
	closeOrder := []string{}
	lokiExporter := newMockLokiExporter(&closeOrder)

	mw := NewMultiWriter(fileLogger, lokiExporter)

	sessionID := "test-session-123"
	provider := "anthropic"
	seq := 1
	status := 200
	headers := http.Header{"Content-Type": {"application/json"}}
	body := []byte(`{"response":"ok"}`)
	timing := ResponseTiming{TTFBMs: 100, TotalMs: 200}
	requestID := "req-123"

	err := mw.LogResponse(sessionID, provider, seq, status, headers, body, nil, timing, requestID)
	if err != nil {
		t.Fatalf("LogResponse returned error: %v", err)
	}

	// Verify file logger was called
	if len(fileLogger.responseCalls) != 1 {
		t.Errorf("Expected 1 response call to file logger, got %d", len(fileLogger.responseCalls))
	}
	if len(fileLogger.responseCalls) > 0 {
		call := fileLogger.responseCalls[0]
		if call.sessionID != sessionID || call.provider != provider || call.seq != seq || call.status != status {
			t.Errorf("File logger called with wrong args")
		}
	}

	// Verify Loki exporter was called
	if len(lokiExporter.pushCalls) != 1 {
		t.Errorf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}
	if len(lokiExporter.pushCalls) > 0 {
		call := lokiExporter.pushCalls[0]
		if call.provider != provider {
			t.Errorf("Loki exporter called with wrong provider: got %s, want %s", call.provider, provider)
		}
		if call.entry["type"] != "response" {
			t.Errorf("Loki entry has wrong type: got %v, want response", call.entry["type"])
		}
	}
}

func TestMultiWriter_LogFork_BothCalled(t *testing.T) {
	fileLogger := newMockFileLogger()
	closeOrder := []string{}
	lokiExporter := newMockLokiExporter(&closeOrder)

	mw := NewMultiWriter(fileLogger, lokiExporter)

	sessionID := "test-session-123"
	provider := "anthropic"
	fromSeq := 5
	parentSession := "parent-session-456"

	err := mw.LogFork(sessionID, provider, fromSeq, parentSession)
	if err != nil {
		t.Fatalf("LogFork returned error: %v", err)
	}

	// Verify file logger was called
	if len(fileLogger.forkCalls) != 1 {
		t.Errorf("Expected 1 fork call to file logger, got %d", len(fileLogger.forkCalls))
	}
	if len(fileLogger.forkCalls) > 0 {
		call := fileLogger.forkCalls[0]
		if call.sessionID != sessionID || call.provider != provider || call.fromSeq != fromSeq || call.parentSession != parentSession {
			t.Errorf("File logger called with wrong args")
		}
	}

	// Verify Loki exporter was called
	if len(lokiExporter.pushCalls) != 1 {
		t.Errorf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}
	if len(lokiExporter.pushCalls) > 0 {
		call := lokiExporter.pushCalls[0]
		if call.provider != provider {
			t.Errorf("Loki exporter called with wrong provider: got %s, want %s", call.provider, provider)
		}
		if call.entry["type"] != "fork" {
			t.Errorf("Loki entry has wrong type: got %v, want fork", call.entry["type"])
		}
	}
}

func TestMultiWriter_NilLoki_NoError(t *testing.T) {
	fileLogger := newMockFileLogger()

	// Create MultiWriter with nil Loki exporter
	mw := NewMultiWriter(fileLogger, nil)

	sessionID := "test-session-123"
	provider := "anthropic"
	upstream := "api.anthropic.com"

	// All methods should work without error when Loki is nil
	err := mw.LogSessionStart(sessionID, provider, upstream)
	if err != nil {
		t.Errorf("LogSessionStart with nil Loki returned error: %v", err)
	}

	err = mw.LogRequest(sessionID, provider, 1, "POST", "/v1/messages", nil, []byte(`{}`), "req-1")
	if err != nil {
		t.Errorf("LogRequest with nil Loki returned error: %v", err)
	}

	err = mw.LogResponse(sessionID, provider, 1, 200, nil, []byte(`{}`), nil, ResponseTiming{}, "req-1")
	if err != nil {
		t.Errorf("LogResponse with nil Loki returned error: %v", err)
	}

	err = mw.LogFork(sessionID, provider, 1, "parent-123")
	if err != nil {
		t.Errorf("LogFork with nil Loki returned error: %v", err)
	}

	// RegisterUpstream should also work
	mw.RegisterUpstream(sessionID, upstream)

	// Verify file logger was called for all operations
	if len(fileLogger.sessionStartCalls) != 1 {
		t.Errorf("Expected 1 session start call, got %d", len(fileLogger.sessionStartCalls))
	}
	if len(fileLogger.requestCalls) != 1 {
		t.Errorf("Expected 1 request call, got %d", len(fileLogger.requestCalls))
	}
	if len(fileLogger.responseCalls) != 1 {
		t.Errorf("Expected 1 response call, got %d", len(fileLogger.responseCalls))
	}
	if len(fileLogger.forkCalls) != 1 {
		t.Errorf("Expected 1 fork call, got %d", len(fileLogger.forkCalls))
	}
	if len(fileLogger.registerUpstreamCalls) != 1 {
		t.Errorf("Expected 1 register upstream call, got %d", len(fileLogger.registerUpstreamCalls))
	}
}

func TestMultiWriter_FileError_Returned(t *testing.T) {
	fileLogger := newMockFileLogger()
	closeOrder := []string{}
	lokiExporter := newMockLokiExporter(&closeOrder)

	mw := NewMultiWriter(fileLogger, lokiExporter)

	sessionID := "test-session-123"
	provider := "anthropic"

	// Test that file errors are returned to caller
	expectedErr := errors.New("file write error")

	// Test LogSessionStart error propagation
	fileLogger.sessionStartError = expectedErr
	err := mw.LogSessionStart(sessionID, provider, "api.anthropic.com")
	if err != expectedErr {
		t.Errorf("LogSessionStart: expected error %v, got %v", expectedErr, err)
	}
	fileLogger.sessionStartError = nil

	// Test LogRequest error propagation
	fileLogger.requestError = expectedErr
	err = mw.LogRequest(sessionID, provider, 1, "POST", "/v1/messages", nil, []byte(`{}`), "req-1")
	if err != expectedErr {
		t.Errorf("LogRequest: expected error %v, got %v", expectedErr, err)
	}
	fileLogger.requestError = nil

	// Test LogResponse error propagation
	fileLogger.responseError = expectedErr
	err = mw.LogResponse(sessionID, provider, 1, 200, nil, []byte(`{}`), nil, ResponseTiming{}, "req-1")
	if err != expectedErr {
		t.Errorf("LogResponse: expected error %v, got %v", expectedErr, err)
	}
	fileLogger.responseError = nil

	// Test LogFork error propagation
	fileLogger.forkError = expectedErr
	err = mw.LogFork(sessionID, provider, 1, "parent-123")
	if err != expectedErr {
		t.Errorf("LogFork: expected error %v, got %v", expectedErr, err)
	}
}

func TestMultiWriter_Close_ClosesLokiFirst(t *testing.T) {
	closeOrder := []string{}
	fileLogger := &mockFileLoggerWithCloseOrder{
		mockFileLogger: newMockFileLogger(),
		closeOrder:     &closeOrder,
	}
	lokiExporter := newMockLokiExporter(&closeOrder)

	mw := NewMultiWriterWithCloseOrder(fileLogger, lokiExporter, &closeOrder)

	err := mw.Close()
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	// Verify close order: Loki first, then file
	if len(closeOrder) != 2 {
		t.Fatalf("Expected 2 closes, got %d: %v", len(closeOrder), closeOrder)
	}
	if closeOrder[0] != "loki" {
		t.Errorf("Expected Loki to close first, but got: %v", closeOrder)
	}
	if closeOrder[1] != "file" {
		t.Errorf("Expected file to close second, but got: %v", closeOrder)
	}
}

func TestMultiWriter_Close_NilLoki(t *testing.T) {
	fileLogger := newMockFileLogger()

	mw := NewMultiWriter(fileLogger, nil)

	err := mw.Close()
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	// Verify file logger was closed
	if fileLogger.closeCalls != 1 {
		t.Errorf("Expected 1 close call to file logger, got %d", fileLogger.closeCalls)
	}
}

func TestMultiWriter_RegisterUpstream_BothCalled(t *testing.T) {
	fileLogger := newMockFileLogger()
	closeOrder := []string{}
	lokiExporter := newMockLokiExporter(&closeOrder)

	mw := NewMultiWriter(fileLogger, lokiExporter)

	sessionID := "test-session-123"
	upstream := "api.anthropic.com"

	mw.RegisterUpstream(sessionID, upstream)

	// Verify file logger was called
	if len(fileLogger.registerUpstreamCalls) != 1 {
		t.Errorf("Expected 1 register upstream call to file logger, got %d", len(fileLogger.registerUpstreamCalls))
	}
	if len(fileLogger.registerUpstreamCalls) > 0 {
		call := fileLogger.registerUpstreamCalls[0]
		if call.sessionID != sessionID || call.upstream != upstream {
			t.Errorf("File logger called with wrong args: got (%s, %s), want (%s, %s)",
				call.sessionID, call.upstream, sessionID, upstream)
		}
	}

	// Note: RegisterUpstream doesn't push to Loki since it's metadata only
	// The session info goes to Loki in LogSessionStart
}

// Test that MultiWriter implements ProxyLogger interface
func TestMultiWriter_ImplementsProxyLogger(t *testing.T) {
	fileLogger := newMockFileLogger()
	mw := NewMultiWriter(fileLogger, nil)

	// This is a compile-time check - if MultiWriter doesn't implement ProxyLogger,
	// this will fail to compile
	var _ ProxyLogger = mw
}

// Test streaming response (with chunks instead of body)
func TestMultiWriter_LogResponse_WithChunks(t *testing.T) {
	fileLogger := newMockFileLogger()
	closeOrder := []string{}
	lokiExporter := newMockLokiExporter(&closeOrder)

	mw := NewMultiWriter(fileLogger, lokiExporter)

	sessionID := "test-session-123"
	provider := "anthropic"
	seq := 1
	status := 200
	headers := http.Header{"Content-Type": {"text/event-stream"}}
	chunks := []StreamChunk{
		{Timestamp: time.Now(), DeltaMs: 0, Raw: "data: {\"chunk\":1}"},
		{Timestamp: time.Now(), DeltaMs: 100, Raw: "data: {\"chunk\":2}"},
	}
	timing := ResponseTiming{TTFBMs: 50, TotalMs: 150}
	requestID := "req-123"

	err := mw.LogResponse(sessionID, provider, seq, status, headers, nil, chunks, timing, requestID)
	if err != nil {
		t.Fatalf("LogResponse returned error: %v", err)
	}

	// Verify file logger was called with chunks
	if len(fileLogger.responseCalls) != 1 {
		t.Errorf("Expected 1 response call to file logger, got %d", len(fileLogger.responseCalls))
	}
	if len(fileLogger.responseCalls) > 0 {
		call := fileLogger.responseCalls[0]
		if len(call.chunks) != 2 {
			t.Errorf("Expected 2 chunks in file logger call, got %d", len(call.chunks))
		}
	}

	// Verify Loki exporter was called
	if len(lokiExporter.pushCalls) != 1 {
		t.Errorf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}
}

func TestMultiWriter_LogRequest_IncludesRequestSHA(t *testing.T) {
	fileLogger := newMockFileLogger()
	closeOrder := []string{}
	lokiExporter := newMockLokiExporter(&closeOrder)

	mw := NewMultiWriter(fileLogger, lokiExporter)

	sessionID := "test-session-sha"
	provider := "anthropic"
	seq := 1
	method := "POST"
	path := "/v1/messages"
	headers := http.Header{"Content-Type": {"application/json"}}
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"Hello"}]}`)
	requestID := "req-sha-test"

	err := mw.LogRequest(sessionID, provider, seq, method, path, headers, body, requestID)
	if err != nil {
		t.Fatalf("LogRequest returned error: %v", err)
	}

	// Verify Loki entry includes request_sha
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	entry := lokiExporter.pushCalls[0].entry
	sha, ok := entry["request_sha"].(string)
	if !ok || sha == "" {
		t.Fatal("Expected request_sha in Loki entry")
	}

	// SHA should be 64 hex characters (SHA256)
	if len(sha) != 64 {
		t.Errorf("Expected 64-char hex SHA, got %d chars: %s", len(sha), sha)
	}

	// Verify we can round-trip: extract body, compute SHA, compare
	bodyFromEntry, ok := entry["body"].(string)
	if !ok {
		t.Fatal("Expected body in Loki entry")
	}

	// Compute SHA of extracted body and verify it matches
	expectedSHA := computeSHA256([]byte(bodyFromEntry))
	if sha != expectedSHA {
		t.Errorf("SHA mismatch: entry has %s, computed %s", sha, expectedSHA)
	}
}

func TestMultiWriterLogObservation(t *testing.T) {
	fileLogger := newMockFileLogger()
	closeOrder := []string{}
	lokiExporter := newMockLokiExporter(&closeOrder)

	mw := NewMultiWriter(fileLogger, lokiExporter)

	entry := map[string]interface{}{
		"type": "request",
		"_meta": map[string]interface{}{
			"schema_version": "telemetry-contract-v0",
		},
		"request": map[string]interface{}{
			"ingress_path": "/cbrun/run-test/mantle/v1/responses",
		},
	}

	if err := mw.LogObservation("mantle-session-123", "openai", entry); err != nil {
		t.Fatalf("LogObservation returned error: %v", err)
	}

	if len(fileLogger.observationCalls) != 1 {
		t.Fatalf("Expected 1 observation call to file logger, got %d", len(fileLogger.observationCalls))
	}
	if got := fileLogger.observationCalls[0].entry["type"]; got != "request" {
		t.Fatalf("file logger observation type = %v, want request", got)
	}

	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}
	if got := lokiExporter.pushCalls[0].entry["type"]; got != "request" {
		t.Fatalf("Loki observation type = %v, want request", got)
	}
}

// computeSHA256 computes the hex-encoded SHA256 of data
func computeSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// multi_writer_test.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	lastCapture           ResponseCapture

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

func (m *mockFileLogger) LogResponse(sessionID, provider string, seq int, status int, headers http.Header, body []byte, chunks []StreamChunk, timing ResponseTiming, requestID string, capture ResponseCapture) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responseCalls = append(m.responseCalls, responseCall{sessionID, provider, seq, status, headers, body, chunks, timing, requestID})
	m.lastCapture = capture
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

func TestMultiWriter_LogRequest_WritesNeutralRunMetadata(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)

	requestID := "req-neutral"
	mw.SetRequestLogContext(requestID, RequestLogContext{
		RunID:         "proj-20260628-run",
		ResolvedRunID: "resolved-other-run-id",
		ClientFPHash:  strings.Repeat("a", 64),
		Project:       "proj",
		ProviderRoute: "aws-bedrock",
		WireAPI:       "messages",
	})

	err := mw.LogRequest("session-1", "anthropic", 1, "POST", "/v1/messages", http.Header{}, []byte(`{"ok":true}`), requestID)
	if err != nil {
		t.Fatalf("LogRequest returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	meta, ok := lokiExporter.pushCalls[0].entry["_meta"].(map[string]interface{})
	if !ok {
		t.Fatalf("_meta missing or wrong type: %#v", lokiExporter.pushCalls[0].entry)
	}
	if got := meta["run_id"]; got != "proj-20260628-run" {
		t.Fatalf("run_id = %v, want proj-20260628-run", got)
	}
	if got := meta["resolved_run_id"]; got != "resolved-other-run-id" {
		t.Fatalf("resolved_run_id = %v, want resolved-other-run-id", got)
	}
	if _, ok := meta["cloud_build_run_id"]; ok {
		t.Fatalf("cloud_build_run_id unexpectedly present in neutral context: %#v", meta)
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

	err := mw.LogResponse(sessionID, provider, seq, status, headers, body, nil, timing, requestID, ResponseCapture{Termination: TerminationEOF})
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

	err = mw.LogResponse(sessionID, provider, 1, 200, nil, []byte(`{}`), nil, ResponseTiming{}, "req-1", ResponseCapture{Termination: TerminationEOF})
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
	err = mw.LogResponse(sessionID, provider, 1, 200, nil, []byte(`{}`), nil, ResponseTiming{}, "req-1", ResponseCapture{Termination: TerminationEOF})
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

	err := mw.LogResponse(sessionID, provider, seq, status, headers, nil, chunks, timing, requestID, ResponseCapture{Termination: TerminationEOF})
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
	// This test is specifically about the body/SHA round-trip, which only
	// applies when body shipping to Loki is opted into (PRI-3040/PRI-2489
	// default is metadata-only; see TestMultiWriter_LogRequest_Loki* below
	// for the default-off behavior).
	mw.includeBody = true

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

// --- PRI-3040/PRI-2489: Loki gets metadata only ---------------------------
//
// LogRequest/LogResponse must default to NOT shipping request/response
// bodies (or, for streaming responses, raw chunk text) to Loki, and must
// keep shipping the full body/chunks to the file logger regardless. The
// flag (MultiWriter.includeBody, wired from LLM_PROXY_LOKI_INCLUDE_BODY /
// loki.include_body) opts a deployment back into the old behavior.

const secretRequestBody = `{"model":"claude-sonnet-4-20250514","system":"you are a secret system prompt","messages":[{"role":"user","content":"a secret prompt that must never reach Loki"}]}`
const secretResponseBody = `{"content":[{"type":"text","text":"a secret response that must never reach Loki"}]}`

func TestMultiWriter_LogRequest_LokiOmitsBodyByDefault(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)
	// includeBody defaults to false (zero value) - not set here deliberately.

	body := []byte(secretRequestBody)
	err := mw.LogRequest("session-1", "anthropic", 1, "POST", "/v1/messages", http.Header{}, body, "req-1")
	if err != nil {
		t.Fatalf("LogRequest returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	entry := lokiExporter.pushCalls[0].entry
	if _, ok := entry["body"]; ok {
		t.Errorf("Loki entry must not contain body by default, got: %#v", entry["body"])
	}
	if omitted, ok := entry["body_omitted"].(bool); !ok || !omitted {
		t.Errorf("Expected body_omitted=true in Loki entry, got: %#v", entry["body_omitted"])
	}
	// Metadata must still be present.
	for _, key := range []string{"method", "path", "headers", "size", "request_sha", "_meta", "type", "seq"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("Expected metadata key %q to still be present in Loki entry, entry: %#v", key, entry)
		}
	}
}

func TestMultiWriter_LogRequest_LokiIncludesBodyWhenFlagSet(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)
	mw.includeBody = true

	body := []byte(secretRequestBody)
	err := mw.LogRequest("session-1", "anthropic", 1, "POST", "/v1/messages", http.Header{}, body, "req-1")
	if err != nil {
		t.Fatalf("LogRequest returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	entry := lokiExporter.pushCalls[0].entry
	got, ok := entry["body"].(string)
	if !ok || got != secretRequestBody {
		t.Errorf("Expected Loki entry body %q, got %#v", secretRequestBody, entry["body"])
	}
	if _, ok := entry["body_omitted"]; ok {
		t.Errorf("Did not expect body_omitted when includeBody=true, got: %#v", entry["body_omitted"])
	}
}

func TestMultiWriter_LogRequest_FileLoggerAlwaysGetsFullBodyRegardlessOfLokiFlag(t *testing.T) {
	for _, includeBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("includeBody=%v", includeBody), func(t *testing.T) {
			fileLogger := newMockFileLogger()
			lokiExporter := newMockLokiExporter(nil)
			mw := NewMultiWriter(fileLogger, lokiExporter)
			mw.includeBody = includeBody

			body := []byte(secretRequestBody)
			if err := mw.LogRequest("session-1", "anthropic", 1, "POST", "/v1/messages", http.Header{}, body, "req-1"); err != nil {
				t.Fatalf("LogRequest returned error: %v", err)
			}

			if len(fileLogger.requestCalls) != 1 {
				t.Fatalf("Expected 1 request call to file logger, got %d", len(fileLogger.requestCalls))
			}
			if got := string(fileLogger.requestCalls[0].body); got != secretRequestBody {
				t.Errorf("File logger must always receive the full body; got %q, want %q", got, secretRequestBody)
			}

			// Aliasing guard: mutate the map handed to the Loki pusher AFTER the
			// call returns (simulating a future refactor that builds one shared
			// entry map and deletes keys from it rather than building the Loki
			// entry independently). If LogRequest ever starts handing the file
			// logger a reference into that same map instead of the raw []byte it
			// gets today, this mutation would corrupt the file logger's copy and
			// this assertion would catch it.
			if len(lokiExporter.pushCalls) != 1 {
				t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
			}
			pushedEntry := lokiExporter.pushCalls[0].entry
			for k := range pushedEntry {
				delete(pushedEntry, k)
			}
			pushedEntry["body"] = "INJECTED-AFTER-THE-FACT"

			if got := string(fileLogger.requestCalls[0].body); got != secretRequestBody {
				t.Fatalf("File logger's recorded body changed after mutating the Loki-pushed map - entry is aliased! got %q, want %q", got, secretRequestBody)
			}
		})
	}
}

func TestMultiWriter_LogResponse_NonStreaming_LokiOmitsBodyByDefault(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)

	body := []byte(secretResponseBody)
	err := mw.LogResponse("session-1", "anthropic", 1, 200, http.Header{}, body, nil, ResponseTiming{}, "req-1", ResponseCapture{Termination: TerminationEOF})
	if err != nil {
		t.Fatalf("LogResponse returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	entry := lokiExporter.pushCalls[0].entry
	if _, ok := entry["body"]; ok {
		t.Errorf("Loki entry must not contain body by default, got: %#v", entry["body"])
	}
	if omitted, ok := entry["body_omitted"].(bool); !ok || !omitted {
		t.Errorf("Expected body_omitted=true in Loki entry, got: %#v", entry["body_omitted"])
	}
	for _, key := range []string{"status", "headers", "timing", "size", "_meta", "type", "seq"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("Expected metadata key %q to still be present in Loki entry, entry: %#v", key, entry)
		}
	}
}

func TestMultiWriter_LogResponse_NonStreaming_LokiIncludesBodyWhenFlagSet(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)
	mw.includeBody = true

	body := []byte(secretResponseBody)
	err := mw.LogResponse("session-1", "anthropic", 1, 200, http.Header{}, body, nil, ResponseTiming{}, "req-1", ResponseCapture{Termination: TerminationEOF})
	if err != nil {
		t.Fatalf("LogResponse returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	entry := lokiExporter.pushCalls[0].entry
	got, ok := entry["body"].(string)
	if !ok || got != secretResponseBody {
		t.Errorf("Expected Loki entry body %q, got %#v", secretResponseBody, entry["body"])
	}
	if _, ok := entry["body_omitted"]; ok {
		t.Errorf("Did not expect body_omitted when includeBody=true, got: %#v", entry["body_omitted"])
	}
}

func TestMultiWriter_LogResponse_Streaming_LokiOmitsChunksByDefault(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)

	chunks := []StreamChunk{
		{Timestamp: time.Now(), DeltaMs: 0, Raw: "data: " + secretResponseBody},
	}
	err := mw.LogResponse("session-1", "anthropic", 1, 200, http.Header{}, nil, chunks, ResponseTiming{}, "req-1", ResponseCapture{Termination: TerminationEOF})
	if err != nil {
		t.Fatalf("LogResponse returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	entry := lokiExporter.pushCalls[0].entry
	if _, ok := entry["chunks"]; ok {
		t.Errorf("Loki entry must not contain chunks by default, got: %#v", entry["chunks"])
	}
	if _, ok := entry["body"]; ok {
		t.Errorf("Loki entry must not contain body by default either, got: %#v", entry["body"])
	}
	if omitted, ok := entry["body_omitted"].(bool); !ok || !omitted {
		t.Errorf("Expected body_omitted=true in Loki entry, got: %#v", entry["body_omitted"])
	}
}

func TestMultiWriter_LogResponse_Streaming_LokiIncludesChunksWhenFlagSet(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)
	mw.includeBody = true

	chunks := []StreamChunk{
		{Timestamp: time.Now(), DeltaMs: 0, Raw: "data: " + secretResponseBody},
	}
	err := mw.LogResponse("session-1", "anthropic", 1, 200, http.Header{}, nil, chunks, ResponseTiming{}, "req-1", ResponseCapture{Termination: TerminationEOF})
	if err != nil {
		t.Fatalf("LogResponse returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	entry := lokiExporter.pushCalls[0].entry
	gotChunks, ok := entry["chunks"].([]StreamChunk)
	if !ok || len(gotChunks) != 1 || gotChunks[0].Raw != chunks[0].Raw {
		t.Errorf("Expected Loki entry chunks %#v, got %#v", chunks, entry["chunks"])
	}
}

func TestMultiWriter_LogResponse_FileLoggerAlwaysGetsFullBodyRegardlessOfLokiFlag(t *testing.T) {
	for _, includeBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("non-streaming/includeBody=%v", includeBody), func(t *testing.T) {
			fileLogger := newMockFileLogger()
			lokiExporter := newMockLokiExporter(nil)
			mw := NewMultiWriter(fileLogger, lokiExporter)
			mw.includeBody = includeBody

			body := []byte(secretResponseBody)
			if err := mw.LogResponse("session-1", "anthropic", 1, 200, http.Header{}, body, nil, ResponseTiming{}, "req-1", ResponseCapture{Termination: TerminationEOF}); err != nil {
				t.Fatalf("LogResponse returned error: %v", err)
			}

			if len(fileLogger.responseCalls) != 1 {
				t.Fatalf("Expected 1 response call to file logger, got %d", len(fileLogger.responseCalls))
			}
			if got := string(fileLogger.responseCalls[0].body); got != secretResponseBody {
				t.Errorf("File logger must always receive the full body; got %q, want %q", got, secretResponseBody)
			}

			// Aliasing guard - see TestMultiWriter_LogRequest_FileLoggerAlwaysGetsFullBodyRegardlessOfLokiFlag.
			if len(lokiExporter.pushCalls) != 1 {
				t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
			}
			pushedEntry := lokiExporter.pushCalls[0].entry
			for k := range pushedEntry {
				delete(pushedEntry, k)
			}
			pushedEntry["body"] = "INJECTED-AFTER-THE-FACT"

			if got := string(fileLogger.responseCalls[0].body); got != secretResponseBody {
				t.Fatalf("File logger's recorded body changed after mutating the Loki-pushed map - entry is aliased! got %q, want %q", got, secretResponseBody)
			}
		})

		t.Run(fmt.Sprintf("streaming/includeBody=%v", includeBody), func(t *testing.T) {
			fileLogger := newMockFileLogger()
			lokiExporter := newMockLokiExporter(nil)
			mw := NewMultiWriter(fileLogger, lokiExporter)
			mw.includeBody = includeBody

			chunks := []StreamChunk{
				{Timestamp: time.Now(), DeltaMs: 0, Raw: "data: " + secretResponseBody},
			}
			if err := mw.LogResponse("session-1", "anthropic", 1, 200, http.Header{}, nil, chunks, ResponseTiming{}, "req-1", ResponseCapture{Termination: TerminationEOF}); err != nil {
				t.Fatalf("LogResponse returned error: %v", err)
			}

			if len(fileLogger.responseCalls) != 1 {
				t.Fatalf("Expected 1 response call to file logger, got %d", len(fileLogger.responseCalls))
			}
			if len(fileLogger.responseCalls[0].chunks) != 1 || fileLogger.responseCalls[0].chunks[0].Raw != chunks[0].Raw {
				t.Errorf("File logger must always receive the full chunks; got %#v, want %#v", fileLogger.responseCalls[0].chunks, chunks)
			}

			// Aliasing guard.
			if len(lokiExporter.pushCalls) != 1 {
				t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
			}
			pushedEntry := lokiExporter.pushCalls[0].entry
			for k := range pushedEntry {
				delete(pushedEntry, k)
			}
			pushedEntry["chunks"] = "INJECTED-AFTER-THE-FACT"

			if len(fileLogger.responseCalls[0].chunks) != 1 || fileLogger.responseCalls[0].chunks[0].Raw != chunks[0].Raw {
				t.Fatalf("File logger's recorded chunks changed after mutating the Loki-pushed map - entry is aliased! got %#v, want %#v", fileLogger.responseCalls[0].chunks, chunks)
			}
		})
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

// --- PRI-3040/PRI-2489, mantle/Bedrock path: LogObservation ----------------
//
// mantle.go's writeMantleObservation hands LogObservation ONE entry map used
// for both destinations (unlike LogRequest/LogResponse, which build a
// separate map per destination). mantle.go nests body/chunks text under
// entry["request"]["body"] (logMantleRequestObservation) and
// entry["response"]["body"]/entry["response"]["chunks"]
// (logMantleResponseObservation, logMantleAbortedResponseObservation).

const secretMantleRequestText = "a secret mantle prompt that must never reach Loki"
const secretMantleResponseText = "a secret mantle response that must never reach Loki"

func mantleRequestEntry(bodyText string) map[string]any {
	return map[string]any{
		"type": "request",
		"_meta": map[string]any{
			"schema_version": "telemetry-contract-v0",
		},
		"request": map[string]any{
			"method":        "POST",
			"ingress_path":  "/cbrun/run-test/mantle/v1/responses",
			"raw_query":     "",
			"proxy_route":   "mantle",
			"upstream_host": "bedrock-mantle.us-west-2.api.aws",
			"upstream_path": "/openai/v1/responses",
			"body":          bodyText,
		},
	}
}

func mantleResponseEntryNonStreaming(bodyText string) map[string]any {
	return map[string]any{
		"type":        "response",
		"status":      200,
		"timing":      ResponseTiming{TTFBMs: 50, TotalMs: 150},
		"termination": TerminationEOF,
		"_meta": map[string]any{
			"schema_version": "telemetry-contract-v0",
		},
		"response": map[string]any{
			"body": bodyText,
		},
		"path":              "/openai/v1/responses",
		"upstream":          "bedrock-mantle.us-west-2.api.aws",
		"metering_provider": "bedrock",
		"capture_version":   CaptureVersion,
	}
}

func mantleResponseEntryStreaming(bodyText string) map[string]any {
	chunks := []StreamChunk{
		{Timestamp: time.Now(), DeltaMs: 0, Raw: "data: " + bodyText},
	}
	return map[string]any{
		"type":        "response",
		"status":      200,
		"timing":      ResponseTiming{TTFBMs: 50, TotalMs: 150},
		"termination": TerminationEOF,
		"_meta": map[string]any{
			"schema_version": "telemetry-contract-v0",
		},
		"response": map[string]any{
			// mantle.go sets both chunks and the response.completed-extracted
			// body when present (mantleResponseBodyFromStream); exercise both.
			"chunks": chunks,
			"body":   map[string]any{"text": bodyText},
		},
		"path":              "/openai/v1/responses",
		"upstream":          "bedrock-mantle.us-west-2.api.aws",
		"metering_provider": "bedrock",
		"capture_version":   CaptureVersion,
	}
}

func TestMultiWriter_LogObservation_MantleRequest_LokiOmitsBodyByDefault(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)

	entry := mantleRequestEntry(secretMantleRequestText)
	if err := mw.LogObservation("mantle-session", "openai", entry); err != nil {
		t.Fatalf("LogObservation returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	request, ok := lokiExporter.pushCalls[0].entry["request"].(map[string]any)
	if !ok {
		t.Fatalf("Expected Loki entry to have a request map, got %#v", lokiExporter.pushCalls[0].entry)
	}
	if _, ok := request["body"]; ok {
		t.Errorf("Loki request observation must not contain body by default, got: %#v", request["body"])
	}
	if omitted, ok := request["body_omitted"].(bool); !ok || !omitted {
		t.Errorf("Expected request.body_omitted=true in Loki entry, got: %#v", request["body_omitted"])
	}
	for _, key := range []string{"method", "ingress_path", "upstream_host", "upstream_path"} {
		if _, ok := request[key]; !ok {
			t.Errorf("Expected metadata key %q to still be present in Loki request observation, got: %#v", key, request)
		}
	}
}

func TestMultiWriter_LogObservation_MantleRequest_LokiIncludesBodyWhenFlagSet(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)
	mw.includeBody = true

	entry := mantleRequestEntry(secretMantleRequestText)
	if err := mw.LogObservation("mantle-session", "openai", entry); err != nil {
		t.Fatalf("LogObservation returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	request, ok := lokiExporter.pushCalls[0].entry["request"].(map[string]any)
	if !ok {
		t.Fatalf("Expected Loki entry to have a request map, got %#v", lokiExporter.pushCalls[0].entry)
	}
	if got := request["body"]; got != secretMantleRequestText {
		t.Errorf("Expected Loki request observation body %q, got %#v", secretMantleRequestText, got)
	}
	if _, ok := request["body_omitted"]; ok {
		t.Errorf("Did not expect request.body_omitted when includeBody=true, got: %#v", request["body_omitted"])
	}
}

func TestMultiWriter_LogObservation_MantleResponseNonStreaming_LokiOmitsBodyByDefault(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)

	entry := mantleResponseEntryNonStreaming(secretMantleResponseText)
	if err := mw.LogObservation("mantle-session", "openai", entry); err != nil {
		t.Fatalf("LogObservation returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	response, ok := lokiExporter.pushCalls[0].entry["response"].(map[string]any)
	if !ok {
		t.Fatalf("Expected Loki entry to have a response map, got %#v", lokiExporter.pushCalls[0].entry)
	}
	if _, ok := response["body"]; ok {
		t.Errorf("Loki response observation must not contain body by default, got: %#v", response["body"])
	}
	if omitted, ok := response["body_omitted"].(bool); !ok || !omitted {
		t.Errorf("Expected response.body_omitted=true in Loki entry, got: %#v", response["body_omitted"])
	}
	// Top-level metadata (status/timing/termination/path/upstream/...) must
	// still be present.
	for _, key := range []string{"status", "timing", "termination", "path", "upstream", "metering_provider", "capture_version", "type"} {
		if _, ok := lokiExporter.pushCalls[0].entry[key]; !ok {
			t.Errorf("Expected metadata key %q to still be present in Loki entry, got: %#v", key, lokiExporter.pushCalls[0].entry)
		}
	}
}

func TestMultiWriter_LogObservation_MantleResponseNonStreaming_LokiIncludesBodyWhenFlagSet(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)
	mw.includeBody = true

	entry := mantleResponseEntryNonStreaming(secretMantleResponseText)
	if err := mw.LogObservation("mantle-session", "openai", entry); err != nil {
		t.Fatalf("LogObservation returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	response, ok := lokiExporter.pushCalls[0].entry["response"].(map[string]any)
	if !ok {
		t.Fatalf("Expected Loki entry to have a response map, got %#v", lokiExporter.pushCalls[0].entry)
	}
	if got := response["body"]; got != secretMantleResponseText {
		t.Errorf("Expected Loki response observation body %q, got %#v", secretMantleResponseText, got)
	}
	if _, ok := response["body_omitted"]; ok {
		t.Errorf("Did not expect response.body_omitted when includeBody=true, got: %#v", response["body_omitted"])
	}
}

func TestMultiWriter_LogObservation_MantleResponseStreaming_LokiOmitsBodyAndChunksByDefault(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)

	entry := mantleResponseEntryStreaming(secretMantleResponseText)
	if err := mw.LogObservation("mantle-session", "openai", entry); err != nil {
		t.Fatalf("LogObservation returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	response, ok := lokiExporter.pushCalls[0].entry["response"].(map[string]any)
	if !ok {
		t.Fatalf("Expected Loki entry to have a response map, got %#v", lokiExporter.pushCalls[0].entry)
	}
	if _, ok := response["chunks"]; ok {
		t.Errorf("Loki response observation must not contain chunks by default, got: %#v", response["chunks"])
	}
	if _, ok := response["body"]; ok {
		t.Errorf("Loki response observation must not contain body by default, got: %#v", response["body"])
	}
	if omitted, ok := response["body_omitted"].(bool); !ok || !omitted {
		t.Errorf("Expected response.body_omitted=true in Loki entry, got: %#v", response["body_omitted"])
	}
}

func TestMultiWriter_LogObservation_MantleResponseStreaming_LokiIncludesChunksAndBodyWhenFlagSet(t *testing.T) {
	fileLogger := newMockFileLogger()
	lokiExporter := newMockLokiExporter(nil)
	mw := NewMultiWriter(fileLogger, lokiExporter)
	mw.includeBody = true

	entry := mantleResponseEntryStreaming(secretMantleResponseText)
	if err := mw.LogObservation("mantle-session", "openai", entry); err != nil {
		t.Fatalf("LogObservation returned error: %v", err)
	}
	if len(lokiExporter.pushCalls) != 1 {
		t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
	}

	response, ok := lokiExporter.pushCalls[0].entry["response"].(map[string]any)
	if !ok {
		t.Fatalf("Expected Loki entry to have a response map, got %#v", lokiExporter.pushCalls[0].entry)
	}
	gotChunks, ok := response["chunks"].([]StreamChunk)
	if !ok || len(gotChunks) != 1 || !strings.Contains(gotChunks[0].Raw, secretMantleResponseText) {
		t.Errorf("Expected Loki response observation chunks to contain %q, got %#v", secretMantleResponseText, response["chunks"])
	}
	if _, ok := response["body"]; !ok {
		t.Errorf("Expected Loki response observation to still have body when includeBody=true, got: %#v", response)
	}
	if _, ok := response["body_omitted"]; ok {
		t.Errorf("Did not expect response.body_omitted when includeBody=true, got: %#v", response["body_omitted"])
	}
}

// TestMultiWriter_LogObservation_FileLoggerAlwaysGetsFullEntryRegardlessOfLokiFlag
// is the LogObservation analogue of the LogRequest/LogResponse aliasing
// guards above. LogObservation is the riskiest of the three: the caller
// hands it ONE entry map, used for both m.file.LogObservation and (after
// stripping, when includeBody is false) m.loki.Push - so stripping MUST
// build a copy, never mutate the shared map in place. This test proves it
// by mutating the map captured by the Loki pusher mock (and its nested
// request/response maps) after the call returns, then asserting the file
// logger's independently-recorded entry - the SAME object the test
// constructed - still has the original body/chunks untouched.
func TestMultiWriter_LogObservation_FileLoggerAlwaysGetsFullEntryRegardlessOfLokiFlag(t *testing.T) {
	scenarios := map[string]func(string) map[string]any{
		"request":              mantleRequestEntry,
		"response-nonstreaming": mantleResponseEntryNonStreaming,
		"response-streaming":    mantleResponseEntryStreaming,
	}
	secretByScenario := map[string]string{
		"request":               secretMantleRequestText,
		"response-nonstreaming": secretMantleResponseText,
		"response-streaming":    secretMantleResponseText,
	}

	for name, build := range scenarios {
		for _, includeBody := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/includeBody=%v", name, includeBody), func(t *testing.T) {
				fileLogger := newMockFileLogger()
				lokiExporter := newMockLokiExporter(nil)
				mw := NewMultiWriter(fileLogger, lokiExporter)
				mw.includeBody = includeBody

				secret := secretByScenario[name]
				entry := build(secret)

				if err := mw.LogObservation("mantle-session", "openai", entry); err != nil {
					t.Fatalf("LogObservation returned error: %v", err)
				}

				if len(fileLogger.observationCalls) != 1 {
					t.Fatalf("Expected 1 observation call to file logger, got %d", len(fileLogger.observationCalls))
				}
				recorded := fileLogger.observationCalls[0].entry

				// Sanity: the file logger's entry has the secret text somewhere,
				// before we touch the Loki side at all.
				if !entryContainsText(recorded, secret) {
					t.Fatalf("File logger's recorded entry is missing the full body/chunks text before any mutation: %#v", recorded)
				}

				if len(lokiExporter.pushCalls) != 1 {
					t.Fatalf("Expected 1 push call to Loki exporter, got %d", len(lokiExporter.pushCalls))
				}

				if !includeBody {
					// Aliasing guard: mutate the map (and its nested request/
					// response maps, if present) handed to the Loki pusher
					// AFTER the call returns, simulating a future refactor
					// that strips keys from a map shared with the file logger
					// instead of a copy. Only meaningful when includeBody is
					// false - that's the branch that must build a copy at all.
					// When includeBody is true, LogObservation intentionally
					// pushes the SAME map to both destinations (nothing needs
					// stripping), so mutating the one-and-only map afterward is
					// expected, not a bug - covered instead by the dedicated
					// *_LokiIncludesBodyWhenFlagSet tests above.
					pushed := lokiExporter.pushCalls[0].entry
					for _, key := range []string{"request", "response"} {
						if nested, ok := pushed[key].(map[string]any); ok {
							for k := range nested {
								delete(nested, k)
							}
							nested["body"] = "INJECTED-AFTER-THE-FACT"
							nested["chunks"] = "INJECTED-AFTER-THE-FACT"
						}
					}
					for k := range pushed {
						delete(pushed, k)
					}
					pushed["body"] = "INJECTED-AFTER-THE-FACT"

					if !entryContainsText(recorded, secret) {
						t.Fatalf("File logger's recorded entry changed after mutating the Loki-pushed map (and its nested maps) - entry is aliased! got %#v", recorded)
					}
				}
			})
		}
	}
}

// entryContainsText reports whether secret appears anywhere in entry's
// "request"/"response" maps, under "body" (any shape, stringified) or
// "chunks" ([]StreamChunk, checked via Raw).
func entryContainsText(entry map[string]interface{}, secret string) bool {
	for _, key := range []string{"request", "response"} {
		nested, ok := entry[key].(map[string]interface{})
		if !ok {
			continue
		}
		if body, ok := nested["body"]; ok {
			if strings.Contains(fmt.Sprintf("%v", body), secret) {
				return true
			}
		}
		if chunks, ok := nested["chunks"].([]StreamChunk); ok {
			for _, c := range chunks {
				if strings.Contains(c.Raw, secret) {
					return true
				}
			}
		}
	}
	return false
}

// computeSHA256 computes the hex-encoded SHA256 of data
func computeSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

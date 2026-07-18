package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureProxyLogger records LogResponse calls; all other ProxyLogger methods no-op.
type captureProxyLogger struct {
	mu        sync.Mutex
	responses []capturedResponse
}

type capturedResponse struct {
	status  int
	chunks  []StreamChunk
	capture ResponseCapture
}

func (c *captureProxyLogger) RegisterUpstream(sessionID, upstream string)                {}
func (c *captureProxyLogger) LogSessionStart(sessionID, provider, upstream string) error { return nil }
func (c *captureProxyLogger) LogRequest(sessionID, provider string, seq int, method, path string, headers http.Header, body []byte, requestID string) error {
	return nil
}
func (c *captureProxyLogger) LogResponse(sessionID, provider string, seq int, status int, headers http.Header, body []byte, chunks []StreamChunk, timing ResponseTiming, requestID string, capture ResponseCapture) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.responses = append(c.responses, capturedResponse{status: status, chunks: chunks, capture: capture})
	return nil
}
func (c *captureProxyLogger) LogObservation(sessionID, provider string, entry map[string]any) error {
	return nil
}
func (c *captureProxyLogger) LogFork(sessionID, provider string, fromSeq int, parentSession string) error {
	return nil
}
func (c *captureProxyLogger) Close() error { return nil }

func (c *captureProxyLogger) captured() []capturedResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedResponse, len(c.responses))
	copy(out, c.responses)
	return out
}

// syncRecorder is a threadsafe http.ResponseWriter+Flusher so the test can
// watch forwarded bytes while streamResponse is writing them.
type syncRecorder struct {
	mu     sync.Mutex
	header http.Header
	body   strings.Builder
}

func newSyncRecorder() *syncRecorder { return &syncRecorder{header: http.Header{}} }

func (s *syncRecorder) Header() http.Header { return s.header }
func (s *syncRecorder) WriteHeader(int)     {}
func (s *syncRecorder) Flush()              {}
func (s *syncRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(p)
}
func (s *syncRecorder) contains(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.body.String(), substr)
}

const terminalEventLine = `data: {"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":7}}}`

// startHeldOpenSSEUpstream serves one SSE response: it flushes a terminal
// event, then holds the stream open (no EOF) until release is closed.
func startHeldOpenSSEUpstream(t *testing.T, release chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		io.WriteString(w, "event: response.completed\n")
		io.WriteString(w, terminalEventLine+"\n")
		io.WriteString(w, "\n")
		fl.Flush()
		<-release
	}))
}

func TestStreamResponseClientCancelStillLogsCapturedChunks(t *testing.T) {
	release := make(chan struct{})
	upstream := startHeldOpenSSEUpstream(t, release)
	// Defers run LIFO: release must close (unblocking the handler) before
	// upstream.Close() waits on the connection going idle, or Close hangs.
	defer upstream.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	logger := &captureProxyLogger{}
	w := newSyncRecorder()

	// The "client" (w's owner) hangs up right after the terminal event has
	// been forwarded to it — the incident shape.
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if w.contains("response.completed") {
				cancel()
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	err = streamResponse(w, resp, logger, nil, "sess-1", "openai", 1, time.Now(), nil, "req-1", "/v1/responses", nil, "test@host", nil)
	if err == nil {
		t.Fatalf("expected a read error from the canceled stream")
	}

	got := logger.captured()
	if len(got) != 1 {
		t.Fatalf("captured %d response records, want 1 — an aborted stream must still log", len(got))
	}
	rec := got[0]
	if rec.capture.Termination != TerminationClientCancel {
		t.Errorf("termination = %q, want %q", rec.capture.Termination, TerminationClientCancel)
	}
	if rec.capture.TerminationError == "" {
		t.Errorf("termination_error should carry the read error")
	}
	if rec.capture.Path != "/v1/responses" {
		t.Errorf("path = %q, want /v1/responses", rec.capture.Path)
	}
	var joined strings.Builder
	for _, c := range rec.chunks {
		joined.WriteString(c.Raw)
	}
	if !strings.Contains(joined.String(), "response.completed") {
		t.Errorf("captured chunks must include the usage-bearing terminal event")
	}
}

func TestStreamResponseEOFLogsTerminationEOF(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, terminalEventLine+"\n\n")
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	logger := &captureProxyLogger{}
	err = streamResponse(newSyncRecorder(), resp, logger, nil, "sess-2", "openai", 1, time.Now(), nil, "req-2", "/v1/responses", nil, "test@host", nil)
	if err != nil {
		t.Fatalf("streamResponse on clean EOF: %v", err)
	}
	got := logger.captured()
	if len(got) != 1 || got[0].capture.Termination != TerminationEOF {
		t.Fatalf("want one record with termination eof, got %+v", got)
	}
	if got[0].capture.TerminationError != "" {
		t.Errorf("termination_error must be empty on eof")
	}
}

type countingEmitter struct {
	mu    sync.Mutex
	calls int
}

func (c *countingEmitter) EmitTurnStart(sessionID, provider, machine string, turnDepth int, errorRecovered bool) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
}
func (c *countingEmitter) EmitTurnEnd(sessionID, provider, machine, stopReason string, isRetry bool, errorType string, patterns PatternData, tokens TokenData) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
}
func (c *countingEmitter) EmitToolCall(sessionID, provider, machine, toolName string, toolIndex int, toolUseID string) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
}
func (c *countingEmitter) EmitToolResult(sessionID, provider, machine, toolName, toolUseID string, isError bool) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
}

func TestStreamResponseClientCancelEmitsNoAgentEvents(t *testing.T) {
	release := make(chan struct{})
	upstream := startHeldOpenSSEUpstream(t, release)
	// Defers run LIFO: release must close (unblocking the handler) before
	// upstream.Close() waits on the connection going idle, or Close hangs.
	defer upstream.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	w := newSyncRecorder()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if w.contains("response.completed") {
				cancel()
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	emitter := &countingEmitter{}
	sm, err := NewSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	_ = streamResponse(w, resp, &captureProxyLogger{}, sm, "sess-3", "openai", 1, time.Now(), nil, "req-3", "/v1/responses", emitter, "test@host", &PatternState{PendingToolIDs: map[string]string{}})

	emitter.mu.Lock()
	calls := emitter.calls
	emitter.mu.Unlock()
	if calls != 0 {
		t.Errorf("agent events on a canceled stream = %d, want 0 (emission stays clean-EOF-only)", calls)
	}
}

func TestUpstreamUnreachableLogsResponseLine(t *testing.T) {
	// Reserve a port and close the listener so the dial reliably fails.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadHost := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()

	logger := &captureProxyLogger{}
	p := NewProxyWithSessionManagerAndLogger(logger, nil)

	req := httptest.NewRequest(http.MethodPost, "/openai/"+deadHost+"/v1/chat/completions", strings.NewReader(`{"stream":false}`))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	got := logger.captured()
	if len(got) != 1 {
		t.Fatalf("captured %d response records, want 1 — a dial failure must pair the request line", len(got))
	}
	rec := got[0]
	if rec.status != 0 {
		t.Errorf("status = %d, want 0 (no upstream response existed)", rec.status)
	}
	if rec.capture.Termination != TerminationUpstreamUnreachable {
		t.Errorf("termination = %q, want %q", rec.capture.Termination, TerminationUpstreamUnreachable)
	}
	if rec.capture.TerminationError == "" {
		t.Errorf("termination_error should carry the dial error")
	}
}

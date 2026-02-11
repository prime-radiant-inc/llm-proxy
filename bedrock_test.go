package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// --- Step 3a: LimitedWriter and model ID validation ---

func TestLimitedWriter_BasicWrite(t *testing.T) {
	var buf bytes.Buffer
	lw := &LimitedWriter{W: &buf, N: 100}

	n, err := lw.Write([]byte("hello"))
	if n != 5 || err != nil {
		t.Errorf("Write() = (%d, %v), want (5, nil)", n, err)
	}
	if buf.String() != "hello" {
		t.Errorf("buffer = %q, want %q", buf.String(), "hello")
	}
	if lw.Overflow {
		t.Error("Overflow should be false")
	}
}

func TestLimitedWriter_OverflowDiscardsEntireChunk(t *testing.T) {
	var buf bytes.Buffer
	lw := &LimitedWriter{W: &buf, N: 10}

	// First write fits
	n, err := lw.Write([]byte("12345"))
	if n != 5 || err != nil {
		t.Fatalf("first Write() = (%d, %v)", n, err)
	}

	// Second write exceeds limit — entire chunk discarded
	n, err = lw.Write([]byte("1234567890"))
	if n != 10 || err != nil {
		t.Errorf("overflow Write() = (%d, %v), want (10, nil)", n, err)
	}
	if !lw.Overflow {
		t.Error("Overflow should be true after exceeding limit")
	}
	// Buffer should only contain first write
	if buf.String() != "12345" {
		t.Errorf("buffer = %q, want %q", buf.String(), "12345")
	}
}

func TestLimitedWriter_PostOverflowWritesDiscarded(t *testing.T) {
	var buf bytes.Buffer
	lw := &LimitedWriter{W: &buf, N: 5}

	lw.Write([]byte("1234567890")) // triggers overflow
	n, err := lw.Write([]byte("more data"))
	if n != 9 || err != nil {
		t.Errorf("post-overflow Write() = (%d, %v), want (9, nil)", n, err)
	}
	// Buffer should be empty (first write was too big, discarded)
	if buf.Len() != 0 {
		t.Errorf("buffer len = %d, want 0", buf.Len())
	}
}

func TestLimitedWriter_AlwaysReportsFullSuccess(t *testing.T) {
	// This is critical: io.TeeReader propagates Write errors to io.Copy.
	// LimitedWriter must NEVER return an error.
	var buf bytes.Buffer
	lw := &LimitedWriter{W: &buf, N: 0} // zero limit = immediate overflow

	n, err := lw.Write([]byte("data"))
	if n != 4 {
		t.Errorf("n = %d, want 4", n)
	}
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestExtractModelID_Valid(t *testing.T) {
	tests := []struct {
		path    string
		wantID  string
		wantErr bool
	}{
		{"/model/us.anthropic.claude-sonnet-4-5-20250929-v2:0/invoke-with-response-stream", "us.anthropic.claude-sonnet-4-5-20250929-v2:0", false},
		{"/model/anthropic.claude-3-haiku-20240307-v1:0/invoke", "anthropic.claude-3-haiku-20240307-v1:0", false},
		{"/model/us.anthropic.claude-haiku-4-5-20251001-v1:0/invoke-with-response-stream", "us.anthropic.claude-haiku-4-5-20251001-v1:0", false},
		{"/model/simple-model/invoke", "simple-model", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			id, err := extractModelID(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractModelID(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
			if id != tt.wantID {
				t.Errorf("extractModelID(%q) = %q, want %q", tt.path, id, tt.wantID)
			}
		})
	}
}

func TestExtractModelID_Invalid(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"empty model", "/model//invoke"},
		{"url encoded chars", "/model/foo%23bar/invoke"},
		{"spaces", "/model/foo bar/invoke"},
		{"query string injection", "/model/foo?bar=baz/invoke"},
		{"special chars", "/model/foo@bar/invoke"},
		{"no suffix", "/model/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := extractModelID(tt.path)
			if err == nil {
				t.Errorf("extractModelID(%q) should fail", tt.path)
			}
		})
	}
}

// --- Step 3b: Eventstream decoder ---

func TestDecodeBedrockEventstream_RealFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/bedrock-eventstream.bin")
	if err != nil {
		t.Fatalf("Failed to read test fixture: %v", err)
	}

	chunks, err := decodeBedrockEventstream(data)
	if err != nil {
		t.Fatalf("decodeBedrockEventstream() error = %v", err)
	}

	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	// Each chunk should have "data: " prefix for parser compatibility
	for i, c := range chunks {
		if len(c.Raw) < 6 || c.Raw[:6] != "data: " {
			t.Errorf("chunk[%d].Raw should start with 'data: ', got prefix %q", i, c.Raw[:min(10, len(c.Raw))])
		}
	}

	// Verify we get expected Anthropic event types
	foundMessageStart := false
	foundMessageStop := false
	for _, c := range chunks {
		raw := c.Raw[6:] // strip "data: "
		if bytes.Contains([]byte(raw), []byte(`"type":"message_start"`)) {
			foundMessageStart = true
		}
		if bytes.Contains([]byte(raw), []byte(`"type":"message_stop"`)) {
			foundMessageStop = true
		}
	}
	if !foundMessageStart {
		t.Error("expected message_start event in decoded chunks")
	}
	if !foundMessageStop {
		t.Error("expected message_stop event in decoded chunks")
	}
}

func TestDecodeBedrockEventstream_EmptyInput(t *testing.T) {
	chunks, err := decodeBedrockEventstream(nil)
	if err != nil {
		t.Errorf("expected no error for nil input, got %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for nil input, got %d", len(chunks))
	}

	chunks, err = decodeBedrockEventstream([]byte{})
	if err != nil {
		t.Errorf("expected no error for empty input, got %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for empty input, got %d", len(chunks))
	}
}

func TestDecodeBedrockEventstream_TruncatedInput(t *testing.T) {
	data, err := os.ReadFile("testdata/bedrock-eventstream.bin")
	if err != nil {
		t.Fatalf("Failed to read test fixture: %v", err)
	}

	// Truncate to partial frame — should return any complete frames decoded
	// before the error, plus a non-nil error
	truncated := data[:len(data)/2]
	chunks, err := decodeBedrockEventstream(truncated)
	// Should get some chunks from complete frames before the truncation
	if len(chunks) == 0 && err == nil {
		t.Error("expected either chunks or error from truncated input")
	}
}

// --- Step 3c: serveBedrock integration tests ---

// staticCredentials provides fixed AWS credentials for testing.
type staticCredentials struct{}

func (s staticCredentials) Retrieve(ctx context.Context) (aws.Credentials, error) {
	return aws.Credentials{
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Source:          "test",
	}, nil
}

// newTestBedrockProxy creates a Proxy with a mock Bedrock backend for testing.
// The mockHandler receives the proxied request after SigV4 signing.
func newTestBedrockProxy(t *testing.T, mockHandler http.HandlerFunc) (*Proxy, *httptest.Server) {
	t.Helper()

	mock := httptest.NewServer(mockHandler)

	tmpDir := t.TempDir()
	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })

	sm, err := NewSessionManager(tmpDir, logger)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { sm.Close() })

	proxy := &Proxy{
		client:         createPassthroughClient(),
		logger:         logger,
		sessionManager: sm,
		bedrock: &bedrockState{
			region:   "us-west-2",
			credProv: staticCredentials{},
			signer:   v4.NewSigner(),
			client: &http.Client{
				Transport: &http.Transport{
					DisableCompression: true,
				},
			},
			semaphore: make(chan struct{}, bedrockMaxConcurrent),
		},
	}

	return proxy, mock
}

func TestServeBedrock_StreamingRoundTrip(t *testing.T) {
	fixtureData, err := os.ReadFile("testdata/bedrock-eventstream.bin")
	if err != nil {
		t.Fatalf("Failed to read fixture: %v", err)
	}

	var receivedAuth string
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		w.Write(fixtureData)
	}))
	defer mock.Close()

	// Override the Bedrock client to point at our mock
	proxy.bedrock.client = mock.Client()
	// Override the upstream URL construction by swapping the region to point at mock
	// We'll need to intercept the URL — let's use a Transport that redirects
	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{
			target: mockHost,
			inner:  http.DefaultTransport,
		},
	}

	reqBody := `{"anthropic_version":"bedrock-2023-05-31","max_tokens":100,"messages":[{"role":"user","content":"Say hi"}],"metadata":{"user_id":"test_user_123"}}`
	req := httptest.NewRequest("POST", "/model/us.anthropic.claude-haiku-4-5-20251001-v1:0/invoke-with-response-stream", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	proxy.serveBedrock(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200. Body: %s", w.Code, w.Body.String())
	}

	// The response should be the raw eventstream bytes
	if w.Body.Len() != len(fixtureData) {
		t.Errorf("response body length = %d, want %d", w.Body.Len(), len(fixtureData))
	}

	// Verify SigV4 signing happened
	if receivedAuth == "" {
		t.Error("expected Authorization header from SigV4 signing")
	}
	if !strings.Contains(receivedAuth, "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization = %q, want AWS4-HMAC-SHA256 signature", receivedAuth)
	}
}

func TestServeBedrock_InvalidModelID_Returns400(t *testing.T) {
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("mock should not be called for invalid model ID")
	}))
	defer mock.Close()

	req := httptest.NewRequest("POST", "/model/foo%23bar/invoke", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	proxy.serveBedrock(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestServeBedrock_Non200Forwarded(t *testing.T) {
	errorBody := `{"message":"Rate limit exceeded"}`
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(errorBody))
	}))
	defer mock.Close()

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/model/us.anthropic.claude-haiku-4-5-20251001-v1:0/invoke-with-response-stream",
		strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.serveBedrock(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Rate limit exceeded") {
		t.Errorf("body = %q, want error message forwarded", w.Body.String())
	}
}

func TestServeBedrock_NonStreamingInvoke(t *testing.T) {
	responseBody := `{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"Hi!"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`

	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the path was preserved
		if !strings.HasPrefix(r.URL.Path, "/model/") {
			t.Errorf("upstream path = %q, want /model/ prefix", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(responseBody))
	}))
	defer mock.Close()

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/model/anthropic.claude-3-haiku-20240307-v1:0/invoke",
		strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.serveBedrock(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Hi!") {
		t.Errorf("response should contain 'Hi!', got %q", w.Body.String())
	}
}

func TestServeBedrock_NotConfigured(t *testing.T) {
	proxy := NewProxy()
	// bedrock is nil

	req := httptest.NewRequest("POST", "/model/foo/invoke", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	proxy.serveBedrock(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestServeBedrock_ConcurrencySemaphore(t *testing.T) {
	// Create proxy with semaphore size 1
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer mock.Close()
	proxy.bedrock.semaphore = make(chan struct{}, 1)

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	// Two concurrent requests — one should queue behind the other
	done := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			req := httptest.NewRequest("POST", "/model/simple/invoke",
				strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			proxy.serveBedrock(w, req)
			done <- w.Code
		}()
	}

	// Both should eventually complete
	for i := 0; i < 2; i++ {
		select {
		case code := <-done:
			if code != http.StatusOK {
				t.Errorf("request %d: status = %d, want 200", i, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for concurrent requests")
		}
	}
}

func TestServeBedrock_HeaderWhitelist(t *testing.T) {
	var receivedHeaders http.Header
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer mock.Close()

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/model/simple/invoke",
		strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Api-Key", "sk-should-not-be-forwarded")
	req.Header.Set("Anthropic-Version", "should-not-be-forwarded")

	w := httptest.NewRecorder()
	proxy.serveBedrock(w, req)

	// Content-Type and Accept should be forwarded
	if receivedHeaders.Get("Content-Type") != "application/json" {
		t.Error("Content-Type should be forwarded")
	}
	if receivedHeaders.Get("Accept") != "application/json" {
		t.Error("Accept should be forwarded")
	}
	// Anthropic-specific headers should NOT be forwarded
	if receivedHeaders.Get("X-Api-Key") != "" {
		t.Error("X-Api-Key should NOT be forwarded to Bedrock")
	}
	if receivedHeaders.Get("Anthropic-Version") != "" {
		t.Error("Anthropic-Version should NOT be forwarded to Bedrock")
	}
}

// rewriteTransport rewrites the request host to point at a test server.
type rewriteTransport struct {
	target string
	inner  http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = t.target
	req.Host = t.target
	return t.inner.RoundTrip(req)
}

// --- Step 3d: Proxy routing tests ---

func TestServeHTTP_BedrockRoutesCorrectly(t *testing.T) {
	// Bedrock paths should route to serveBedrock
	proxy := NewProxy()
	// No bedrock configured — should get 503
	req := httptest.NewRequest("POST", "/model/us.anthropic.claude-haiku-4-5-20251001-v1:0/invoke-with-response-stream",
		strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Bedrock path with no config: status = %d, want 503", w.Code)
	}
}

func TestServeHTTP_ExistingPathsUnchanged(t *testing.T) {
	// Non-Bedrock paths should still go through ParseProxyURL
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")
	proxy := NewProxy()

	req := httptest.NewRequest("POST", "/anthropic/"+upstreamHost+"/v1/messages",
		strings.NewReader(`{"test":"data"}`))
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("existing path: status = %d, want 200", w.Code)
	}
}

func TestIsConversationEndpoint_BedrockPaths(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{"/model/us.anthropic.claude-sonnet-4-5-20250929-v2:0/invoke-with-response-stream", true},
		{"/model/anthropic.claude-3-haiku-20240307-v1:0/invoke", true},
		{"/model/simple/invoke", true},
	}

	for _, tt := range tests {
		got := isConversationEndpoint(tt.path)
		if got != tt.expected {
			t.Errorf("isConversationEndpoint(%q) = %v, want %v", tt.path, got, tt.expected)
		}
	}
}

// --- Step 3e: Provider aliasing test ---

func TestServeBedrock_UsesAnthropicProvider(t *testing.T) {
	// Verify Bedrock traffic flows through provider=anthropic dispatch path.
	// This is critical for session tracking, fingerprinting, and Loki dashboards.
	var loggedProvider string
	responseBody := `{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"Hi"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`

	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(responseBody))
	}))
	defer mock.Close()

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	// Use a logger wrapper to capture the provider
	origLogger := proxy.logger
	proxy.logger = &providerCapture{inner: origLogger, capturedProvider: &loggedProvider}

	req := httptest.NewRequest("POST", "/model/us.anthropic.claude-haiku-4-5-20251001-v1:0/invoke",
		strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"test_user"}}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.serveBedrock(w, req)

	if loggedProvider != "anthropic" {
		t.Errorf("provider = %q, want 'anthropic' (Bedrock uses same provider for session tracking)", loggedProvider)
	}
}

// providerCapture wraps a ProxyLogger to capture the provider used in LogRequest.
type providerCapture struct {
	inner            ProxyLogger
	capturedProvider *string
}

func (pc *providerCapture) RegisterUpstream(sessionID, upstream string) {
	pc.inner.RegisterUpstream(sessionID, upstream)
}
func (pc *providerCapture) LogSessionStart(sessionID, provider, upstream string) error {
	*pc.capturedProvider = provider
	return pc.inner.LogSessionStart(sessionID, provider, upstream)
}
func (pc *providerCapture) LogRequest(sessionID, provider string, seq int, method, path string, headers http.Header, body []byte, requestID string) error {
	*pc.capturedProvider = provider
	return pc.inner.LogRequest(sessionID, provider, seq, method, path, headers, body, requestID)
}
func (pc *providerCapture) LogResponse(sessionID, provider string, seq int, status int, headers http.Header, body []byte, chunks []StreamChunk, timing ResponseTiming, requestID string) error {
	return pc.inner.LogResponse(sessionID, provider, seq, status, headers, body, chunks, timing, requestID)
}
func (pc *providerCapture) LogFork(sessionID, provider string, fromSeq int, parentSession string) error {
	return pc.inner.LogFork(sessionID, provider, fromSeq, parentSession)
}
func (pc *providerCapture) Close() error {
	return pc.inner.Close()
}

// Verify unused imports are actually needed
var _ io.Reader = (*bytes.Reader)(nil)
var _ context.Context = context.Background()

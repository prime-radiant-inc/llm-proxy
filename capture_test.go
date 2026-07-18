package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMeteringProviderFromUpstream(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"openrouter.ai", "openrouter"},
		{"api.openai.com", "openai"},
		{"api.anthropic.com", "anthropic"},
		{"bedrock-runtime.us-west-2.amazonaws.com", "anthropic"},
		{"chatgpt.com", "openai"},
		{"OPENROUTER.AI", "openrouter"},
		{" api.openai.com ", "openai"},
		{"bedrock-mantle.us-east-1.api.aws", ""},
		{"example.com", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := meteringProviderFromUpstream(tc.host); got != tc.want {
			t.Errorf("meteringProviderFromUpstream(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func readSessionLines(t *testing.T, baseDir, upstream, sessionID string) []map[string]any {
	t.Helper()
	dateStr := time.Now().Format("2006-01-02")
	path := filepath.Join(baseDir, upstream, dateStr, sessionID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("unmarshal line %q: %v", raw, err)
		}
		lines = append(lines, entry)
	}
	return lines
}

func TestLogRequestStampsCaptureFacts(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	const session = "sess-capture-req"
	const upstream = "openrouter.ai"
	if err := logger.LogSessionStart(session, "openai", upstream); err != nil {
		t.Fatalf("LogSessionStart: %v", err)
	}
	if err := logger.LogRequest(session, "openai", 1, "POST", "/api/v1/chat/completions", nil, []byte(`{}`), "req-1"); err != nil {
		t.Fatalf("LogRequest: %v", err)
	}

	lines := readSessionLines(t, dir, upstream, session)
	req := lines[1]
	if got := req["metering_provider"]; got != "openrouter" {
		t.Errorf("metering_provider = %v, want openrouter (host-derived, never the route segment)", got)
	}
	if got := req["upstream"]; got != upstream {
		t.Errorf("upstream = %v, want %v", got, upstream)
	}
	if got, ok := req["capture_version"].(float64); !ok || int(got) != CaptureVersion {
		t.Errorf("capture_version = %v, want %d", req["capture_version"], CaptureVersion)
	}
}

func TestLogResponseStampsCaptureFacts(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	const session = "sess-capture-resp"
	const upstream = "api.openai.com"
	if err := logger.LogSessionStart(session, "openai", upstream); err != nil {
		t.Fatalf("LogSessionStart: %v", err)
	}
	err = logger.LogResponse(session, "openai", 1, 200, nil, []byte(`{}`), nil, ResponseTiming{}, "req-1", ResponseCapture{
		Path:        "/v1/responses",
		Termination: TerminationEOF,
	})
	if err != nil {
		t.Fatalf("LogResponse: %v", err)
	}

	lines := readSessionLines(t, dir, upstream, session)
	resp := lines[1]
	if got := resp["path"]; got != "/v1/responses" {
		t.Errorf("path = %v, want /v1/responses", got)
	}
	if got := resp["termination"]; got != "eof" {
		t.Errorf("termination = %v, want eof", got)
	}
	if _, present := resp["termination_error"]; present {
		t.Errorf("termination_error should be omitted on eof")
	}
	if got := resp["metering_provider"]; got != "openai" {
		t.Errorf("metering_provider = %v, want openai", got)
	}
	if got, ok := resp["capture_version"].(float64); !ok || int(got) != CaptureVersion {
		t.Errorf("capture_version = %v, want %d", resp["capture_version"], CaptureVersion)
	}
}

func TestBedrockLegsStampTerminationEOF(t *testing.T) {
	// Test three Bedrock relay paths: (a) non-2xx error, (b) non-streaming 200, (c) streaming 200.
	// Each must log exactly one response with Termination=TerminationEOF and a non-empty Path.

	fixtureData, err := os.ReadFile("testdata/bedrock-eventstream.bin")
	if err != nil {
		t.Fatalf("Failed to read fixture: %v", err)
	}

	testCases := []struct {
		name    string
		path    string
		handler http.HandlerFunc
	}{
		{
			name: "non-200 error",
			path: "/model/us.anthropic.claude-haiku-4-5-20251001-v1:0/invoke",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"message":"Rate limit exceeded"}`))
			}),
		},
		{
			name: "non-streaming 200",
			path: "/model/anthropic.claude-3-haiku-20240307-v1:0/invoke",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"Hi"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
			}),
		},
		{
			name: "streaming 200",
			path: "/model/us.anthropic.claude-sonnet-4-5-20250929-v2:0/invoke-with-response-stream",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
				w.WriteHeader(http.StatusOK)
				w.Write(fixtureData)
			}),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// newTestBedrockProxy is defined in bedrock_test.go
			proxy, mock := newTestBedrockProxy(t, tc.handler)
			defer mock.Close()

			// Redirect mock Bedrock requests to our test server
			mockHost := strings.TrimPrefix(mock.URL, "http://")
			proxy.bedrock.client = &http.Client{
				Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
			}

			// Make request through proxy
			reqBody := `{"anthropic_version":"bedrock-2023-05-31","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest("POST", tc.path, strings.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			proxy.serveBedrock(w, req)

			// Read the captured logs
			logger := proxy.logger.(*Logger)
			entries := readObservationLogEntries(t, logger)

			// Find the response entry (should be second after session_start + request)
			var responseEntry map[string]any
			for _, entry := range entries {
				if entryType, _ := entry["type"].(string); entryType == "response" {
					responseEntry = entry
					break
				}
			}

			if responseEntry == nil {
				t.Fatalf("response entry not found in logs, entries: %v", entries)
			}

			// Verify termination=eof
			if got := responseEntry["termination"]; got != "eof" {
				t.Errorf("termination = %q, want eof", got)
			}

			// Verify path is set
			if got := responseEntry["path"]; got == "" {
				t.Errorf("path is empty, expected %q", tc.path)
			} else if got != tc.path {
				t.Errorf("path = %q, want %q", got, tc.path)
			}

			// Verify no termination_error on eof
			if _, present := responseEntry["termination_error"]; present {
				t.Errorf("termination_error should be omitted on eof")
			}
		})
	}
}

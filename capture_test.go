package main

import (
	"encoding/json"
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

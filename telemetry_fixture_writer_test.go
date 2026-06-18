package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type telemetryFixtureSpec struct {
	name          string
	requestID     string
	sessionID     string
	runID         string
	rejectedRunID string
	host          string
	baseTime      time.Time
	timingTTFBMs  int64
	timingTotalMs int64
	run           func(*testing.T) []map[string]any
}

func TestServeMantleWritesTelemetryContractFixtures(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("LLM_PROXY_WRITE_TELEMETRY_FIXTURES"))
	if root == "" {
		t.Skip("LLM_PROXY_WRITE_TELEMETRY_FIXTURES not set")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir fixture root: %v", err)
	}

	baseTime := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	specs := []telemetryFixtureSpec{
		{
			name:          "codex-success",
			requestID:     "00000000-0000-4000-8000-000000000001",
			sessionID:     "sess_example",
			runID:         "run-example",
			host:          "bedrock-mantle.us-west-2.api.aws",
			baseTime:      baseTime,
			timingTTFBMs:  12,
			timingTotalMs: 34,
			run: func(t *testing.T) []map[string]any {
				return runMantleNonStreamingFixture(
					t,
					"/cbrun/run-example/mantle/v1/responses",
					`{"model":"openai.gpt-5.5"}`,
					http.StatusOK,
					`{"id":"resp_success","object":"response","output":[],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`,
				)
			},
		},
		{
			name:          "codex-invalid-run",
			requestID:     "00000000-0000-4000-8000-000000000002",
			sessionID:     "sess_example",
			rejectedRunID: "-bad",
			host:          "bedrock-mantle.us-west-2.api.aws",
			baseTime:      baseTime.Add(10 * time.Second),
			run: func(t *testing.T) []map[string]any {
				return runMantleInvalidRunFixture(t)
			},
		},
		{
			name:          "codex-usage-missing",
			requestID:     "00000000-0000-4000-8000-000000000003",
			sessionID:     "sess_example",
			runID:         "run-example",
			host:          "bedrock-mantle.us-west-2.api.aws",
			baseTime:      baseTime.Add(20 * time.Second),
			timingTTFBMs:  13,
			timingTotalMs: 35,
			run: func(t *testing.T) []map[string]any {
				return runMantleNonStreamingFixture(
					t,
					"/cbrun/run-example/mantle/v1/responses",
					`{"model":"openai.gpt-5.5"}`,
					http.StatusOK,
					`{"id":"resp_missing","object":"response","output":[]}`,
				)
			},
		},
		{
			name:          "codex-unsupported-usage",
			requestID:     "00000000-0000-4000-8000-000000000004",
			sessionID:     "sess_example",
			runID:         "run-example",
			host:          "bedrock-mantle.us-west-2.api.aws",
			baseTime:      baseTime.Add(30 * time.Second),
			timingTTFBMs:  14,
			timingTotalMs: 36,
			run: func(t *testing.T) []map[string]any {
				return runMantleNonStreamingFixture(
					t,
					"/cbrun/run-example/mantle/v1/responses",
					`{"model":"openai.gpt-5.5"}`,
					http.StatusOK,
					`{"id":"resp_bad_usage","object":"response","output":[],"usage":{"input_tokens":"ten","output_tokens":[],"total_tokens":{}}}`,
				)
			},
		},
		{
			name:          "codex-streaming-success",
			requestID:     "00000000-0000-4000-8000-000000000005",
			sessionID:     "sess_example",
			runID:         "run-example",
			host:          "bedrock-mantle.us-west-2.api.aws",
			baseTime:      baseTime.Add(40 * time.Second),
			timingTTFBMs:  15,
			timingTotalMs: 37,
			run: func(t *testing.T) []map[string]any {
				return runMantleStreamingFixture(
					t,
					"/cbrun/run-example/mantle/v1/responses",
					`{"model":"openai.gpt-5.5","stream":true}`,
					[]string{
						`data: {"type":"response.created"}`,
						`data: {"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`,
					},
				)
			},
		},
		{
			name:          "codex-streaming-failed-no-usage",
			requestID:     "00000000-0000-4000-8000-000000000006",
			sessionID:     "sess_example",
			runID:         "run-example",
			host:          "bedrock-mantle.us-west-2.api.aws",
			baseTime:      baseTime.Add(50 * time.Second),
			timingTTFBMs:  16,
			timingTotalMs: 38,
			run: func(t *testing.T) []map[string]any {
				return runMantleStreamingFixture(
					t,
					"/cbrun/run-example/mantle/v1/responses",
					`{"model":"openai.gpt-5.5","stream":true}`,
					[]string{
						`data: {"type":"response.created"}`,
						`data: {"type":"response.failed","error":{"message":"fixture failure"}}`,
					},
				)
			},
		},
		{
			name:          "codex-streaming-incomplete-null-usage",
			requestID:     "00000000-0000-4000-8000-000000000007",
			sessionID:     "sess_example",
			runID:         "run-example",
			host:          "bedrock-mantle.us-west-2.api.aws",
			baseTime:      baseTime.Add(60 * time.Second),
			timingTTFBMs:  17,
			timingTotalMs: 39,
			run: func(t *testing.T) []map[string]any {
				return runMantleStreamingFixture(
					t,
					"/cbrun/run-example/mantle/v1/responses",
					`{"model":"openai.gpt-5.5","stream":true}`,
					[]string{
						`data: {"type":"response.created"}`,
						`data: {"type":"response.completed","response":{"status":"incomplete","usage":null}}`,
					},
				)
			},
		},
		{
			name:          "codex-non2xx",
			requestID:     "00000000-0000-4000-8000-000000000008",
			sessionID:     "sess_example",
			runID:         "run-example",
			host:          "bedrock-mantle.us-west-2.api.aws",
			baseTime:      baseTime.Add(70 * time.Second),
			timingTTFBMs:  18,
			timingTotalMs: 40,
			run: func(t *testing.T) []map[string]any {
				return runMantleNonStreamingFixture(
					t,
					"/cbrun/run-example/mantle/v1/responses",
					`{"model":"openai.gpt-5.5"}`,
					http.StatusTooManyRequests,
					`{"error":{"message":"slow down"}}`,
				)
			},
		},
		{
			name:          "codex-streaming-missing-completed",
			requestID:     "00000000-0000-4000-8000-000000000009",
			sessionID:     "sess_example",
			runID:         "run-example",
			host:          "bedrock-mantle.us-west-2.api.aws",
			baseTime:      baseTime.Add(80 * time.Second),
			timingTTFBMs:  19,
			timingTotalMs: 41,
			run: func(t *testing.T) []map[string]any {
				return runMantleStreamingFixture(
					t,
					"/cbrun/run-example/mantle/v1/responses",
					`{"model":"openai.gpt-5.5","stream":true}`,
					[]string{
						`data: {"type":"response.created"}`,
						`data: {"type":"response.output_text.delta","delta":"fixture"}`,
					},
				)
			},
		},
		{
			name:          "claude-bedrock-parity",
			requestID:     "00000000-0000-4000-8000-000000000010",
			sessionID:     "sess_claude_bedrock",
			runID:         "run-claude-bedrock",
			host:          "bedrock-runtime.us-west-2.amazonaws.com",
			baseTime:      baseTime.Add(90 * time.Second),
			timingTTFBMs:  21,
			timingTotalMs: 42,
			run: func(t *testing.T) []map[string]any {
				return runBedrockParityFixture(t)
			},
		},
	}

	for _, spec := range specs {
		spec := spec
		t.Run(spec.name, func(t *testing.T) {
			entries := normalizeFixtureEntries(spec.run(t), spec)
			writeFixtureJSONL(t, filepath.Join(root, spec.name+".jsonl"), entries)
		})
	}
}

func runMantleInvalidRunFixture(t *testing.T) []map[string]any {
	t.Helper()

	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("invalid run fixture must not hit upstream")
	}))
	defer mock.Close()

	req := httptest.NewRequest("POST", "/cbrun/-bad/mantle/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	return readObservationLogEntries(t, proxy.logger.(*Logger))
}

func runMantleNonStreamingFixture(t *testing.T, path, requestBody string, status int, responseBody string) []map[string]any {
	t.Helper()

	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(responseBody))
	}))
	defer mock.Close()

	proxy.tokenSub = newSub(t, writeScript(t, `printf '%s\n' '{"token":"REAL-BEARER","run_id":"run-example"}'`))
	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", path, strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)
	if w.Code != status {
		t.Fatalf("status = %d, want %d", w.Code, status)
	}
	return readObservationLogEntries(t, proxy.logger.(*Logger))
}

func runMantleStreamingFixture(t *testing.T, path, requestBody string, events []string) []map[string]any {
	t.Helper()

	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, event := range events {
			_, _ = w.Write([]byte(event + "\n\n"))
		}
	}))
	defer mock.Close()

	proxy.tokenSub = newSub(t, writeScript(t, `printf '%s\n' '{"token":"REAL-BEARER","run_id":"run-example"}'`))
	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", path, strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	return readObservationLogEntries(t, proxy.logger.(*Logger))
}

func runBedrockParityFixture(t *testing.T) []map[string]any {
	t.Helper()

	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_fixture","type":"message","role":"assistant","model":"anthropic.claude-3-haiku-20240307-v1:0","content":[],"usage":{"input_tokens":6,"output_tokens":4}}`))
	}))
	defer mock.Close()

	proxy.tokenSub = newSub(t, writeScript(t, `printf '%s\n' '{"token":"REAL-BEARER","run_id":"run-claude-bedrock"}'`))
	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/model/anthropic.claude-3-haiku-20240307-v1:0/invoke", strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":1,"messages":[],"metadata":{"run_id":"run-claude-bedrock"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()
	proxy.serveBedrock(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	return readObservationLogEntries(t, proxy.logger.(*Logger))
}

func normalizeFixtureEntries(entries []map[string]any, spec telemetryFixtureSpec) []map[string]any {
	normalized := make([]map[string]any, 0, len(entries))
	for i, entry := range entries {
		entry = cloneJSONMap(entry)
		meta, _ := entry["_meta"].(map[string]any)
		if meta != nil {
			meta["ts"] = spec.baseTime.Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339Nano)
			if entryType, _ := entry["type"].(string); entryType != "session_start" {
				meta["request_id"] = spec.requestID
			}
			meta["session"] = spec.sessionID
			delete(meta, "machine")
			meta["host"] = spec.host
			if _, ok := meta["cloud_build_run_id"]; ok {
				meta["cloud_build_run_id"] = spec.runID
			}
			if _, ok := meta["rejected_run_id"]; ok && spec.rejectedRunID != "" {
				meta["rejected_run_id"] = spec.rejectedRunID
			}
		}

		switch entry["type"] {
		case "request":
			normalizeRequestEntry(entry)
		case "response":
			normalizeResponseEntry(entry, spec)
		case "session_start":
			entry["upstream"] = spec.host
		}

		normalized = append(normalized, entry)
	}
	return normalized
}

func normalizeRequestEntry(entry map[string]any) {
	if request, ok := entry["request"].(map[string]any); ok {
		if body, ok := request["body"].(map[string]any); ok {
			delete(body, "input")
		}
		return
	}

	if headers, ok := entry["headers"].(map[string]any); ok {
		delete(headers, "Authorization")
	}

	if body, ok := entry["body"].(string); ok {
		var raw map[string]any
		if err := json.Unmarshal([]byte(body), &raw); err == nil {
			if _, ok := raw["messages"]; ok {
				raw["messages"] = []any{}
			}
			encoded, err := json.Marshal(raw)
			if err == nil {
				entry["body"] = string(encoded)
			}
		}
	}
}

func normalizeResponseEntry(entry map[string]any, spec telemetryFixtureSpec) {
	if timing, ok := entry["timing"].(map[string]any); ok {
		timing["ttfb_ms"] = spec.timingTTFBMs
		timing["total_ms"] = spec.timingTotalMs
	}

	if headers, ok := entry["headers"].(map[string]any); ok {
		delete(headers, "Date")
		delete(headers, "Content-Length")
	}

	if response, ok := entry["response"].(map[string]any); ok {
		normalizeStreamChunks(response, spec.baseTime)
		return
	}

	if chunks, ok := entry["chunks"].([]any); ok {
		for i, chunk := range chunks {
			chunkMap, _ := chunk.(map[string]any)
			if chunkMap == nil {
				continue
			}
			chunkMap["ts"] = spec.baseTime.Add(time.Duration(i+1) * 100 * time.Millisecond).UTC().Format(time.RFC3339Nano)
			chunkMap["delta_ms"] = int64((i + 1) * 5)
		}
	}
}

func normalizeStreamChunks(response map[string]any, baseTime time.Time) {
	chunks, _ := response["chunks"].([]any)
	for i, chunk := range chunks {
		chunkMap, _ := chunk.(map[string]any)
		if chunkMap == nil {
			continue
		}
		chunkMap["ts"] = baseTime.Add(time.Duration(i+1) * 100 * time.Millisecond).UTC().Format(time.RFC3339Nano)
		chunkMap["delta_ms"] = int64((i + 1) * 5)
	}
}

func writeFixtureJSONL(t *testing.T, path string, entries []map[string]any) {
	t.Helper()

	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal %s: %v", path, err)
		}
		lines = append(lines, string(data))
	}
	payload := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
	for _, forbidden := range []string{"REAL-BEARER", "real-bearer", "inbound-nonce", "Bearer "} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("fixture %s leaked forbidden token %q", path, forbidden)
		}
	}
}

func cloneJSONMap(in map[string]any) map[string]any {
	data, err := json.Marshal(in)
	if err != nil {
		panic(fmt.Sprintf("marshal clone: %v", err))
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(fmt.Sprintf("unmarshal clone: %v", err))
	}
	return out
}

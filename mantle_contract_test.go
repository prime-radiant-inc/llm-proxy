package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCountingResolver(t *testing.T, output string) (*APITokenSubstituter, string) {
	t.Helper()

	counter := filepath.Join(t.TempDir(), "resolver-count")
	script := writeScript(t, `echo x >> `+counter+`; printf '%s\n' '`+output+`'`)
	return newSub(t, script), counter
}

func readResolverCalls(t *testing.T, counter string) int {
	t.Helper()

	data, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read resolver count: %v", err)
	}
	return strings.Count(strings.TrimSpace(string(data)), "x")
}

func readObservationLogEntries(t *testing.T, logger *Logger) []map[string]any {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(logger.baseDir, "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob log entries: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 log file, got %d (%v)", len(paths), paths)
	}

	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}

	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("unmarshal log entry: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestServeMantleRejectsBarePathWhenRunIDRequired(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
	proxy.tokenSub = counterSub
	proxy.mantleRequireCloudBuildRunID = true

	req := httptest.NewRequest("POST", "/mantle/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 0 {
		t.Fatalf("resolver should not run for bare /mantle path when run id is required")
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestServeMantleRejectsInvalidCBRunIDBeforeResolver(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
	proxy.tokenSub = counterSub

	req := httptest.NewRequest("POST", "/cbrun/-bad/mantle/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 0 {
		t.Fatalf("resolver should not run for invalid cloud build run id")
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	if got := entries[0]["type"]; got != "error" {
		t.Fatalf("entry type = %v, want error", got)
	}
}

func TestServeMantleRejectsEncodedSlashRunIDBeforeResolver(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
	proxy.tokenSub = counterSub

	req := httptest.NewRequest("POST", "/cbrun/run%2F123/mantle/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 0 {
		t.Fatalf("resolver should not run for encoded slash in cloud build run id")
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestServeMantleRejectsEncodedTraversalSuffixBeforeResolver(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
	proxy.tokenSub = counterSub
	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/cbrun/run-123/mantle/v1/%2e%2e/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 0 {
		t.Fatalf("resolver should not run for encoded traversal in mantle suffix")
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestServeMantleRejectsNonExactRunScopedPathBeforeResolver(t *testing.T) {
	cases := []string{
		"/cbrun/run-123/mantle/v1/responses/extra",
		"/cbrun/run-123/mantle/v1/chat/completions",
	}

	for _, reqPath := range cases {
		t.Run(reqPath, func(t *testing.T) {
			upstreamCalls := 0
			proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls++
			}))
			defer mock.Close()

			counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
			proxy.tokenSub = counterSub
			mockHost := strings.TrimPrefix(mock.URL, "http://")
			proxy.bedrock.client = &http.Client{
				Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
			}

			req := httptest.NewRequest("POST", reqPath, strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer inbound-nonce")
			w := httptest.NewRecorder()

			proxy.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
			}
			if readResolverCalls(t, counter) != 0 {
				t.Fatalf("resolver should not run for non-exact mantle path %q", reqPath)
			}
			if upstreamCalls != 0 {
				t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
			}
		})
	}
}

func TestServeMantleRejectsUnresolvedRunIDBeforeUpstream(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-other"}`)
	proxy.tokenSub = counterSub

	req := httptest.NewRequest("POST", "/cbrun/run-123/mantle/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 1 {
		t.Fatalf("resolver calls = %d, want 1", readResolverCalls(t, counter))
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	meta := entries[0]["_meta"].(map[string]any)
	if got := meta["cloud_build_run_id"]; got != "run-123" {
		t.Fatalf("_meta.cloud_build_run_id = %v, want run-123", got)
	}
	errorInfo := entries[0]["error"].(map[string]any)
	if got := errorInfo["class"]; got != "unresolved_run_id" {
		t.Fatalf("error.class = %v, want unresolved_run_id", got)
	}
}

func TestServeMantleWritesTelemetryContractObservationNonStreaming(t *testing.T) {
	var receivedPath, receivedAuth string
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp_123","object":"response","output":[],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`))
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-test"}`)
	proxy.tokenSub = counterSub

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/cbrun/run-test/mantle/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 1 {
		t.Fatalf("resolver calls = %d, want 1", readResolverCalls(t, counter))
	}
	if receivedPath != "/openai/v1/responses" {
		t.Fatalf("upstream path = %q, want /openai/v1/responses", receivedPath)
	}
	if receivedAuth != "Bearer REAL-BEARER" {
		t.Fatalf("upstream auth = %q, want %q", receivedAuth, "Bearer REAL-BEARER")
	}

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}

	requestEntry := entries[0]
	if got := requestEntry["type"]; got != "request" {
		t.Fatalf("request entry type = %v, want request", got)
	}
	requestMeta := requestEntry["_meta"].(map[string]any)
	if got := requestMeta["schema_version"]; got != "telemetry-contract-v0" {
		t.Fatalf("request schema_version = %v, want telemetry-contract-v0", got)
	}
	if got := requestMeta["cloud_build_run_id"]; got != "run-test" {
		t.Fatalf("request cloud_build_run_id = %v, want run-test", got)
	}
	if got := requestMeta["provider"]; got != "openai" {
		t.Fatalf("request provider = %v, want openai", got)
	}
	if got := requestMeta["provider_route"]; got != "bedrock-mantle" {
		t.Fatalf("request provider_route = %v, want bedrock-mantle", got)
	}
	if got := requestMeta["wire_api"]; got != "openai-responses" {
		t.Fatalf("request wire_api = %v, want openai-responses", got)
	}
	if got := requestMeta["model_raw"]; got != "openai.gpt-5.5" {
		t.Fatalf("request model_raw = %v, want openai.gpt-5.5", got)
	}
	if got := requestMeta["model_normalized"]; got != "openai.gpt-5.5" {
		t.Fatalf("request model_normalized = %v, want openai.gpt-5.5", got)
	}
	requestInfo := requestEntry["request"].(map[string]any)
	if got := requestInfo["ingress_path"]; got != "/cbrun/run-test/mantle/v1/responses" {
		t.Fatalf("request.ingress_path = %v, want /cbrun/run-test/mantle/v1/responses", got)
	}
	if got := requestInfo["proxy_route"]; got != "/mantle/v1/responses" {
		t.Fatalf("request.proxy_route = %v, want /mantle/v1/responses", got)
	}
	if got := requestInfo["upstream_path"]; got != "/openai/v1/responses" {
		t.Fatalf("request.upstream_path = %v, want /openai/v1/responses", got)
	}

	responseEntry := entries[1]
	if got := responseEntry["type"]; got != "response" {
		t.Fatalf("response entry type = %v, want response", got)
	}
	responseMeta := responseEntry["_meta"].(map[string]any)
	if got := responseMeta["request_id"]; got != requestMeta["request_id"] {
		t.Fatalf("response request_id = %v, want %v", got, requestMeta["request_id"])
	}
	responseInfo := responseEntry["response"].(map[string]any)
	body := responseInfo["body"].(map[string]any)
	usage := body["usage"].(map[string]any)
	if got := usage["input_tokens"]; got != float64(10) {
		t.Fatalf("usage.input_tokens = %v, want 10", got)
	}
	if got := usage["output_tokens"]; got != float64(20) {
		t.Fatalf("usage.output_tokens = %v, want 20", got)
	}
	if got := usage["total_tokens"]; got != float64(30) {
		t.Fatalf("usage.total_tokens = %v, want 30", got)
	}
}

func TestServeMantleForwardsPercentEncodedSafeRunID(t *testing.T) {
	var receivedPath, receivedAuth string
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp_123","object":"response","output":[]}`))
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
	proxy.tokenSub = counterSub

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/cbrun/run%2D123/mantle/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 1 {
		t.Fatalf("resolver calls = %d, want 1", readResolverCalls(t, counter))
	}
	if receivedPath != "/openai/v1/responses" {
		t.Fatalf("upstream path = %q, want /openai/v1/responses", receivedPath)
	}
	if receivedAuth != "Bearer REAL-BEARER" {
		t.Fatalf("upstream auth = %q, want %q", receivedAuth, "Bearer REAL-BEARER")
	}
}

func TestServeMantleLogsNon2xxResponseObservation(t *testing.T) {
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-test"}`)
	proxy.tokenSub = counterSub

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/cbrun/run-test/mantle/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 1 {
		t.Fatalf("resolver calls = %d, want 1", readResolverCalls(t, counter))
	}

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	responseEntry := entries[1]
	if got := responseEntry["type"]; got != "response" {
		t.Fatalf("response entry type = %v, want response", got)
	}
	if got := responseEntry["status"]; got != float64(http.StatusTooManyRequests) {
		t.Fatalf("response status = %v, want 429", got)
	}
}

func TestServeMantleForwardsRunScopedPath(t *testing.T) {
	var receivedPath, receivedAuth string
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp_123","object":"response","output":[]}`))
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
	proxy.tokenSub = counterSub

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/cbrun/run-123/mantle/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 1 {
		t.Fatalf("resolver calls = %d, want 1", readResolverCalls(t, counter))
	}
	if receivedPath != "/openai/v1/responses" {
		t.Fatalf("upstream path = %q, want /openai/v1/responses", receivedPath)
	}
	if receivedAuth != "Bearer REAL-BEARER" {
		t.Fatalf("upstream auth = %q, want %q", receivedAuth, "Bearer REAL-BEARER")
	}
}

func TestServeMantleLogsStreamingObservationWithUsage(t *testing.T) {
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"type\":\"response.created\"}\n\n"))
		w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"total_tokens\":18}}}\n\n"))
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-test"}`)
	proxy.tokenSub = counterSub

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/cbrun/run-test/mantle/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 1 {
		t.Fatalf("resolver calls = %d, want 1", readResolverCalls(t, counter))
	}
	if body := w.Body.String(); !strings.Contains(body, "response.completed") {
		t.Fatalf("stream body = %q, want completed SSE event", body)
	}

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	responseEntry := entries[1]
	responseInfo := responseEntry["response"].(map[string]any)
	chunks := responseInfo["chunks"].([]any)
	if len(chunks) != 4 {
		t.Fatalf("logged chunks = %d, want 4", len(chunks))
	}
	lastChunk := chunks[2].(map[string]any)
	if raw := lastChunk["raw"]; raw != "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"total_tokens\":18}}}\n" {
		t.Fatalf("last data chunk raw = %q", raw)
	}
	body := responseInfo["body"].(map[string]any)
	usage := body["usage"].(map[string]any)
	if got := usage["input_tokens"]; got != float64(11) {
		t.Fatalf("usage.input_tokens = %v, want 11", got)
	}
	if got := usage["output_tokens"]; got != float64(7) {
		t.Fatalf("usage.output_tokens = %v, want 7", got)
	}
	if got := usage["total_tokens"]; got != float64(18) {
		t.Fatalf("usage.total_tokens = %v, want 18", got)
	}
}

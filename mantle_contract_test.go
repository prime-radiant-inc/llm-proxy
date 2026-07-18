package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

	_, data := readObservationLogFile(t, logger)

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

func readObservationLogFile(t *testing.T, logger *Logger) (string, []byte) {
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
	return paths[0], data
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

func TestServeMantleRunEnvelopeAllowsStaleResolverRunID(t *testing.T) {
	var receivedPath string
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"resolved-other-run"}`)
	proxy.tokenSub = counterSub

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/runs/run-test/mantle/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5","input":"hi"}`))
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

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	meta := findLogEntryByType(t, entries, "request")["_meta"].(map[string]any)
	assertMetaString(t, meta, "run_id", "run-test")
	if _, ok := meta["cloud_build_run_id"]; ok {
		t.Fatalf("canonical mantle emitted legacy key: %#v", meta)
	}
}

func TestServeMantleRunEnvelopeProjectMismatchRejected(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","project":"proj","run_id":"resolved-other-run"}`)
	proxy.tokenSub = counterSub

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/runs/other-run/mantle/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5","input":"hi"}`))
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
	if paths, _ := filepath.Glob(filepath.Join(proxy.logger.(*Logger).baseDir, "*", "*", "*.jsonl")); len(paths) != 0 {
		t.Fatalf("project mismatch produced log files: %v", paths)
	}
}

func TestServeMantleRunEnvelopeEmptyProjectAllowsStaleResolverRunID(t *testing.T) {
	var receivedPath string
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","project":"","run_id":"resolved-other-run"}`)
	proxy.tokenSub = counterSub

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/runs/other-run/mantle/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5","input":"hi"}`))
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

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	meta := findLogEntryByType(t, entries, "request")["_meta"].(map[string]any)
	assertMetaString(t, meta, "run_id", "other-run")
	if _, ok := meta["cloud_build_run_id"]; ok {
		t.Fatalf("canonical mantle emitted legacy key: %#v", meta)
	}
}

func TestServeMantleRunEnvelopeRejectsLegacyModelRouteBeforeResolver(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-test"}`)
	proxy.tokenSub = counterSub

	req := httptest.NewRequest("POST", "/cbrun/run-test/model/anthropic.claude-3-haiku-20240307-v1:0/invoke", strings.NewReader(`{"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 0 {
		t.Fatalf("resolver should not run for legacy /cbrun model route")
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestServeMantleLogsTokenSubstitutionFailureObservationWithoutSecrets(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counter := filepath.Join(t.TempDir(), "resolver-count")
	proxy.tokenSub = newSub(t, writeScript(t, `echo x >> `+counter+`
exit 3`))

	req := httptest.NewRequest("POST", "/cbrun/run-test/mantle/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401. body=%s", w.Code, w.Body.String())
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
	entry := entries[0]
	if got := entry["type"]; got != "error" {
		t.Fatalf("entry type = %v, want error", got)
	}
	if got := entry["status"]; got != float64(http.StatusUnauthorized) {
		t.Fatalf("entry status = %v, want 401", got)
	}
	errorInfo := entry["error"].(map[string]any)
	if got := errorInfo["class"]; got != "token_substitution_failed" {
		t.Fatalf("error.class = %v, want token_substitution_failed", got)
	}

	logPath, rawLog := readObservationLogFile(t, proxy.logger.(*Logger))
	for _, forbidden := range []string{"inbound-nonce", "Bearer", "REAL-BEARER"} {
		if strings.Contains(string(rawLog), forbidden) {
			t.Fatalf("log file %s should not contain %q: %s", logPath, forbidden, string(rawLog))
		}
	}
}

func TestServeMantleLogsUpstreamTransportErrorObservation(t *testing.T) {
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not be called when custom RoundTripper is installed")
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-test"}`)
	proxy.tokenSub = counterSub
	proxy.bedrock.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("dial boom")
		}),
	}

	req := httptest.NewRequest("POST", "/cbrun/run-test/mantle/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 1 {
		t.Fatalf("resolver calls = %d, want 1", readResolverCalls(t, counter))
	}

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	requestMeta := entries[0]["_meta"].(map[string]any)
	errorEntry := entries[1]
	if got := errorEntry["type"]; got != "response" {
		t.Fatalf("terminal entry type = %v, want response", got)
	}
	if got := errorEntry["status"]; got != float64(http.StatusBadGateway) {
		t.Fatalf("terminal entry status = %v, want 502", got)
	}
	if got := errorEntry["termination"]; got != TerminationUpstreamUnreachable {
		t.Fatalf("terminal entry termination = %v, want %v", got, TerminationUpstreamUnreachable)
	}
	errorMeta := errorEntry["_meta"].(map[string]any)
	if got := errorMeta["request_id"]; got != requestMeta["request_id"] {
		t.Fatalf("error request_id = %v, want %v", got, requestMeta["request_id"])
	}
	if got := errorMeta["cloud_build_run_id"]; got != "run-test" {
		t.Fatalf("error cloud_build_run_id = %v, want run-test", got)
	}
	errorInfo := errorEntry["error"].(map[string]any)
	if got := errorInfo["class"]; got != "upstream_request_failed" {
		t.Fatalf("error.class = %v, want upstream_request_failed", got)
	}

	_, rawLog := readObservationLogFile(t, proxy.logger.(*Logger))
	for _, forbidden := range []string{"inbound-nonce", "Bearer", "REAL-BEARER"} {
		if strings.Contains(string(rawLog), forbidden) {
			t.Fatalf("log should not contain %q: %s", forbidden, string(rawLog))
		}
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

	req := httptest.NewRequest("POST", "/cbrun/run-test/mantle/v1/responses?trace=1&prompt=two+words", strings.NewReader(`{"model":"openai.gpt-5.5","input":"hi"}`))
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
	if got := requestInfo["raw_query"]; got != "trace=1&prompt=two+words" {
		t.Fatalf("request.raw_query = %v, want trace=1&prompt=two+words", got)
	}
	if got := requestInfo["upstream_host"]; got != "bedrock-mantle.us-west-2.api.aws" {
		t.Fatalf("request.upstream_host = %v, want bedrock-mantle.us-west-2.api.aws", got)
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

func TestServeMantleNonStreamingPassThroughBeforeDrain(t *testing.T) {
	const fullBody = `{"id":"resp_123","object":"response","output":[{"type":"message","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`

	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not be called when custom RoundTripper is installed")
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-test"}`)
	proxy.tokenSub = counterSub

	releaseRest := make(chan struct{})
	upstreamBody := &stagedReadCloser{
		first: []byte(fullBody[:48]),
		rest:  []byte(fullBody[48:]),
		wait:  releaseRest,
	}
	proxy.bedrock.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
					"X-Upstream":   []string{"mantle"},
				},
				Body: upstreamBody,
			}, nil
		}),
	}

	req := httptest.NewRequest("POST", "/cbrun/run-test/mantle/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")

	rw := newProbeResponseWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		proxy.ServeHTTP(rw, req)
	}()

	waitForSignal(t, rw.headerWritten, releaseRest, done, "expected headers/status before upstream body drained")
	if rw.status != http.StatusAccepted {
		close(releaseRest)
		<-done
		t.Fatalf("status = %d, want 202", rw.status)
	}
	if got := rw.Header().Get("X-Upstream"); got != "mantle" {
		close(releaseRest)
		<-done
		t.Fatalf("X-Upstream header = %q, want mantle", got)
	}

	waitForSignal(t, rw.bodyWritten, releaseRest, done, "expected response body bytes before upstream body drained")
	if got := rw.BodyString(); !strings.Contains(got, `{"id":"resp_123"`) {
		close(releaseRest)
		<-done
		t.Fatalf("partial body = %q, want first chunk", got)
	}

	close(releaseRest)
	<-done

	if readResolverCalls(t, counter) != 1 {
		t.Fatalf("resolver calls = %d, want 1", readResolverCalls(t, counter))
	}
	if got := rw.BodyString(); got != fullBody {
		t.Fatalf("full body = %q, want %q", got, fullBody)
	}

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	responseBody := entries[1]["response"].(map[string]any)["body"].(map[string]any)
	usage := responseBody["usage"].(map[string]any)
	if got := usage["total_tokens"]; got != float64(30) {
		t.Fatalf("usage.total_tokens = %v, want 30", got)
	}
}

func TestServeMantleNonStreamingCopyErrorStampsNonEOFTermination(t *testing.T) {
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not be called when custom RoundTripper is installed")
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-test"}`)
	proxy.tokenSub = counterSub
	proxy.bedrock.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       &errAfterDataReadCloser{data: []byte(`{"id":"resp_123"`)},
			}, nil
		}),
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

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	entry := entries[1]
	if got := entry["type"]; got != "response" {
		t.Fatalf("entry type = %v, want response", got)
	}
	if got := entry["termination"]; got != TerminationUpstreamError {
		t.Fatalf("termination = %v, want %v (copy error mid-relay, no client cancel)", got, TerminationUpstreamError)
	}
}

func TestServeMantleLogsStreamingReadErrorObservation(t *testing.T) {
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not be called when custom RoundTripper is installed")
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-test"}`)
	proxy.tokenSub = counterSub
	proxy.bedrock.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &errAfterDataReadCloser{data: []byte("data: {\"type\":\"response.created\"}\n")},
			}, nil
		}),
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
	if body := w.Body.String(); !strings.Contains(body, "response.created") {
		t.Fatalf("stream body = %q, want partial SSE event", body)
	}
	if readResolverCalls(t, counter) != 1 {
		t.Fatalf("resolver calls = %d, want 1", readResolverCalls(t, counter))
	}

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	errorEntry := entries[1]
	if got := errorEntry["type"]; got != "response" {
		t.Fatalf("terminal entry type = %v, want response", got)
	}
	if got := errorEntry["status"]; got != float64(http.StatusOK) {
		t.Fatalf("terminal entry status = %v, want 200", got)
	}
	if got := errorEntry["termination"]; got != TerminationUpstreamError {
		t.Fatalf("terminal entry termination = %v, want %v", got, TerminationUpstreamError)
	}
	errorInfo := errorEntry["error"].(map[string]any)
	if got := errorInfo["class"]; got != "upstream_stream_read_failed" {
		t.Fatalf("error.class = %v, want upstream_stream_read_failed", got)
	}
	responseInfo := errorEntry["response"].(map[string]any)
	chunks := responseInfo["chunks"].([]any)
	if len(chunks) != 1 {
		t.Fatalf("logged chunks = %d, want 1", len(chunks))
	}
}

func TestMantleUpstreamStreamReadFailureEmitsResponseRecordWithTermination(t *testing.T) {
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not be called when custom RoundTripper is installed")
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-test"}`)
	proxy.tokenSub = counterSub
	proxy.bedrock.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &errAfterDataReadCloser{data: []byte("data: {\"type\":\"response.created\"}\n")},
			}, nil
		}),
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

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	entry := entries[1]
	if entry["type"] != "response" {
		t.Fatalf("type = %v, want response — aborts must pair the request observation", entry["type"])
	}
	if got := entry["status"]; got != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", got)
	}
	if entry["termination"] != TerminationUpstreamError {
		t.Errorf("termination = %v, want %v", entry["termination"], TerminationUpstreamError)
	}
	errObj, _ := entry["error"].(map[string]any)
	if errObj["class"] != "upstream_stream_read_failed" {
		t.Errorf("error.class = %v", errObj["class"])
	}
	responseInfo, _ := entry["response"].(map[string]any)
	chunks, _ := responseInfo["chunks"].([]any)
	if len(chunks) != 1 {
		t.Fatalf("logged chunks = %d, want 1", len(chunks))
	}
}

func TestServeMantleLogsStreamingWriteErrorObservation(t *testing.T) {
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not be called when custom RoundTripper is installed")
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-test"}`)
	proxy.tokenSub = counterSub
	proxy.bedrock.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\"}\n")),
			}, nil
		}),
	}

	req := httptest.NewRequest("POST", "/cbrun/run-test/mantle/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := &failingWriteResponseWriter{header: make(http.Header)}

	proxy.ServeHTTP(w, req)

	if w.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.status)
	}
	if readResolverCalls(t, counter) != 1 {
		t.Fatalf("resolver calls = %d, want 1", readResolverCalls(t, counter))
	}

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	errorEntry := entries[1]
	if got := errorEntry["type"]; got != "response" {
		t.Fatalf("terminal entry type = %v, want response", got)
	}
	if got := errorEntry["termination"]; got != TerminationClientCancel {
		t.Fatalf("terminal entry termination = %v, want %v", got, TerminationClientCancel)
	}
	errorInfo := errorEntry["error"].(map[string]any)
	if got := errorInfo["class"]; got != "client_stream_write_failed" {
		t.Fatalf("error.class = %v, want client_stream_write_failed", got)
	}
	responseInfo := errorEntry["response"].(map[string]any)
	chunks := responseInfo["chunks"].([]any)
	if len(chunks) != 1 {
		t.Fatalf("logged chunks = %d, want 1 attempted chunk", len(chunks))
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

func TestServeMantleRejectsMalformedRunScopedSuffixWithDistinctClass(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
	proxy.tokenSub = counterSub

	req := httptest.NewRequest("POST", "/cbrun/run-123/mantle/v1/responses/extra", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 0 {
		t.Fatalf("resolver should not run for malformed mantle suffix")
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	errorInfo := entries[0]["error"].(map[string]any)
	if got := errorInfo["class"]; got != "invalid_mantle_path" {
		t.Fatalf("error.class = %v, want invalid_mantle_path", got)
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

func TestServeMantleForwardsRunScopedPathWithBareResolverMetadata(t *testing.T) {
	var receivedPath, receivedAuth string
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp_123","object":"response","output":[]}`))
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `REAL-BEARER`)
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

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	requestMeta := entries[0]["_meta"].(map[string]any)
	if got := requestMeta["cloud_build_run_id"]; got != "run-123" {
		t.Fatalf("request cloud_build_run_id = %v, want run-123", got)
	}
	responseMeta := entries[1]["_meta"].(map[string]any)
	if got := responseMeta["request_id"]; got != requestMeta["request_id"] {
		t.Fatalf("response request_id = %v, want %v", got, requestMeta["request_id"])
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

// TestServeMantleResponseObservationStampsCaptureFacts covers the success
// path builder (logMantleResponseObservation): the bastion's ingest door
// meters top-level fields only, so a mantle response record must carry the
// same path/upstream/metering_provider/capture_version stamps every other
// response line carries (see logger.go LogResponse), not just the nested
// response.body/response.chunks payload.
func TestServeMantleResponseObservationStampsCaptureFacts(t *testing.T) {
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp_123","object":"response","output":[],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`))
	}))
	defer mock.Close()

	counterSub, _ := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-test"}`)
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

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	entry := entries[1]
	if got := entry["type"]; got != "response" {
		t.Fatalf("entry type = %v, want response", got)
	}

	const wantUpstream = "bedrock-mantle.us-west-2.api.aws"
	if got := entry["path"]; got != "/openai/v1/responses" {
		t.Fatalf("path = %v, want /openai/v1/responses", got)
	}
	if got := entry["upstream"]; got != wantUpstream {
		t.Fatalf("upstream = %v, want %v", got, wantUpstream)
	}
	// bedrock-mantle.* isn't a host meteringProviderFromUpstream recognizes
	// (the bastion consumer's ProviderFromHost has the same gap); mantle's
	// _meta.provider already carries "openai" and covers metering resolution
	// through the consumer's documented legacy-shim fallback.
	if got, want := entry["metering_provider"], meteringProviderFromUpstream(wantUpstream); got != want {
		t.Fatalf("metering_provider = %v, want %v", got, want)
	}
	if got := entry["capture_version"]; got != float64(CaptureVersion) {
		t.Fatalf("capture_version = %v, want %v", got, CaptureVersion)
	}
}

// TestServeMantleAbortedResponseObservationStampsCaptureFacts covers the
// abort-path builder (logMantleAbortedResponseObservation) with the same
// capture-fact stamps as the success path above.
func TestServeMantleAbortedResponseObservationStampsCaptureFacts(t *testing.T) {
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not be called when custom RoundTripper is installed")
	}))
	defer mock.Close()

	counterSub, _ := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-test"}`)
	proxy.tokenSub = counterSub
	proxy.bedrock.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("dial boom")
		}),
	}

	req := httptest.NewRequest("POST", "/cbrun/run-test/mantle/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502. body=%s", w.Code, w.Body.String())
	}

	entries := readObservationLogEntries(t, proxy.logger.(*Logger))
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2", len(entries))
	}
	entry := entries[1]
	if got := entry["termination"]; got != TerminationUpstreamUnreachable {
		t.Fatalf("termination = %v, want %v", got, TerminationUpstreamUnreachable)
	}

	const wantUpstream = "bedrock-mantle.us-west-2.api.aws"
	if got := entry["path"]; got != "/openai/v1/responses" {
		t.Fatalf("path = %v, want /openai/v1/responses", got)
	}
	if got := entry["upstream"]; got != wantUpstream {
		t.Fatalf("upstream = %v, want %v", got, wantUpstream)
	}
	if got, want := entry["metering_provider"], meteringProviderFromUpstream(wantUpstream); got != want {
		t.Fatalf("metering_provider = %v, want %v", got, want)
	}
	if got := entry["capture_version"]; got != float64(CaptureVersion) {
		t.Fatalf("capture_version = %v, want %v", got, CaptureVersion)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type stagedReadCloser struct {
	first []byte
	rest  []byte
	wait  <-chan struct{}
	step  int
}

type errAfterDataReadCloser struct {
	data []byte
	done bool
}

func (r *errAfterDataReadCloser) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("stream read boom")
}

func (r *errAfterDataReadCloser) Close() error {
	return nil
}

type failingWriteResponseWriter struct {
	header http.Header
	status int
}

func (w *failingWriteResponseWriter) Header() http.Header {
	return w.header
}

func (w *failingWriteResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *failingWriteResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return 0, errors.New("client write boom")
}

func (r *stagedReadCloser) Read(p []byte) (int, error) {
	switch r.step {
	case 0:
		r.step++
		return copy(p, r.first), nil
	case 1:
		<-r.wait
		r.step++
		return copy(p, r.rest), nil
	default:
		return 0, io.EOF
	}
}

func (r *stagedReadCloser) Close() error {
	return nil
}

type probeResponseWriter struct {
	header        http.Header
	status        int
	body          bytes.Buffer
	headerWritten chan struct{}
	bodyWritten   chan struct{}
	headerOnce    sync.Once
	bodyOnce      sync.Once
	mu            sync.Mutex
}

func newProbeResponseWriter() *probeResponseWriter {
	return &probeResponseWriter{
		header:        make(http.Header),
		headerWritten: make(chan struct{}),
		bodyWritten:   make(chan struct{}),
	}
}

func (w *probeResponseWriter) Header() http.Header {
	return w.header
}

func (w *probeResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = status
	}
	w.mu.Unlock()
	w.headerOnce.Do(func() { close(w.headerWritten) })
}

func (w *probeResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.mu.Lock()
	n, err := w.body.Write(p)
	w.mu.Unlock()
	if n > 0 {
		w.bodyOnce.Do(func() { close(w.bodyWritten) })
	}
	return n, err
}

func (w *probeResponseWriter) BodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func waitForSignal(t *testing.T, signal <-chan struct{}, releaseRest chan struct{}, done <-chan struct{}, msg string) {
	t.Helper()

	select {
	case <-signal:
		return
	case <-time.After(250 * time.Millisecond):
		close(releaseRest)
		<-done
		t.Fatal(msg)
	}
}

// loki_exporter_test.go
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewLokiExporter_RequiresURL(t *testing.T) {
	cfg := LokiExporterConfig{
		URL: "", // empty URL
	}

	_, err := NewLokiExporter(cfg)
	if err == nil {
		t.Error("expected error when URL is empty")
	}
	if err != nil && !strings.Contains(err.Error(), "URL") {
		t.Errorf("error should mention URL, got: %v", err)
	}
}

func TestNewLokiExporter_DefaultValues(t *testing.T) {
	cfg := LokiExporterConfig{
		URL: "https://localhost:3100/loki/api/v1/push",
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer exporter.Close()

	// Verify defaults are applied for zero-value fields
	// Note: UseGzip defaults to false (Go zero value) - the application-level
	// default of true comes from config.go's DefaultConfig(), not here.
	if exporter.config.BatchSize != 1000 {
		t.Errorf("expected default BatchSize 1000, got %d", exporter.config.BatchSize)
	}
	if exporter.config.BatchWait != 5*time.Second {
		t.Errorf("expected default BatchWait 5s, got %v", exporter.config.BatchWait)
	}
	if exporter.config.RetryMax != 5 {
		t.Errorf("expected default RetryMax 5, got %d", exporter.config.RetryMax)
	}
	if exporter.config.RetryWait != 100*time.Millisecond {
		t.Errorf("expected default RetryWait 100ms, got %v", exporter.config.RetryWait)
	}
	if exporter.config.BufferSize != 10000 {
		t.Errorf("expected default BufferSize 10000, got %d", exporter.config.BufferSize)
	}
}

func TestNewLokiExporter_CustomValues(t *testing.T) {
	cfg := LokiExporterConfig{
		URL:         "http://localhost:3100/loki/api/v1/push",
		BatchSize:   500,
		BatchWait:   2 * time.Second,
		RetryMax:    3,
		RetryWait:   50 * time.Millisecond,
		UseGzip:     false,
		BufferSize:  5000,
		Environment: "production",
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer exporter.Close()

	if exporter.config.BatchSize != 500 {
		t.Errorf("expected BatchSize 500, got %d", exporter.config.BatchSize)
	}
	if exporter.config.BatchWait != 2*time.Second {
		t.Errorf("expected BatchWait 2s, got %v", exporter.config.BatchWait)
	}
	if exporter.config.RetryMax != 3 {
		t.Errorf("expected RetryMax 3, got %d", exporter.config.RetryMax)
	}
	if exporter.config.Environment != "production" {
		t.Errorf("expected Environment 'production', got %s", exporter.config.Environment)
	}
}

func TestDoSend_AuthTokenHeader(t *testing.T) {
	var receivedAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		AuthToken:         "my-secret-token",
		UseGzip:           false, // disable for simpler test
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer exporter.Close()

	// Create a minimal Loki push request
	payload := LokiPushRequest{
		Streams: []LokiStream{{
			Stream: map[string]string{"app": "test"},
			Values: [][]string{{"1234567890000000000", "test message"}},
		}},
	}

	err = exporter.doSend(payload)
	if err != nil {
		t.Fatalf("doSend failed: %v", err)
	}

	expectedAuth := "Bearer my-secret-token"
	if receivedAuth != expectedAuth {
		t.Errorf("expected Authorization header %q, got %q", expectedAuth, receivedAuth)
	}
}

func TestDoSend_NoAuthToken(t *testing.T) {
	var receivedAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		AuthToken:         "", // no auth token
		UseGzip:           false,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer exporter.Close()

	payload := LokiPushRequest{
		Streams: []LokiStream{{
			Stream: map[string]string{"app": "test"},
			Values: [][]string{{"1234567890000000000", "test message"}},
		}},
	}

	err = exporter.doSend(payload)
	if err != nil {
		t.Fatalf("doSend failed: %v", err)
	}

	if receivedAuth != "" {
		t.Errorf("expected no Authorization header, got %q", receivedAuth)
	}
}

func TestDoSend_GzipCompression(t *testing.T) {
	var receivedContentEncoding string
	var receivedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentEncoding = r.Header.Get("Content-Encoding")
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		UseGzip:           true,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer exporter.Close()

	payload := LokiPushRequest{
		Streams: []LokiStream{{
			Stream: map[string]string{"app": "test"},
			Values: [][]string{{"1234567890000000000", "test message"}},
		}},
	}

	err = exporter.doSend(payload)
	if err != nil {
		t.Fatalf("doSend failed: %v", err)
	}

	if receivedContentEncoding != "gzip" {
		t.Errorf("expected Content-Encoding 'gzip', got %q", receivedContentEncoding)
	}

	// Verify the body is actually gzip compressed
	gzipReader, err := gzip.NewReader(bytes.NewReader(receivedBody))
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	defer gzipReader.Close()

	decompressed, err := io.ReadAll(gzipReader)
	if err != nil {
		t.Fatalf("failed to decompress: %v", err)
	}

	var decoded LokiPushRequest
	if err := json.Unmarshal(decompressed, &decoded); err != nil {
		t.Fatalf("failed to unmarshal decompressed data: %v", err)
	}

	if len(decoded.Streams) != 1 {
		t.Errorf("expected 1 stream, got %d", len(decoded.Streams))
	}
}

func TestPush_AddsToChannel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         100,              // high threshold so no auto-flush
		BatchWait:         10 * time.Second, // long wait so no auto-flush
		BufferSize:        100,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer exporter.Close()

	entry := map[string]interface{}{
		"type": "request",
		"_meta": map[string]interface{}{
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"machine": "test@host",
			"session": "test-session",
		},
	}

	// Push should succeed (non-blocking)
	exporter.Push(entry, "anthropic")

	// Wait a moment for the channel send
	time.Sleep(10 * time.Millisecond)

	stats := exporter.Stats()
	// Entry is queued, not yet sent
	if stats.EntriesDropped != 0 {
		t.Errorf("expected 0 dropped entries, got %d", stats.EntriesDropped)
	}
}

func TestPush_DropsWhenChannelFull(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slow server - simulate backpressure
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         10000,            // very high so no auto-flush from size
		BatchWait:         10 * time.Second, // very long so no auto-flush from time
		BufferSize:        5,                // tiny buffer to force drops
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer exporter.Close()

	entry := map[string]interface{}{
		"type": "request",
		"_meta": map[string]interface{}{
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"machine": "test@host",
		},
	}

	// Push more entries than the buffer can hold
	for i := 0; i < 20; i++ {
		exporter.Push(entry, "anthropic")
	}

	// Wait for pushes to complete
	time.Sleep(50 * time.Millisecond)

	stats := exporter.Stats()
	if stats.EntriesDropped == 0 {
		t.Error("expected some entries to be dropped when channel is full")
	}
}

func TestPush_ExtractsTimestamp(t *testing.T) {
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,         // flush immediately
		BatchWait:         time.Hour, // don't wait
		UseGzip:           false,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	testTime := "2024-01-24T10:30:00.123456789Z"
	entry := map[string]interface{}{
		"type": "request",
		"_meta": map[string]interface{}{
			"ts":      testTime,
			"machine": "test@host",
		},
	}

	exporter.Push(entry, "anthropic")

	// Wait for batch to flush
	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	if len(receivedPayload.Streams) == 0 {
		t.Fatal("expected at least one stream")
	}
	if len(receivedPayload.Streams[0].Values) == 0 {
		t.Fatal("expected at least one value")
	}

	// Timestamp should be in nanoseconds
	ts := receivedPayload.Streams[0].Values[0][0]
	// 2024-01-24T10:30:00.123456789Z in nanoseconds
	expectedTs := "1706092200123456789"
	if ts != expectedTs {
		t.Errorf("expected timestamp %s, got %s", expectedTs, ts)
	}
}

func TestPush_ExtractsLogType(t *testing.T) {
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := map[string]interface{}{
		"type": "session_start",
		"_meta": map[string]interface{}{
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"machine": "test@host",
		},
	}

	exporter.Push(entry, "anthropic")

	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	if len(receivedPayload.Streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	logType := receivedPayload.Streams[0].Stream["log_type"]
	if logType != "session_start" {
		t.Errorf("expected log_type 'session_start', got %q", logType)
	}
}

func TestSendBatch_GroupsByLabels(t *testing.T) {
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         4, // flush after 4 entries
		BatchWait:         time.Hour,
		UseGzip:           false,
		Environment:       "test",
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Push entries with different providers (different labels)
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	entry1 := map[string]interface{}{"type": "request", "_meta": map[string]interface{}{"ts": ts, "machine": "test@host"}}
	entry2 := map[string]interface{}{"type": "response", "_meta": map[string]interface{}{"ts": ts, "machine": "test@host"}}
	entry3 := map[string]interface{}{"type": "request", "_meta": map[string]interface{}{"ts": ts, "machine": "test@host"}}
	entry4 := map[string]interface{}{"type": "response", "_meta": map[string]interface{}{"ts": ts, "machine": "test@host"}}

	exporter.Push(entry1, "anthropic")
	exporter.Push(entry2, "anthropic")
	exporter.Push(entry3, "openai")
	exporter.Push(entry4, "openai")

	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	// Should have 4 streams: anthropic/request, anthropic/response, openai/request, openai/response
	if len(receivedPayload.Streams) != 4 {
		t.Errorf("expected 4 streams (grouped by labels), got %d", len(receivedPayload.Streams))
	}

	// Verify each stream has the right labels
	providers := make(map[string]bool)
	logTypes := make(map[string]bool)
	for _, stream := range receivedPayload.Streams {
		providers[stream.Stream["provider"]] = true
		logTypes[stream.Stream["log_type"]] = true
		if stream.Stream["app"] != "llm-proxy" {
			t.Errorf("expected app 'llm-proxy', got %q", stream.Stream["app"])
		}
		if stream.Stream["environment"] != "test" {
			t.Errorf("expected environment 'test', got %q", stream.Stream["environment"])
		}
	}

	if !providers["anthropic"] || !providers["openai"] {
		t.Errorf("expected both anthropic and openai providers, got %v", providers)
	}
	if !logTypes["request"] || !logTypes["response"] {
		t.Errorf("expected both request and response log types, got %v", logTypes)
	}
}

func TestSendBatch_RetriesOnError(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requestCount, 1)
		if count < 3 {
			// First two requests fail
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// Third request succeeds
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		RetryMax:          5,
		RetryWait:         10 * time.Millisecond, // fast retries for test
		UseGzip:           false,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := map[string]interface{}{
		"type":  "request",
		"_meta": map[string]interface{}{"ts": time.Now().UTC().Format(time.RFC3339Nano), "machine": "test@host"},
	}

	exporter.Push(entry, "anthropic")

	time.Sleep(200 * time.Millisecond) // wait for retries
	exporter.Close()

	finalCount := atomic.LoadInt32(&requestCount)
	if finalCount < 3 {
		t.Errorf("expected at least 3 requests (2 retries + success), got %d", finalCount)
	}

	stats := exporter.Stats()
	if stats.EntriesSent != 1 {
		t.Errorf("expected 1 entry sent, got %d", stats.EntriesSent)
	}
}

func TestSendBatch_ExponentialBackoff(t *testing.T) {
	var requestTimes []time.Time
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestTimes = append(requestTimes, time.Now())
		count := len(requestTimes)
		mu.Unlock()

		if count < 4 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		RetryMax:          5,
		RetryWait:         50 * time.Millisecond, // base delay
		UseGzip:           false,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := map[string]interface{}{
		"type":  "request",
		"_meta": map[string]interface{}{"ts": time.Now().UTC().Format(time.RFC3339Nano), "machine": "test@host"},
	}

	exporter.Push(entry, "anthropic")

	time.Sleep(1 * time.Second) // wait for retries
	exporter.Close()

	mu.Lock()
	times := make([]time.Time, len(requestTimes))
	copy(times, requestTimes)
	mu.Unlock()

	if len(times) < 4 {
		t.Fatalf("expected at least 4 requests, got %d", len(times))
	}

	// Verify exponential backoff: each delay should be roughly 2x the previous
	// With jitter (25%), we expect: 50ms, 100ms, 200ms (roughly)
	delay1 := times[1].Sub(times[0])
	delay2 := times[2].Sub(times[1])
	delay3 := times[3].Sub(times[2])

	// Allow for jitter by checking delay2 > delay1 * 1.5 (roughly)
	if delay2 < delay1 {
		t.Errorf("expected exponential backoff: delay2 (%v) should be > delay1 (%v)", delay2, delay1)
	}
	if delay3 < delay2 {
		t.Errorf("expected exponential backoff: delay3 (%v) should be > delay2 (%v)", delay3, delay2)
	}
}

func TestClose_FlushesRemaining(t *testing.T) {
	var receivedEntries int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload LokiPushRequest
		json.Unmarshal(body, &payload)

		for _, stream := range payload.Streams {
			atomic.AddInt32(&receivedEntries, int32(len(stream.Values)))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1000,             // high threshold - won't trigger auto-flush
		BatchWait:         10 * time.Second, // long wait - won't trigger auto-flush
		UseGzip:           false,
		BufferSize:        100,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Push several entries
	for i := 0; i < 10; i++ {
		entry := map[string]interface{}{
			"type":  "request",
			"seq":   i,
			"_meta": map[string]interface{}{"ts": time.Now().UTC().Format(time.RFC3339Nano), "machine": "test@host"},
		}
		exporter.Push(entry, "anthropic")
	}

	// Close should flush remaining entries
	err = exporter.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	received := atomic.LoadInt32(&receivedEntries)
	if received != 10 {
		t.Errorf("expected 10 entries flushed on close, got %d", received)
	}
}

func TestClose_TimesOut(t *testing.T) {
	// Server that never responds
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
		ShutdownTimeout:   100 * time.Millisecond, // very short timeout for test
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := map[string]interface{}{
		"type":  "request",
		"_meta": map[string]interface{}{"ts": time.Now().UTC().Format(time.RFC3339Nano), "machine": "test@host"},
	}
	exporter.Push(entry, "anthropic")

	// Give entry time to be processed
	time.Sleep(50 * time.Millisecond)

	err = exporter.Close()
	if err == nil {
		t.Error("expected timeout error on close")
	}
	if err != nil && !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout error, got: %v", err)
	}
}

func TestStats_Accurate(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requestCount, 1)
		// Fail every other request
		if count%2 == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1, // flush each entry individually
		BatchWait:         time.Hour,
		RetryMax:          1, // only 1 retry, so some will fail
		RetryWait:         10 * time.Millisecond,
		UseGzip:           false,
		BufferSize:        5, // small buffer to test drops
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Push several entries
	for i := 0; i < 5; i++ {
		entry := map[string]interface{}{
			"type":  "request",
			"seq":   i,
			"_meta": map[string]interface{}{"ts": time.Now().UTC().Format(time.RFC3339Nano), "machine": "test@host"},
		}
		exporter.Push(entry, "anthropic")
	}

	time.Sleep(500 * time.Millisecond)
	exporter.Close()

	stats := exporter.Stats()

	// Stats should be non-negative
	if stats.EntriesSent < 0 {
		t.Error("EntriesSent should be non-negative")
	}
	if stats.EntriesFailed < 0 {
		t.Error("EntriesFailed should be non-negative")
	}
	if stats.EntriesDropped < 0 {
		t.Error("EntriesDropped should be non-negative")
	}
	if stats.BatchesSent < 0 {
		t.Error("BatchesSent should be non-negative")
	}

	// Total processed should equal total pushed (accounting for drops)
	total := stats.EntriesSent + stats.EntriesFailed + stats.EntriesDropped
	if total == 0 {
		t.Error("expected some entries to be processed")
	}
}

func TestBatchFlushByTime(t *testing.T) {
	var receivedPayload LokiPushRequest
	var receiveTime atomic.Value // stores time.Time
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receiveTime.Store(time.Now())
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		json.Unmarshal(body, &receivedPayload)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1000,                   // high threshold - won't trigger by size
		BatchWait:         100 * time.Millisecond, // short wait for test
		UseGzip:           false,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer exporter.Close()

	startTime := time.Now()
	entry := map[string]interface{}{
		"type":  "request",
		"_meta": map[string]interface{}{"ts": time.Now().UTC().Format(time.RFC3339Nano), "machine": "test@host"},
	}
	exporter.Push(entry, "anthropic")

	// Wait for batch to flush by time
	time.Sleep(300 * time.Millisecond)

	receiveTimeVal := receiveTime.Load()
	if receiveTimeVal == nil {
		t.Fatal("expected batch to be flushed by time")
	}

	elapsed := receiveTimeVal.(time.Time).Sub(startTime)
	if elapsed < 100*time.Millisecond {
		t.Errorf("batch flushed too early: %v", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("batch flushed too late: %v", elapsed)
	}
}

func TestBatchFlushBySize(t *testing.T) {
	var receivedAt atomic.Value // stores time.Time
	var receivedCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if receivedAt.Load() == nil {
			receivedAt.Store(time.Now())
		}
		body, _ := io.ReadAll(r.Body)
		var payload LokiPushRequest
		json.Unmarshal(body, &payload)
		for _, stream := range payload.Streams {
			atomic.AddInt32(&receivedCount, int32(len(stream.Values)))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         5,                // low threshold - trigger after 5 entries
		BatchWait:         10 * time.Second, // long wait - shouldn't trigger by time
		UseGzip:           false,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer exporter.Close()

	startTime := time.Now()

	// Push exactly batch size entries
	for i := 0; i < 5; i++ {
		entry := map[string]interface{}{
			"type":  "request",
			"seq":   i,
			"_meta": map[string]interface{}{"ts": time.Now().UTC().Format(time.RFC3339Nano), "machine": "test@host"},
		}
		exporter.Push(entry, "anthropic")
	}

	// Give a moment for batch to flush
	time.Sleep(200 * time.Millisecond)

	receivedAtVal := receivedAt.Load()
	if receivedAtVal == nil {
		t.Fatal("expected batch to be flushed by size")
	}

	elapsed := receivedAtVal.(time.Time).Sub(startTime)
	// Should flush quickly (by size), not wait for the 10 second timeout
	if elapsed > 1*time.Second {
		t.Errorf("batch should have flushed by size, not time: elapsed %v", elapsed)
	}

	count := atomic.LoadInt32(&receivedCount)
	if count != 5 {
		t.Errorf("expected 5 entries, got %d", count)
	}
}

func TestLokiPushRequestFormat(t *testing.T) {
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify content type
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type 'application/json', got %q", r.Header.Get("Content-Type"))
		}

		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
		Environment:       "production",
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := map[string]interface{}{
		"type":     "request",
		"method":   "POST",
		"path":     "/v1/messages",
		"provider": "anthropic",
		"_meta": map[string]interface{}{
			"ts":      "2024-01-24T10:30:00.000000000Z",
			"machine": "user@hostname",
			"session": "test-session-123",
		},
	}

	exporter.Push(entry, "anthropic")
	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	// Verify stream structure
	if len(receivedPayload.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(receivedPayload.Streams))
	}

	stream := receivedPayload.Streams[0]

	// Verify labels (FR6)
	expectedLabels := map[string]string{
		"app":         "llm-proxy",
		"provider":    "anthropic",
		"environment": "production",
		"machine":     "user@hostname",
		"log_type":    "request",
	}

	for key, expected := range expectedLabels {
		if stream.Stream[key] != expected {
			t.Errorf("expected label %s=%q, got %q", key, expected, stream.Stream[key])
		}
	}

	// Verify values format
	if len(stream.Values) != 1 {
		t.Fatalf("expected 1 value, got %d", len(stream.Values))
	}

	value := stream.Values[0]
	if len(value) != 2 {
		t.Fatalf("expected value to have 2 elements [timestamp, line], got %d", len(value))
	}

	// Timestamp should be nanoseconds
	// 2024-01-24T10:30:00.000000000Z = 1706092200000000000 nanoseconds
	if value[0] != "1706092200000000000" {
		t.Errorf("expected timestamp in nanoseconds, got %s", value[0])
	}

	// Log line should be JSON
	var logLine map[string]interface{}
	if err := json.Unmarshal([]byte(value[1]), &logLine); err != nil {
		t.Errorf("expected JSON log line, got error: %v", err)
	}
}

func TestDoSend_ReturnsErrorOn4xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid request"))
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		UseGzip:           false,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer exporter.Close()

	payload := LokiPushRequest{
		Streams: []LokiStream{{
			Stream: map[string]string{"app": "test"},
			Values: [][]string{{"1234567890000000000", "test message"}},
		}},
	}

	err = exporter.doSend(payload)
	if err == nil {
		t.Error("expected error on 4xx response")
	}
}

// Tests for extended labels (PRI-298)

func TestExtractExtendedLabels_Request(t *testing.T) {
	// Test request with model, stream, and tools
	entry := map[string]interface{}{
		"type": "request",
		"body": `{"model":"claude-sonnet-4-20250514","stream":true,"tools":[{"name":"bash"}],"messages":[]}`,
	}

	model, statusBucket, stream, hasTools, stopReason, ratelimitStatus := extractExtendedLabels(entry, "request")

	if model != "claude-sonnet-4-20250514" {
		t.Errorf("expected model 'claude-sonnet-4-20250514', got %q", model)
	}
	if stream != "true" {
		t.Errorf("expected stream 'true', got %q", stream)
	}
	if hasTools != "true" {
		t.Errorf("expected hasTools 'true', got %q", hasTools)
	}
	// These should be empty for requests
	if statusBucket != "" {
		t.Errorf("expected empty statusBucket for request, got %q", statusBucket)
	}
	if stopReason != "" {
		t.Errorf("expected empty stopReason for request, got %q", stopReason)
	}
	if ratelimitStatus != "" {
		t.Errorf("expected empty ratelimitStatus for request, got %q", ratelimitStatus)
	}
}

func TestExtractExtendedLabels_RequestNoTools(t *testing.T) {
	entry := map[string]interface{}{
		"type": "request",
		"body": `{"model":"gpt-4","stream":false,"messages":[]}`,
	}

	model, _, stream, hasTools, _, _ := extractExtendedLabels(entry, "request")

	if model != "gpt-4" {
		t.Errorf("expected model 'gpt-4', got %q", model)
	}
	if stream != "false" {
		t.Errorf("expected stream 'false', got %q", stream)
	}
	if hasTools != "false" {
		t.Errorf("expected hasTools 'false' when no tools, got %q", hasTools)
	}
}

func TestExtractExtendedLabels_Response2xx(t *testing.T) {
	entry := map[string]interface{}{
		"type":   "response",
		"status": 200,
		"body":   `{"stop_reason":"end_turn"}`,
		"headers": map[string]interface{}{
			"Anthropic-Ratelimit-Unified-Status": "allowed",
		},
	}

	_, statusBucket, _, _, stopReason, ratelimitStatus := extractExtendedLabels(entry, "response")

	if statusBucket != "2xx" {
		t.Errorf("expected statusBucket '2xx', got %q", statusBucket)
	}
	if stopReason != "end_turn" {
		t.Errorf("expected stopReason 'end_turn', got %q", stopReason)
	}
	if ratelimitStatus != "allowed" {
		t.Errorf("expected ratelimitStatus 'allowed', got %q", ratelimitStatus)
	}
}

func TestExtractExtendedLabels_Response4xx(t *testing.T) {
	entry := map[string]interface{}{
		"type":   "response",
		"status": 429,
		"headers": map[string]interface{}{
			"Anthropic-Ratelimit-Unified-Status": "limited",
		},
	}

	_, statusBucket, _, _, _, ratelimitStatus := extractExtendedLabels(entry, "response")

	if statusBucket != "4xx" {
		t.Errorf("expected statusBucket '4xx', got %q", statusBucket)
	}
	if ratelimitStatus != "limited" {
		t.Errorf("expected ratelimitStatus 'limited', got %q", ratelimitStatus)
	}
}

func TestExtractExtendedLabels_Response5xx(t *testing.T) {
	entry := map[string]interface{}{
		"type":   "response",
		"status": 500,
	}

	_, statusBucket, _, _, _, _ := extractExtendedLabels(entry, "response")

	if statusBucket != "5xx" {
		t.Errorf("expected statusBucket '5xx', got %q", statusBucket)
	}
}

func TestExtractExtendedLabels_ResponseFloat64Status(t *testing.T) {
	// JSON unmarshaling produces float64 for numbers
	entry := map[string]interface{}{
		"type":   "response",
		"status": float64(201),
	}

	_, statusBucket, _, _, _, _ := extractExtendedLabels(entry, "response")

	if statusBucket != "2xx" {
		t.Errorf("expected statusBucket '2xx' for float64 status, got %q", statusBucket)
	}
}

func TestExtractExtendedLabels_SessionStart(t *testing.T) {
	// session_start logs should have no extended labels
	entry := map[string]interface{}{
		"type": "session_start",
	}

	model, statusBucket, stream, hasTools, stopReason, ratelimitStatus := extractExtendedLabels(entry, "session_start")

	if model != "" || statusBucket != "" || stream != "" || hasTools != "" || stopReason != "" || ratelimitStatus != "" {
		t.Error("expected all extended labels to be empty for session_start")
	}
}

func TestExtractStopReason_NonStreaming(t *testing.T) {
	entry := map[string]interface{}{
		"body": `{"stop_reason":"max_tokens","content":[]}`,
	}

	stopReason := extractStopReason(entry)

	if stopReason != "max_tokens" {
		t.Errorf("expected stopReason 'max_tokens', got %q", stopReason)
	}
}

func TestExtractStopReason_Streaming(t *testing.T) {
	// Simulate streaming chunks with message_delta containing stop_reason
	entry := map[string]interface{}{
		"chunks": []interface{}{
			map[string]interface{}{
				"raw": `{"type":"content_block_delta","delta":{"text":"hello"}}`,
			},
			map[string]interface{}{
				"raw": `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
			},
		},
	}

	stopReason := extractStopReason(entry)

	if stopReason != "tool_use" {
		t.Errorf("expected stopReason 'tool_use', got %q", stopReason)
	}
}

func TestPush_ExtractsExtendedLabels(t *testing.T) {
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
		Environment:       "test",
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := map[string]interface{}{
		"type": "request",
		"body": `{"model":"claude-opus-4-5-20251101","stream":true,"tools":[{"name":"bash"}]}`,
		"_meta": map[string]interface{}{
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"machine": "test@host",
		},
	}

	exporter.Push(entry, "anthropic")
	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	if len(receivedPayload.Streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	stream := receivedPayload.Streams[0]

	// Check extended labels are present
	if stream.Stream["model"] != "claude-opus-4-5-20251101" {
		t.Errorf("expected model label 'claude-opus-4-5-20251101', got %q", stream.Stream["model"])
	}
	if stream.Stream["stream"] != "true" {
		t.Errorf("expected stream label 'true', got %q", stream.Stream["stream"])
	}
	if stream.Stream["has_tools"] != "true" {
		t.Errorf("expected has_tools label 'true', got %q", stream.Stream["has_tools"])
	}
}

func TestPush_ResponseExtendedLabels(t *testing.T) {
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
		Environment:       "test",
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := map[string]interface{}{
		"type":   "response",
		"status": 200,
		"body":   `{"stop_reason":"end_turn"}`,
		"headers": map[string]interface{}{
			"Anthropic-Ratelimit-Unified-Status": "allowed",
		},
		"_meta": map[string]interface{}{
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"machine": "test@host",
		},
	}

	exporter.Push(entry, "anthropic")
	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	if len(receivedPayload.Streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	stream := receivedPayload.Streams[0]

	if stream.Stream["status_bucket"] != "2xx" {
		t.Errorf("expected status_bucket '2xx', got %q", stream.Stream["status_bucket"])
	}
	if stream.Stream["stop_reason"] != "end_turn" {
		t.Errorf("expected stop_reason 'end_turn', got %q", stream.Stream["stop_reason"])
	}
	if stream.Stream["ratelimit_status"] != "allowed" {
		t.Errorf("expected ratelimit_status 'allowed', got %q", stream.Stream["ratelimit_status"])
	}
}

func TestDoSend_ReturnsErrorOn5xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		UseGzip:           false,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer exporter.Close()

	payload := LokiPushRequest{
		Streams: []LokiStream{{
			Stream: map[string]string{"app": "test"},
			Values: [][]string{{"1234567890000000000", "test message"}},
		}},
	}

	err = exporter.doSend(payload)
	if err == nil {
		t.Error("expected error on 5xx response")
	}
}

func TestNonBlockingPush(t *testing.T) {
	// Server that blocks forever
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {} // block forever
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Millisecond,
		BufferSize:        1, // tiny buffer
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := map[string]interface{}{
		"type":  "request",
		"_meta": map[string]interface{}{"ts": time.Now().UTC().Format(time.RFC3339Nano), "machine": "test@host"},
	}

	// Push should be non-blocking even when channel is full
	done := make(chan bool)
	go func() {
		for i := 0; i < 100; i++ {
			exporter.Push(entry, "anthropic")
		}
		done <- true
	}()

	select {
	case <-done:
		// Push completed without blocking - good!
	case <-time.After(1 * time.Second):
		t.Error("Push blocked when it should be non-blocking")
	}

	// Close without waiting (force close)
	exporter.forceClose()
}

// Tests for Agent Observability Event Infrastructure (PRI-343)

func TestLogTypeConstants(t *testing.T) {
	// Verify log type constants are defined with expected values
	if LogTypeTurnStart != "turn_start" {
		t.Errorf("expected LogTypeTurnStart='turn_start', got %q", LogTypeTurnStart)
	}
	if LogTypeTurnEnd != "turn_end" {
		t.Errorf("expected LogTypeTurnEnd='turn_end', got %q", LogTypeTurnEnd)
	}
	if LogTypeToolCall != "tool_call" {
		t.Errorf("expected LogTypeToolCall='tool_call', got %q", LogTypeToolCall)
	}
	if LogTypeToolResult != "tool_result" {
		t.Errorf("expected LogTypeToolResult='tool_result', got %q", LogTypeToolResult)
	}
}

func TestClassifyErrorType(t *testing.T) {
	tests := []struct {
		name         string
		statusCode   int
		responseBody string
		want         string
	}{
		{"rate_limit_429", 429, "", "rate_limit"},
		{"context_length_400", 400, `{"error": {"message": "context length exceeded"}}`, "context_length"},
		{"context_length_400_alt", 400, `{"error": {"message": "prompt is too long"}}`, "context_length"},
		{"invalid_request_400", 400, `{"error": {"message": "invalid model"}}`, "invalid_request"},
		{"server_error_500", 500, "", "server_error"},
		{"server_error_502", 502, "", "server_error"},
		{"server_error_503", 503, "", "server_error"},
		{"success_200", 200, "", ""},
		{"success_201", 201, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyErrorType(tt.statusCode, tt.responseBody)
			if got != tt.want {
				t.Errorf("classifyErrorType(%d, %q) = %q, want %q", tt.statusCode, tt.responseBody, got, tt.want)
			}
		})
	}
}

func TestPatternDataStruct(t *testing.T) {
	// Test that PatternData struct has expected fields and marshals correctly
	pd := PatternData{
		TurnDepth:        5,
		ToolStreak:       3,
		RetryCount:       2,
		SessionToolCount: 10,
	}

	data, err := json.Marshal(pd)
	if err != nil {
		t.Fatalf("failed to marshal PatternData: %v", err)
	}

	var unmarshaled map[string]interface{}
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("failed to unmarshal PatternData: %v", err)
	}

	// Verify JSON field names
	if v, ok := unmarshaled["turn_depth"].(float64); !ok || int(v) != 5 {
		t.Errorf("expected turn_depth=5, got %v", unmarshaled["turn_depth"])
	}
	if v, ok := unmarshaled["tool_streak"].(float64); !ok || int(v) != 3 {
		t.Errorf("expected tool_streak=3, got %v", unmarshaled["tool_streak"])
	}
	if v, ok := unmarshaled["retry_count"].(float64); !ok || int(v) != 2 {
		t.Errorf("expected retry_count=2, got %v", unmarshaled["retry_count"])
	}
	if v, ok := unmarshaled["session_tool_count"].(float64); !ok || int(v) != 10 {
		t.Errorf("expected session_tool_count=10, got %v", unmarshaled["session_tool_count"])
	}
}

func TestTokenDataStruct(t *testing.T) {
	// Test that TokenData struct has expected fields and marshals correctly
	td := TokenData{
		InputTokens:              100,
		OutputTokens:             50,
		CacheReadInputTokens:     80,
		CacheCreationInputTokens: 20,
	}

	data, err := json.Marshal(td)
	if err != nil {
		t.Fatalf("failed to marshal TokenData: %v", err)
	}

	var unmarshaled map[string]interface{}
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("failed to unmarshal TokenData: %v", err)
	}

	// Verify JSON field names match spec
	if v, ok := unmarshaled["input_tokens"].(float64); !ok || int(v) != 100 {
		t.Errorf("expected input_tokens=100, got %v", unmarshaled["input_tokens"])
	}
	if v, ok := unmarshaled["output_tokens"].(float64); !ok || int(v) != 50 {
		t.Errorf("expected output_tokens=50, got %v", unmarshaled["output_tokens"])
	}
	if v, ok := unmarshaled["cache_read_input_tokens"].(float64); !ok || int(v) != 80 {
		t.Errorf("expected cache_read_input_tokens=80, got %v", unmarshaled["cache_read_input_tokens"])
	}
	if v, ok := unmarshaled["cache_creation_input_tokens"].(float64); !ok || int(v) != 20 {
		t.Errorf("expected cache_creation_input_tokens=20, got %v", unmarshaled["cache_creation_input_tokens"])
	}
}

func TestEmitTurnStart(t *testing.T) {
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
		Environment:       "test",
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	exporter.EmitTurnStart("test-session", "anthropic", "test@host", 5, true)

	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	if len(receivedPayload.Streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	stream := receivedPayload.Streams[0]

	// Verify log_type label
	if stream.Stream["log_type"] != "turn_start" {
		t.Errorf("expected log_type 'turn_start', got %q", stream.Stream["log_type"])
	}

	// Parse JSON body
	if len(stream.Values) == 0 || len(stream.Values[0]) < 2 {
		t.Fatal("expected values with log line")
	}

	var logBody map[string]interface{}
	if err := json.Unmarshal([]byte(stream.Values[0][1]), &logBody); err != nil {
		t.Fatalf("failed to parse log body: %v", err)
	}

	// Verify JSON body fields
	if v, ok := logBody["turn_depth"].(float64); !ok || int(v) != 5 {
		t.Errorf("expected turn_depth=5, got %v", logBody["turn_depth"])
	}
	if v, ok := logBody["error_recovered"].(bool); !ok || !v {
		t.Errorf("expected error_recovered=true, got %v", logBody["error_recovered"])
	}
}

func TestEmitTurnEnd(t *testing.T) {
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
		Environment:       "test",
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	patterns := PatternData{
		TurnDepth:        3,
		ToolStreak:       2,
		RetryCount:       1,
		SessionToolCount: 7,
	}
	tokens := TokenData{
		InputTokens:              1000,
		OutputTokens:             500,
		CacheReadInputTokens:     800,
		CacheCreationInputTokens: 200,
	}

	exporter.EmitTurnEnd("test-session", "anthropic", "test@host", "end_turn", false, "", patterns, tokens)

	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	if len(receivedPayload.Streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	stream := receivedPayload.Streams[0]

	// Verify labels
	if stream.Stream["log_type"] != "turn_end" {
		t.Errorf("expected log_type 'turn_end', got %q", stream.Stream["log_type"])
	}
	if stream.Stream["stop_reason"] != "end_turn" {
		t.Errorf("expected stop_reason 'end_turn', got %q", stream.Stream["stop_reason"])
	}
	if stream.Stream["is_retry"] != "false" {
		t.Errorf("expected is_retry 'false', got %q", stream.Stream["is_retry"])
	}

	// Parse JSON body
	var logBody map[string]interface{}
	if err := json.Unmarshal([]byte(stream.Values[0][1]), &logBody); err != nil {
		t.Fatalf("failed to parse log body: %v", err)
	}

	// Verify pattern data in body
	if v, ok := logBody["turn_depth"].(float64); !ok || int(v) != 3 {
		t.Errorf("expected turn_depth=3, got %v", logBody["turn_depth"])
	}
	if v, ok := logBody["tool_streak"].(float64); !ok || int(v) != 2 {
		t.Errorf("expected tool_streak=2, got %v", logBody["tool_streak"])
	}
	if v, ok := logBody["retry_count"].(float64); !ok || int(v) != 1 {
		t.Errorf("expected retry_count=1, got %v", logBody["retry_count"])
	}
	if v, ok := logBody["session_tool_count"].(float64); !ok || int(v) != 7 {
		t.Errorf("expected session_tool_count=7, got %v", logBody["session_tool_count"])
	}

	// Verify token data in body
	if v, ok := logBody["input_tokens"].(float64); !ok || int(v) != 1000 {
		t.Errorf("expected input_tokens=1000, got %v", logBody["input_tokens"])
	}
	if v, ok := logBody["output_tokens"].(float64); !ok || int(v) != 500 {
		t.Errorf("expected output_tokens=500, got %v", logBody["output_tokens"])
	}
	if v, ok := logBody["cache_read_input_tokens"].(float64); !ok || int(v) != 800 {
		t.Errorf("expected cache_read_input_tokens=800, got %v", logBody["cache_read_input_tokens"])
	}
	if v, ok := logBody["cache_creation_input_tokens"].(float64); !ok || int(v) != 200 {
		t.Errorf("expected cache_creation_input_tokens=200, got %v", logBody["cache_creation_input_tokens"])
	}
}

func TestEmitTurnEnd_WithRetryAndErrorType(t *testing.T) {
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
		Environment:       "test",
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	patterns := PatternData{TurnDepth: 2}
	tokens := TokenData{}

	exporter.EmitTurnEnd("test-session", "anthropic", "test@host", "error", true, "rate_limit", patterns, tokens)

	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	if len(receivedPayload.Streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	stream := receivedPayload.Streams[0]

	// Verify retry and error labels
	if stream.Stream["is_retry"] != "true" {
		t.Errorf("expected is_retry 'true', got %q", stream.Stream["is_retry"])
	}
	if stream.Stream["error_type"] != "rate_limit" {
		t.Errorf("expected error_type 'rate_limit', got %q", stream.Stream["error_type"])
	}
}

func TestEmitToolCall(t *testing.T) {
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
		Environment:       "test",
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	exporter.EmitToolCall("test-session", "anthropic", "test@host", "Bash", 2, "toolu_01abc")

	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	if len(receivedPayload.Streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	stream := receivedPayload.Streams[0]

	// Verify labels
	if stream.Stream["log_type"] != "tool_call" {
		t.Errorf("expected log_type 'tool_call', got %q", stream.Stream["log_type"])
	}
	if stream.Stream["tool_name"] != "Bash" {
		t.Errorf("expected tool_name 'Bash', got %q", stream.Stream["tool_name"])
	}

	// Parse JSON body
	var logBody map[string]interface{}
	if err := json.Unmarshal([]byte(stream.Values[0][1]), &logBody); err != nil {
		t.Fatalf("failed to parse log body: %v", err)
	}

	// Verify tool_index and tool_use_id in body (not labels - high cardinality)
	if v, ok := logBody["tool_index"].(float64); !ok || int(v) != 2 {
		t.Errorf("expected tool_index=2, got %v", logBody["tool_index"])
	}
	if v, ok := logBody["tool_use_id"].(string); !ok || v != "toolu_01abc" {
		t.Errorf("expected tool_use_id='toolu_01abc', got %v", logBody["tool_use_id"])
	}
}

func TestEmitToolResult(t *testing.T) {
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
		Environment:       "test",
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	exporter.EmitToolResult("test-session", "anthropic", "test@host", "Read", "toolu_01xyz", true)

	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	if len(receivedPayload.Streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	stream := receivedPayload.Streams[0]

	// Verify labels
	if stream.Stream["log_type"] != "tool_result" {
		t.Errorf("expected log_type 'tool_result', got %q", stream.Stream["log_type"])
	}
	if stream.Stream["tool_name"] != "Read" {
		t.Errorf("expected tool_name 'Read', got %q", stream.Stream["tool_name"])
	}

	// Parse JSON body
	var logBody map[string]interface{}
	if err := json.Unmarshal([]byte(stream.Values[0][1]), &logBody); err != nil {
		t.Fatalf("failed to parse log body: %v", err)
	}

	// Verify is_error and tool_use_id in body
	if v, ok := logBody["is_error"].(bool); !ok || !v {
		t.Errorf("expected is_error=true, got %v", logBody["is_error"])
	}
	if v, ok := logBody["tool_use_id"].(string); !ok || v != "toolu_01xyz" {
		t.Errorf("expected tool_use_id='toolu_01xyz', got %v", logBody["tool_use_id"])
	}
}

func TestEmitToolResult_NoError(t *testing.T) {
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
		Environment:       "test",
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	exporter.EmitToolResult("test-session", "anthropic", "test@host", "Bash", "toolu_02abc", false)

	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	if len(receivedPayload.Streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	// Parse JSON body
	var logBody map[string]interface{}
	if err := json.Unmarshal([]byte(receivedPayload.Streams[0].Values[0][1]), &logBody); err != nil {
		t.Fatalf("failed to parse log body: %v", err)
	}

	// Verify is_error is false
	if v, ok := logBody["is_error"].(bool); !ok || v {
		t.Errorf("expected is_error=false, got %v", logBody["is_error"])
	}
}

func TestEmitEvent_NumericValuesInBodyNotLabels(t *testing.T) {
	// Verify that numeric values (turn_depth, tool_index, tokens) go in JSON body, not labels
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	patterns := PatternData{TurnDepth: 10, ToolStreak: 5}
	tokens := TokenData{InputTokens: 1000}
	exporter.EmitTurnEnd("test-session", "anthropic", "test@host", "end_turn", false, "", patterns, tokens)

	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	stream := receivedPayload.Streams[0]

	// Numeric values should NOT be in labels
	forbiddenLabels := []string{"turn_depth", "tool_streak", "retry_count", "session_tool_count",
		"tool_index", "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"}
	for _, label := range forbiddenLabels {
		if _, exists := stream.Stream[label]; exists {
			t.Errorf("numeric value %q should not be in labels", label)
		}
	}
}

func TestEmitEvent_ToolUseIDInBodyNotLabels(t *testing.T) {
	// tool_use_id is high cardinality - must be in JSON body, not labels
	var receivedPayload LokiPushRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         time.Hour,
		UseGzip:           false,
	}

	exporter, err := NewLokiExporter(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	exporter.EmitToolCall("test-session", "anthropic", "test@host", "Bash", 0, "toolu_unique123")

	time.Sleep(100 * time.Millisecond)
	exporter.Close()

	stream := receivedPayload.Streams[0]

	// tool_use_id should NOT be in labels (high cardinality)
	if _, exists := stream.Stream["tool_use_id"]; exists {
		t.Error("tool_use_id should not be in labels (high cardinality)")
	}
}

func TestFindStopReasonInChunks_WithSSEPrefix(t *testing.T) {
	// Chunks stored in StreamChunk.Raw have "data: " prefix from SSE parsing.
	// findStopReasonInChunks must strip this prefix before JSON unmarshal.
	chunks := []string{
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`data: [DONE]`,
	}

	got := findStopReasonInChunks(chunks)
	if got != "end_turn" {
		t.Errorf("findStopReasonInChunks() = %q, want %q", got, "end_turn")
	}
}

func TestFindStopReasonInChunks_WithoutPrefix(t *testing.T) {
	// Raw JSON without SSE prefix should also work
	chunks := []string{
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":100}}`,
	}

	got := findStopReasonInChunks(chunks)
	if got != "max_tokens" {
		t.Errorf("findStopReasonInChunks() = %q, want %q", got, "max_tokens")
	}
}

func TestFindStopReasonInChunks_EmptyChunks(t *testing.T) {
	got := findStopReasonInChunks(nil)
	if got != "" {
		t.Errorf("findStopReasonInChunks(nil) = %q, want empty", got)
	}

	got = findStopReasonInChunks([]string{})
	if got != "" {
		t.Errorf("findStopReasonInChunks([]) = %q, want empty", got)
	}
}

func TestFindStopReasonInChunks_NoMessageDelta(t *testing.T) {
	chunks := []string{
		`data: {"type":"content_block_start","content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`,
		`data: {"type":"content_block_stop","index":0}`,
	}

	got := findStopReasonInChunks(chunks)
	if got != "" {
		t.Errorf("findStopReasonInChunks() = %q, want empty (no message_delta)", got)
	}
}

func TestLokiExporter_TransportLabel(t *testing.T) {
	var receivedPayload LokiPushRequest
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	exporter, err := NewLokiExporter(LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewLokiExporter: %v", err)
	}

	// Push with transport=bedrock via _meta
	entry := map[string]interface{}{
		"type": "request",
		"body": `{"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`,
		"_meta": map[string]interface{}{
			"ts":             time.Now().Format(time.RFC3339Nano),
			"machine":        "test-machine",
			"transport":      "bedrock",
			"model_override": "us.anthropic.claude-sonnet-4-5-20250929-v2:0",
		},
	}
	exporter.Push(entry, "anthropic")

	time.Sleep(200 * time.Millisecond)
	exporter.Close()

	mu.Lock()
	defer mu.Unlock()

	if len(receivedPayload.Streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	stream := receivedPayload.Streams[0]

	// transport label should be "bedrock"
	if stream.Stream["transport"] != "bedrock" {
		t.Errorf("transport = %q, want 'bedrock'", stream.Stream["transport"])
	}

	// model should come from model_override, not body
	if stream.Stream["model"] != "us.anthropic.claude-sonnet-4-5-20250929-v2:0" {
		t.Errorf("model = %q, want 'us.anthropic.claude-sonnet-4-5-20250929-v2:0'", stream.Stream["model"])
	}

	// provider should still be anthropic
	if stream.Stream["provider"] != "anthropic" {
		t.Errorf("provider = %q, want 'anthropic'", stream.Stream["provider"])
	}
}

func TestLokiExporter_TransportDefaultDirect(t *testing.T) {
	var receivedPayload LokiPushRequest
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	exporter, err := NewLokiExporter(LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         1,
		BatchWait:         100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewLokiExporter: %v", err)
	}

	// Push without _meta.transport — should default to "direct"
	entry := map[string]interface{}{
		"type": "request",
		"body": `{"model":"claude-sonnet-4-20250514","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`,
		"_meta": map[string]interface{}{
			"ts":      time.Now().Format(time.RFC3339Nano),
			"machine": "test-machine",
		},
	}
	exporter.Push(entry, "anthropic")

	time.Sleep(200 * time.Millisecond)
	exporter.Close()

	mu.Lock()
	defer mu.Unlock()

	if len(receivedPayload.Streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	stream := receivedPayload.Streams[0]

	if stream.Stream["transport"] != "direct" {
		t.Errorf("transport = %q, want 'direct' (default)", stream.Stream["transport"])
	}
}

func TestLokiExporter_DifferentTransportsSeparateStreams(t *testing.T) {
	var receivedPayload LokiPushRequest
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedPayload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	exporter, err := NewLokiExporter(LokiExporterConfig{
		URL:               server.URL,
		AllowInsecureHTTP: true,
		BatchSize:         10,
		BatchWait:         100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewLokiExporter: %v", err)
	}

	// Push one "direct" and one "bedrock" entry
	directEntry := map[string]interface{}{
		"type": "request",
		"body": `{"model":"claude-sonnet-4-20250514","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`,
		"_meta": map[string]interface{}{
			"ts":      time.Now().Format(time.RFC3339Nano),
			"machine": "test-machine",
		},
	}
	bedrockEntry := map[string]interface{}{
		"type": "request",
		"body": `{"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`,
		"_meta": map[string]interface{}{
			"ts":             time.Now().Format(time.RFC3339Nano),
			"machine":        "test-machine",
			"transport":      "bedrock",
			"model_override": "us.anthropic.claude-sonnet-4-5-20250929-v2:0",
		},
	}
	exporter.Push(directEntry, "anthropic")
	exporter.Push(bedrockEntry, "anthropic")

	time.Sleep(200 * time.Millisecond)
	exporter.Close()

	mu.Lock()
	defer mu.Unlock()

	// Should have 2 separate streams due to different transport values
	if len(receivedPayload.Streams) < 2 {
		t.Errorf("expected 2 streams (different transports), got %d", len(receivedPayload.Streams))
	}
}

func TestNewLokiExporter_RejectsPlainHTTPWithoutOptIn(t *testing.T) {
	_, err := NewLokiExporter(LokiExporterConfig{URL: "http://loki.example.com/loki/api/v1/push"})
	if err == nil || !strings.Contains(err.Error(), "allow_insecure_http") {
		t.Fatalf("expected plain HTTP opt-in error, got %v", err)
	}
}

func TestNewLokiExporter_AllowsPlainHTTPWithOptIn(t *testing.T) {
	exporter, err := NewLokiExporter(LokiExporterConfig{URL: "http://loki.example.com/loki/api/v1/push", AllowInsecureHTTP: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer exporter.Close()
}

func TestNewLokiExporter_RejectsCredentialsInURL(t *testing.T) {
	_, err := NewLokiExporter(LokiExporterConfig{URL: "https://user" + ":" + "pass@loki.example.com/loki/api/v1/push"})
	if err == nil || !strings.Contains(err.Error(), "auth_token") {
		t.Fatalf("expected credential rejection, got %v", err)
	}
	if strings.Contains(err.Error(), "user:pass") {
		t.Fatalf("error leaked credentials: %v", err)
	}
}

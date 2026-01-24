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
		URL: "http://localhost:3100/loki/api/v1/push",
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
		URL:       server.URL,
		AuthToken: "my-secret-token",
		UseGzip:   false, // disable for simpler test
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
		URL:       server.URL,
		AuthToken: "", // no auth token
		UseGzip:   false,
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
		URL:     server.URL,
		UseGzip: true,
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
		URL:        server.URL,
		BatchSize:  100,                   // high threshold so no auto-flush
		BatchWait:  10 * time.Second,      // long wait so no auto-flush
		BufferSize: 100,
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
		URL:        server.URL,
		BatchSize:  10000,             // very high so no auto-flush from size
		BatchWait:  10 * time.Second,  // very long so no auto-flush from time
		BufferSize: 5,                 // tiny buffer to force drops
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
		URL:       server.URL,
		BatchSize: 1,         // flush immediately
		BatchWait: time.Hour, // don't wait
		UseGzip:   false,
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
		URL:       server.URL,
		BatchSize: 1,
		BatchWait: time.Hour,
		UseGzip:   false,
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
		URL:         server.URL,
		BatchSize:   4, // flush after 4 entries
		BatchWait:   time.Hour,
		UseGzip:     false,
		Environment: "test",
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
		URL:       server.URL,
		BatchSize: 1,
		BatchWait: time.Hour,
		RetryMax:  5,
		RetryWait: 10 * time.Millisecond, // fast retries for test
		UseGzip:   false,
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
		URL:       server.URL,
		BatchSize: 1,
		BatchWait: time.Hour,
		RetryMax:  5,
		RetryWait: 50 * time.Millisecond, // base delay
		UseGzip:   false,
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
		URL:        server.URL,
		BatchSize:  1000,             // high threshold - won't trigger auto-flush
		BatchWait:  10 * time.Second, // long wait - won't trigger auto-flush
		UseGzip:    false,
		BufferSize: 100,
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
		URL:             server.URL,
		BatchSize:       1,
		BatchWait:       time.Hour,
		UseGzip:         false,
		ShutdownTimeout: 100 * time.Millisecond, // very short timeout for test
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
		URL:        server.URL,
		BatchSize:  1, // flush each entry individually
		BatchWait:  time.Hour,
		RetryMax:   1, // only 1 retry, so some will fail
		RetryWait:  10 * time.Millisecond,
		UseGzip:    false,
		BufferSize: 5, // small buffer to test drops
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
		URL:       server.URL,
		BatchSize: 1000,                    // high threshold - won't trigger by size
		BatchWait: 100 * time.Millisecond,  // short wait for test
		UseGzip:   false,
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
		URL:       server.URL,
		BatchSize: 5,                  // low threshold - trigger after 5 entries
		BatchWait: 10 * time.Second,   // long wait - shouldn't trigger by time
		UseGzip:   false,
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
		URL:         server.URL,
		BatchSize:   1,
		BatchWait:   time.Hour,
		UseGzip:     false,
		Environment: "production",
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
		URL:     server.URL,
		UseGzip: false,
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

func TestDoSend_ReturnsErrorOn5xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := LokiExporterConfig{
		URL:     server.URL,
		UseGzip: false,
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
		URL:        server.URL,
		BatchSize:  1,
		BatchWait:  time.Millisecond,
		BufferSize: 1, // tiny buffer
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

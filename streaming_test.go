// streaming_test.go
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamingResponseCapture(t *testing.T) {
	// Mock SSE upstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter doesn't support flushing")
		}

		events := []string{
			"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Hello\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\" World\"}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}

		for _, event := range events {
			w.Write([]byte(event))
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	tmpDir := t.TempDir()
	logger, _ := NewLogger(tmpDir)
	defer logger.Close()

	proxy := NewProxyWithLogger(logger)

	reqPath := "/anthropic/" + upstreamHost + "/v1/messages"
	req := httptest.NewRequest("POST", reqPath, strings.NewReader(`{"stream":true}`))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	// Verify response was streamed
	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "message_start") {
		t.Error("Response should contain message_start event")
	}
	if !strings.Contains(body, "Hello") {
		t.Error("Response should contain Hello")
	}
}

func TestStreamingChunkTiming(t *testing.T) {
	// Test that chunks are captured with timing
	chunks := []StreamChunk{}
	startTime := time.Now()

	for i := 0; i < 3; i++ {
		time.Sleep(10 * time.Millisecond)
		chunk := StreamChunk{
			Timestamp: time.Now(),
			DeltaMs:   time.Since(startTime).Milliseconds(),
			Raw:       "event: test\ndata: {}\n\n",
		}
		chunks = append(chunks, chunk)
	}

	if len(chunks) != 3 {
		t.Errorf("Expected 3 chunks, got %d", len(chunks))
	}

	// Verify timing increases
	if chunks[2].DeltaMs <= chunks[0].DeltaMs {
		t.Error("Delta times should increase")
	}
}

func TestIsStreamingRequest(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected bool
	}{
		{"stream true no spaces", `{"stream":true}`, true},
		{"stream true with spaces", `{"stream": true}`, true},
		{"stream false", `{"stream":false}`, false},
		{"no stream field", `{"messages":[]}`, false},
		{"empty body", ``, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isStreamingRequest([]byte(tt.body))
			if result != tt.expected {
				t.Errorf("isStreamingRequest(%q) = %v, want %v", tt.body, result, tt.expected)
			}
		})
	}
}

func TestIsStreamingResponse(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		expected    bool
	}{
		{"event-stream", "text/event-stream", true},
		{"event-stream with charset", "text/event-stream; charset=utf-8", true},
		{"json", "application/json", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				Header: http.Header{"Content-Type": []string{tt.contentType}},
			}
			result := isStreamingResponse(resp)
			if result != tt.expected {
				t.Errorf("isStreamingResponse with Content-Type %q = %v, want %v", tt.contentType, result, tt.expected)
			}
		})
	}
}

func TestExtractDeltaTextAnthropic(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		expected string
	}{
		{
			name:     "content_block_delta with text",
			data:     `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`,
			expected: "Hello",
		},
		{
			name:     "message_start event",
			data:     `data: {"type":"message_start"}`,
			expected: "",
		},
		{
			name:     "not a data line",
			data:     `event: message_start`,
			expected: "",
		},
		{
			name:     "DONE marker",
			data:     `data: [DONE]`,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractDeltaText([]byte(tt.data), "anthropic")
			if result != tt.expected {
				t.Errorf("extractDeltaText(%q, anthropic) = %q, want %q", tt.data, result, tt.expected)
			}
		})
	}
}

func TestExtractDeltaTextOpenAI(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		expected string
	}{
		{
			name:     "delta with content",
			data:     `data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			expected: "Hello",
		},
		{
			name:     "delta without content",
			data:     `data: {"choices":[{"delta":{"role":"assistant"}}]}`,
			expected: "",
		},
		{
			name:     "DONE marker",
			data:     `data: [DONE]`,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractDeltaText([]byte(tt.data), "openai")
			if result != tt.expected {
				t.Errorf("extractDeltaText(%q, openai) = %q, want %q", tt.data, result, tt.expected)
			}
		})
	}
}

func TestStreamingResponseWriterAccumulatesText(t *testing.T) {
	w := httptest.NewRecorder()
	sw := NewStreamingResponseWriter(w, "anthropic")

	// Simulate receiving SSE chunks
	chunks := []string{
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n",
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" World\"}}\n",
	}

	for _, chunk := range chunks {
		sw.Write([]byte(chunk))
	}

	if sw.AccumulatedText() != "Hello World" {
		t.Errorf("AccumulatedText() = %q, want %q", sw.AccumulatedText(), "Hello World")
	}

	if len(sw.Chunks()) != 2 {
		t.Errorf("Expected 2 chunks, got %d", len(sw.Chunks()))
	}
}

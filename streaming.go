// streaming.go
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// SessionManager placeholder - defined properly in Task 17
type SessionManager struct{}

func (sm *SessionManager) RecordResponse(sessionID string, seq int, reqBody, respBody []byte, provider string) {
}

// isStreamingRequest checks if the request is asking for streaming
func isStreamingRequest(body []byte) bool {
	s := string(body)
	return strings.Contains(s, `"stream":true`) ||
		strings.Contains(s, `"stream": true`)
}

// isStreamingResponse checks if the response is SSE
func isStreamingResponse(resp *http.Response) bool {
	contentType := resp.Header.Get("Content-Type")
	return strings.HasPrefix(contentType, "text/event-stream")
}

// StreamingResponseWriter wraps http.ResponseWriter to capture chunks and accumulate text
type StreamingResponseWriter struct {
	http.ResponseWriter
	chunks          []StreamChunk
	startTime       time.Time
	lastChunk       time.Time
	accumulatedText strings.Builder
	provider        string
}

func NewStreamingResponseWriter(w http.ResponseWriter, provider string) *StreamingResponseWriter {
	now := time.Now()
	return &StreamingResponseWriter{
		ResponseWriter: w,
		chunks:         make([]StreamChunk, 0),
		startTime:      now,
		lastChunk:      now,
		provider:       provider,
	}
}

func (s *StreamingResponseWriter) Write(data []byte) (int, error) {
	now := time.Now()

	chunk := StreamChunk{
		Timestamp: now,
		DeltaMs:   now.Sub(s.startTime).Milliseconds(),
		Raw:       string(data),
	}
	s.chunks = append(s.chunks, chunk)
	s.lastChunk = now

	// Extract and accumulate text deltas for fingerprinting
	if text := extractDeltaText(data, s.provider); text != "" {
		s.accumulatedText.WriteString(text)
	}

	return s.ResponseWriter.Write(data)
}

func (s *StreamingResponseWriter) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *StreamingResponseWriter) Chunks() []StreamChunk {
	return s.chunks
}

func (s *StreamingResponseWriter) AccumulatedText() string {
	return s.accumulatedText.String()
}

// extractDeltaText extracts text content from SSE delta events (provider-aware)
func extractDeltaText(data []byte, provider string) string {
	line := string(data)

	// SSE format: "data: {...}\n"
	if !strings.HasPrefix(line, "data: ") {
		return ""
	}

	jsonStr := strings.TrimPrefix(line, "data: ")
	jsonStr = strings.TrimSpace(jsonStr)

	if jsonStr == "[DONE]" || jsonStr == "" {
		return ""
	}

	var event map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &event); err != nil {
		return ""
	}

	if provider == "anthropic" {
		// Anthropic: {"type":"content_block_delta","delta":{"type":"text_delta","text":"..."}}
		if event["type"] != "content_block_delta" {
			return ""
		}
		if delta, ok := event["delta"].(map[string]interface{}); ok {
			if text, ok := delta["text"].(string); ok {
				return text
			}
		}
	} else if provider == "openai" {
		// OpenAI: {"choices":[{"delta":{"content":"..."}}]}
		if choices, ok := event["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if delta, ok := choice["delta"].(map[string]interface{}); ok {
					if content, ok := delta["content"].(string); ok {
						return content
					}
				}
			}
		}
	}

	return ""
}

// streamResponse handles streaming responses from upstream
// NOTE: sessionManager param for future fingerprinting (Task 18)
func streamResponse(w http.ResponseWriter, resp *http.Response, logger *Logger, sm *SessionManager, sessionID, provider string, seq int, startTime time.Time, reqBody []byte) error {
	sw := NewStreamingResponseWriter(w, provider)

	// Copy headers
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// Stream the response
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			sw.Write(line)
			sw.Flush()
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	// Log the complete streaming response
	if logger != nil {
		ttfb := int64(0)
		if len(sw.chunks) > 0 {
			ttfb = sw.chunks[0].DeltaMs
		}
		timing := ResponseTiming{
			TTFBMs:  ttfb,
			TotalMs: time.Since(startTime).Milliseconds(),
		}
		logger.LogResponse(sessionID, provider, seq, resp.StatusCode, resp.Header, nil, sw.chunks, timing)
	}

	// Future: Update session fingerprint with accumulated response text (Task 18)
	// SessionManager integration will be completed in Task 17

	return nil
}

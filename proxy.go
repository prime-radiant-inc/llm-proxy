// proxy.go
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Proxy struct {
	client         *http.Client
	logger         *Logger
	sessionManager *SessionManager
}

// createPassthroughClient creates an HTTP client configured for true passthrough proxying
func createPassthroughClient() *http.Client {
	transport := &http.Transport{
		// Disable automatic compression - preserve original encoding for passthrough
		DisableCompression: true,
		// Use reasonable timeouts for LLM APIs (can have long responses)
		ResponseHeaderTimeout: 0, // No timeout - streaming can be very long
		// Enable HTTP/2 (default in Go, but be explicit)
		ForceAttemptHTTP2: true,
	}
	return &http.Client{
		Transport: transport,
		// No timeout - let context handle cancellation
		Timeout: 0,
	}
}

func NewProxy() *Proxy {
	return &Proxy{
		client: createPassthroughClient(),
	}
}

func NewProxyWithLogger(logger *Logger) *Proxy {
	return &Proxy{
		client: createPassthroughClient(),
		logger: logger,
	}
}

func NewProxyWithSessionManager(logger *Logger, sm *SessionManager) *Proxy {
	return &Proxy{
		client:         createPassthroughClient(),
		logger:         logger,
		sessionManager: sm,
	}
}

func (p *Proxy) generateSessionID() string {
	return time.Now().UTC().Format("20060102-150405") + "-" + randomHex(4)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Fallback to time-based uniqueness if crypto/rand fails
		// This shouldn't happen on modern systems but handle gracefully
		return hex.EncodeToString([]byte(time.Now().String())[:n])
	}
	return hex.EncodeToString(b)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// Parse the proxy URL
	provider, upstream, path, err := ParseProxyURL(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// For OpenAI requests, dynamically route based on auth type:
	// - JWT tokens (ChatGPT OAuth) → chatgpt.com/backend-api/codex
	// - API keys (sk-...) → api.openai.com
	if provider == "openai" && upstream == "api.openai.com" {
		if isJWTAuth(r.Header) {
			upstream = "chatgpt.com"
			path = "/backend-api/codex" + path
		}
	}

	// Determine scheme (use http for tests, https for real)
	scheme := "https"
	if isLocalhost(upstream) {
		scheme = "http"
	}

	// Build upstream URL
	upstreamURL := scheme + "://" + upstream + path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	// Buffer request body for logging
	var reqBody []byte
	if r.Body != nil {
		reqBody, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body: "+err.Error(), http.StatusInternalServerError)
			return
		}
		r.Body.Close()
	}

	// Create forwarded request with buffered body
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "failed to create request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy headers
	copyHeaders(proxyReq.Header, r.Header)

	// Set host header
	proxyReq.Host = upstream

	// Determine session ID and sequence for logging (conversation endpoints only)
	var sessionID string
	var seq int
	var isNewSession bool
	var requestID string
	shouldLog := p.logger != nil && isConversationEndpoint(path)

	if shouldLog {
		// Generate unique request ID for this API call
		requestID = uuid.New().String()

		if p.sessionManager != nil {
			var err error
			sessionID, seq, isNewSession, err = p.sessionManager.GetOrCreateSession(reqBody, provider, upstream, r.Header, path)
			if err != nil {
				// Fallback to generating a new session
				sessionID = p.generateSessionID()
				seq = 1
				isNewSession = true
			}
		} else {
			// No session manager - generate new session for each request
			sessionID = p.generateSessionID()
			seq = 1
			isNewSession = true
		}

		// Only log session_start on new sessions (seq == 1)
		if isNewSession {
			p.logger.LogSessionStart(sessionID, provider, upstream)
		}
		p.logger.LogRequest(sessionID, provider, seq, r.Method, path, r.Header, reqBody, requestID)
	}

	// Make request to upstream
	resp, err := p.client.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Handle streaming vs non-streaming responses
	if isStreamingResponse(resp) {
		var loggerForStream *Logger
		var smForStream *SessionManager
		if shouldLog {
			loggerForStream = p.logger
			smForStream = p.sessionManager
		}
		streamResponse(w, resp, loggerForStream, smForStream, sessionID, provider, seq, startTime, reqBody, requestID)
		return
	}

	// Non-streaming response
	// Record TTFB
	ttfb := time.Since(startTime)

	// Buffer response body for logging
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "failed to read response body: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Record total time
	totalTime := time.Since(startTime)

	// Log response and record fingerprint for session tracking (conversation endpoints only)
	if shouldLog {
		timing := ResponseTiming{
			TTFBMs:  ttfb.Milliseconds(),
			TotalMs: totalTime.Milliseconds(),
		}
		p.logger.LogResponse(sessionID, provider, seq, resp.StatusCode, resp.Header, respBody, nil, timing, requestID)
	}

	// Copy response headers
	copyHeaders(w.Header(), resp.Header)

	// Write status code
	w.WriteHeader(resp.StatusCode)

	// Write response body
	w.Write(respBody)
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// isLocalhost checks if the host is localhost for determining http vs https scheme.
// Uses strings.HasPrefix for safety (avoids panics on short strings).
func isLocalhost(host string) bool {
	return strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "localhost")
}

// isJWTAuth checks if the Authorization header contains a JWT token (ChatGPT OAuth)
// rather than an OpenAI API key (sk-...).
// JWT format: three base64-encoded parts separated by dots (header.payload.signature)
func isJWTAuth(headers http.Header) bool {
	auth := headers.Get("Authorization")
	if auth == "" {
		return false
	}

	// Extract token from "Bearer <token>"
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == auth {
		// No "Bearer " prefix
		return false
	}

	// API keys start with sk-
	if strings.HasPrefix(token, "sk-") {
		return false
	}

	// JWT has exactly 3 parts separated by dots
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}

	// Each part should be non-empty (basic validation)
	for _, part := range parts {
		if len(part) == 0 {
			return false
		}
	}

	return true
}

// isConversationEndpoint returns true for API endpoints that represent conversations
// (i.e., have messages that can be tracked for session continuity)
func isConversationEndpoint(path string) bool {
	// Anthropic
	if path == "/v1/messages" {
		return true
	}

	// OpenAI Chat/Completions
	if path == "/v1/chat/completions" || path == "/v1/completions" || path == "/v1/responses" {
		return true
	}

	// OpenAI Threads API - matches /v1/threads/{id}/messages or /v1/threads/{id}/runs[/...]
	if strings.HasPrefix(path, "/v1/threads/") {
		parts := strings.Split(path, "/")
		// /v1/threads/{id}/messages or /v1/threads/{id}/runs
		if len(parts) >= 5 && (parts[4] == "messages" || parts[4] == "runs") {
			return true
		}
	}

	// ChatGPT backend API (used with OAuth authentication)
	// Paths like /backend-api/codex/v1/responses
	if strings.HasPrefix(path, "/backend-api/") && strings.HasSuffix(path, "/responses") {
		return true
	}

	return false
}

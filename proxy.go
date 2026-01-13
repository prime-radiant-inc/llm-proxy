// proxy.go
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Proxy struct {
	client    *http.Client
	logger    *Logger
	sessionMu sync.Mutex
	seqNums   map[string]int
}

func NewProxy() *Proxy {
	return &Proxy{
		client:  &http.Client{},
		seqNums: make(map[string]int),
	}
}

func NewProxyWithLogger(logger *Logger) *Proxy {
	return &Proxy{
		client:  &http.Client{},
		logger:  logger,
		seqNums: make(map[string]int),
	}
}

func (p *Proxy) generateSessionID() string {
	return time.Now().UTC().Format("20060102-150405") + "-" + randomHex(4)
}

func (p *Proxy) nextSeq(sessionID string) int {
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	seq := p.seqNums[sessionID]
	p.seqNums[sessionID] = seq + 1
	return seq
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
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

	// Generate session ID and sequence for logging
	var sessionID string
	var seq int
	if p.logger != nil {
		sessionID = p.generateSessionID()
		seq = p.nextSeq(sessionID)
		p.logger.LogSessionStart(sessionID, provider, upstream)
		p.logger.LogRequest(sessionID, provider, seq, r.Method, path, r.Header, reqBody)
	}

	// Make request to upstream
	resp, err := p.client.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

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

	// Log response
	if p.logger != nil {
		timing := ResponseTiming{
			TTFBMs:  ttfb.Milliseconds(),
			TotalMs: totalTime.Milliseconds(),
		}
		p.logger.LogResponse(sessionID, provider, seq, resp.StatusCode, resp.Header, respBody, nil, timing)
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

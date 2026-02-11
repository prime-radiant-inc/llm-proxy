package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/google/uuid"
)

// validModelID validates Bedrock model IDs to prevent SSRF.
// Allows alphanumeric, dots, hyphens, underscores, and optional :version suffix.
var validModelID = regexp.MustCompile(`^[a-zA-Z0-9._-]+(:[0-9]+)?$`)

// bedrockMaxRequestBody is the max request body size for Bedrock requests (16 MB).
const bedrockMaxRequestBody = 16 << 20

// bedrockMaxBuffer is the max buffer size for observability decode (4 MB).
const bedrockMaxBuffer = 4 << 20

// bedrockMaxConcurrent is the max concurrent Bedrock requests.
const bedrockMaxConcurrent = 5

// bedrockMaxErrorBody is the max error response body to read (1 MB).
const bedrockMaxErrorBody = 1 << 20

// LimitedWriter stops writing after N bytes. NEVER returns an error —
// this is critical because io.TeeReader propagates Write errors to io.Copy,
// which would break the client stream.
type LimitedWriter struct {
	W        io.Writer
	N        int64
	Overflow bool
}

func (lw *LimitedWriter) Write(p []byte) (int, error) {
	if lw.Overflow {
		return len(p), nil
	}
	if int64(len(p)) > lw.N {
		lw.Overflow = true
		return len(p), nil
	}
	n, _ := lw.W.Write(p)
	lw.N -= int64(n)
	return len(p), nil
}

// extractModelID extracts and validates the model ID from a Bedrock URL path.
// Path format: /model/{modelId}/invoke or /model/{modelId}/invoke-with-response-stream
func extractModelID(path string) (string, error) {
	trimmed := strings.TrimPrefix(path, "/model/")
	if trimmed == path {
		return "", fmt.Errorf("path does not start with /model/")
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		return "", fmt.Errorf("empty model ID in path %q", path)
	}
	modelID := parts[0]
	if !validModelID.MatchString(modelID) {
		return "", fmt.Errorf("invalid model ID: %q", modelID)
	}
	return modelID, nil
}

// isBedrockStreaming returns true if the path ends with invoke-with-response-stream.
func isBedrockStreaming(path string) bool {
	return strings.HasSuffix(path, "/invoke-with-response-stream")
}

// decodeBedrockEventstream decodes a complete Bedrock eventstream response buffer
// into normalized StreamChunks (with "data: " prefix for parser compatibility).
// Returns any successfully decoded chunks even if the buffer is truncated.
func decodeBedrockEventstream(buf []byte) ([]StreamChunk, error) {
	if len(buf) == 0 {
		return nil, nil
	}

	decoder := eventstream.NewDecoder()
	reader := bytes.NewReader(buf)
	var chunks []StreamChunk
	var lastErr error

	for reader.Len() > 0 {
		msg, err := decoder.Decode(reader, nil)
		if err != nil {
			lastErr = fmt.Errorf("eventstream decode: %w", err)
			break
		}

		// Parse the frame payload: {"bytes": "<base64>", "p": "<padding>"}
		var payload struct {
			Bytes string `json:"bytes"`
		}
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			// Skip frames with non-JSON payload (e.g., exception frames)
			continue
		}
		if payload.Bytes == "" {
			continue
		}

		// Base64-decode to get the Anthropic event JSON
		decoded, err := base64.StdEncoding.DecodeString(payload.Bytes)
		if err != nil {
			// Try URL-safe encoding as fallback
			decoded, err = base64.URLEncoding.DecodeString(payload.Bytes)
			if err != nil {
				lastErr = fmt.Errorf("base64 decode: %w", err)
				continue
			}
		}

		// Prepend "data: " for compatibility with ParseStreamingResponse
		chunks = append(chunks, StreamChunk{
			Raw:       "data: " + string(decoded),
			Timestamp: time.Now(),
		})
	}

	return chunks, lastErr
}

// bedrockState holds per-proxy Bedrock resources initialized at startup.
type bedrockState struct {
	region     string
	credProv   aws.CredentialsProvider
	signer    *v4.Signer
	client    *http.Client
	semaphore chan struct{}
	decodeErrors int64 // atomic counter
}

// initBedrock initializes Bedrock resources. Returns nil if Bedrock is not configured.
func initBedrock(region string) (*bedrockState, error) {
	if region == "" {
		return nil, nil
	}

	if err := ValidateBedrockRegion(region); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	return &bedrockState{
		region:   region,
		credProv: cfg.Credentials,
		signer:   v4.NewSigner(),
		client: &http.Client{
			Transport: &http.Transport{
				DisableCompression:    true,
				ResponseHeaderTimeout: 300 * time.Second,
				ForceAttemptHTTP2:     true,
			},
			Timeout: 0,
		},
		semaphore: make(chan struct{}, bedrockMaxConcurrent),
	}, nil
}

// serveBedrock handles Bedrock pass-through requests. The proxy signs requests
// with SigV4, forwards to Bedrock, streams the response to the client, and
// decodes the eventstream for observability after the stream completes.
func (p *Proxy) serveBedrock(w http.ResponseWriter, r *http.Request) {
	if p.bedrock == nil {
		http.Error(w, "Bedrock not configured", http.StatusServiceUnavailable)
		return
	}

	startTime := time.Now()

	// Extract and validate model ID
	modelID, err := extractModelID(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	streaming := isBedrockStreaming(r.URL.Path)

	// Acquire concurrency semaphore
	select {
	case p.bedrock.semaphore <- struct{}{}:
		defer func() { <-p.bedrock.semaphore }()
	case <-r.Context().Done():
		http.Error(w, "request cancelled", http.StatusServiceUnavailable)
		return
	}

	// Read request body (capped)
	var reqBody []byte
	if r.Body != nil {
		reqBody, err = io.ReadAll(io.LimitReader(r.Body, bedrockMaxRequestBody))
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusInternalServerError)
			return
		}
		r.Body.Close()
	}

	// Use provider=anthropic — Bedrock payloads use the Anthropic JSON format
	provider := "anthropic"
	upstream := fmt.Sprintf("bedrock-runtime.%s.amazonaws.com", p.bedrock.region)

	// Session tracking and logging setup
	var sessionID string
	var seq int
	var isNewSession bool
	var requestID string
	var patternState *PatternState

	// Bedrock paths are always conversation endpoints
	shouldLog := p.logger != nil

	if shouldLog {
		requestID = uuid.New().String()

		// Set Bedrock context for Loki transport/model labels on response entries
		if mw, ok := p.logger.(*MultiWriter); ok {
			mw.SetBedrockContext(requestID, modelID)
			defer mw.ClearBedrockContext(requestID)
		}

		if p.sessionManager != nil {
			sessionID, seq, isNewSession, err = p.sessionManager.GetOrCreateSession(reqBody, provider, upstream, r.Header, r.URL.Path)
			if err != nil {
				sessionID = p.generateSessionID()
				seq = 1
				isNewSession = true
			}

			if p.eventEmitter != nil {
				patternState, _ = p.sessionManager.LoadPatternState(sessionID)
				if patternState == nil {
					patternState = &PatternState{PendingToolIDs: make(map[string]string)}
				}
				errorRecovered := patternState.LastWasError
				hadError := p.processToolResultsAndEmitEvents(reqBody, sessionID, provider, patternState)
				patternState.LastWasError = hadError
				patternState.TurnCount++
				p.eventEmitter.EmitTurnStart(sessionID, provider, p.machineID, patternState.TurnCount, errorRecovered)
			}
		} else {
			sessionID = p.generateSessionID()
			seq = 1
			isNewSession = true
		}

		if isNewSession {
			p.logger.LogSessionStart(sessionID, provider, upstream)
		}
		p.logger.LogRequest(sessionID, provider, seq, r.Method, r.URL.Path, r.Header, reqBody, requestID)
	}

	// Build upstream URL — path stays the same since CC sends the Bedrock path format
	upstreamURL := fmt.Sprintf("https://%s%s", upstream, r.URL.Path)

	// Create the upstream request
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	// Whitelist headers — only copy Content-Type and Accept to avoid SigV4 conflicts
	if ct := r.Header.Get("Content-Type"); ct != "" {
		proxyReq.Header.Set("Content-Type", ct)
	}
	if accept := r.Header.Get("Accept"); accept != "" {
		proxyReq.Header.Set("Accept", accept)
	}

	// SigV4 sign the request
	bodyHash := sha256Hex(reqBody)
	creds, err := p.bedrock.credProv.Retrieve(r.Context())
	if err != nil {
		http.Error(w, "failed to retrieve AWS credentials", http.StatusInternalServerError)
		return
	}
	if err := p.bedrock.signer.SignHTTP(r.Context(), creds, proxyReq, bodyHash, "bedrock", p.bedrock.region, time.Now()); err != nil {
		http.Error(w, "failed to sign request", http.StatusInternalServerError)
		return
	}

	// Send to Bedrock
	resp, err := p.bedrock.client.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Non-200: forward error directly, skip eventstream decode
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, bedrockMaxErrorBody))

		if shouldLog {
			timing := ResponseTiming{
				TTFBMs:  time.Since(startTime).Milliseconds(),
				TotalMs: time.Since(startTime).Milliseconds(),
			}
			p.logger.LogResponse(sessionID, provider, seq, resp.StatusCode, resp.Header, errBody, nil, timing, requestID)
		}

		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		w.Write(errBody)
		return
	}

	if streaming {
		p.serveBedrockStreaming(w, resp, startTime, modelID, upstream, provider, sessionID, seq, reqBody, requestID, patternState, shouldLog)
	} else {
		p.serveBedrockNonStreaming(w, resp, startTime, modelID, upstream, provider, sessionID, seq, reqBody, requestID, patternState, shouldLog)
	}
}

// serveBedrockStreaming handles streaming Bedrock responses using buffer-then-decode.
func (p *Proxy) serveBedrockStreaming(w http.ResponseWriter, resp *http.Response, startTime time.Time, modelID, upstream, provider, sessionID string, seq int, reqBody []byte, requestID string, patternState *PatternState, shouldLog bool) {
	// Set up TeeReader with LimitedWriter for observability buffer
	observeBuf := bytes.NewBuffer(make([]byte, 0, bedrockMaxBuffer))
	limitedW := &LimitedWriter{W: observeBuf, N: int64(bedrockMaxBuffer)}
	tee := io.TeeReader(resp.Body, limitedW)

	// Forward headers and status
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// Stream raw bytes to client — this is the critical path
	_, copyErr := io.Copy(w, tee)

	// Critical path done — CC has the full response.
	// Now decode the buffered copy for observability.
	ttfb := time.Since(startTime)
	totalTime := time.Since(startTime)

	if limitedW.Overflow {
		log.Printf("WARNING: Bedrock response exceeded %d byte buffer, observability decode skipped (model=%s session=%s)", bedrockMaxBuffer, modelID, sessionID)
		atomic.AddInt64(&p.bedrock.decodeErrors, 1)
	}

	if copyErr != nil {
		log.Printf("WARNING: Bedrock stream copy error: %v (model=%s session=%s)", copyErr, modelID, sessionID)
	}

	// Decode eventstream frames from buffer
	var chunks []StreamChunk
	if !limitedW.Overflow && observeBuf.Len() > 0 {
		var decodeErr error
		chunks, decodeErr = decodeBedrockEventstream(observeBuf.Bytes())
		if decodeErr != nil {
			log.Printf("WARNING: Bedrock decode error: %v (model=%s session=%s, decoded %d chunks before error)", decodeErr, modelID, sessionID, len(chunks))
			atomic.AddInt64(&p.bedrock.decodeErrors, 1)
		}
	}

	if shouldLog {
		timing := ResponseTiming{
			TTFBMs:  ttfb.Milliseconds(),
			TotalMs: totalTime.Milliseconds(),
		}
		p.logger.LogResponse(sessionID, provider, seq, resp.StatusCode, resp.Header, nil, chunks, timing, requestID)

		// Emit agent observability events
		if p.eventEmitter != nil && patternState != nil && p.sessionManager != nil && len(chunks) > 0 {
			parsed := ParseStreamingResponse(chunks)
			emitResponseEvents(p.eventEmitter, p.sessionManager, sessionID, provider, p.machineID, patternState, parsed.Content, parsed.Usage, parsed.StopReason, resp.StatusCode, "")
		}
	}
}

// serveBedrockNonStreaming handles non-streaming Bedrock responses (/invoke).
func (p *Proxy) serveBedrockNonStreaming(w http.ResponseWriter, resp *http.Response, startTime time.Time, modelID, upstream, provider, sessionID string, seq int, reqBody []byte, requestID string, patternState *PatternState, shouldLog bool) {
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, bedrockMaxRequestBody))
	if err != nil {
		http.Error(w, "failed to read response body", http.StatusBadGateway)
		return
	}

	totalTime := time.Since(startTime)

	if shouldLog {
		timing := ResponseTiming{
			TTFBMs:  totalTime.Milliseconds(),
			TotalMs: totalTime.Milliseconds(),
		}
		p.logger.LogResponse(sessionID, provider, seq, resp.StatusCode, resp.Header, respBody, nil, timing, requestID)

		if p.eventEmitter != nil && patternState != nil {
			parsed := ParseResponseBody(string(respBody), upstream)
			p.processResponseAndEmitEvents(parsed, sessionID, provider, patternState, resp.StatusCode, string(respBody))
		}
	}

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// sha256Hex returns the hex-encoded SHA256 hash of the data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

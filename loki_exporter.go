// loki_exporter.go
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Log type constants for agent observability events (PRI-343)
const (
	LogTypeTurnStart  = "turn_start"
	LogTypeTurnEnd    = "turn_end"
	LogTypeToolCall   = "tool_call"
	LogTypeToolResult = "tool_result"
)

// PatternData holds agent behavior pattern metrics for JSON body (not labels)
type PatternData struct {
	TurnDepth        int `json:"turn_depth"`
	ToolStreak       int `json:"tool_streak"`
	RetryCount       int `json:"retry_count"`
	SessionToolCount int `json:"session_tool_count"`
}

// TokenData holds token usage metrics for JSON body (not labels)
type TokenData struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// LokiExporterConfig holds configuration for the Loki exporter
type LokiExporterConfig struct {
	URL               string        // Full push endpoint URL
	AuthToken         string        // ****** for auth (optional)
	AllowInsecureHTTP bool          // Explicit opt-in for plain HTTP Loki endpoints
	BatchSize         int           // Number of entries per batch
	BatchWait         time.Duration // Duration to wait before flushing batch
	RetryMax          int           // Maximum retry attempts
	RetryWait         time.Duration // Base delay between retries
	UseGzip           bool          // Enable gzip compression
	Environment       string        // Environment label
	BufferSize        int           // Channel buffer size
	ShutdownTimeout   time.Duration // Timeout for graceful shutdown
}

// LokiStream represents a single stream in the Loki push request
type LokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"`
}

// LokiPushRequest represents the Loki push API request format
type LokiPushRequest struct {
	Streams []LokiStream `json:"streams"`
}

// LokiExporterStats holds statistics about the exporter's operation
type LokiExporterStats struct {
	EntriesSent    int64
	EntriesFailed  int64
	EntriesDropped int64
	BatchesSent    int64
}

// lokiEntry is an internal struct for queued entries
type lokiEntry struct {
	entry     map[string]interface{}
	provider  string
	timestamp time.Time
	logType   string
	machine   string

	// Extended labels for richer querying (PRI-298)
	model           string // LLM model name (e.g., "claude-sonnet-4-20250514")
	statusBucket    string // HTTP status bucket: "2xx", "4xx", "5xx", or empty
	stream          string // "true" or "false" for streaming requests
	hasTools        string // "true" or "false" if request includes tools
	stopReason      string // Response stop reason (e.g., "end_turn", "max_tokens")
	ratelimitStatus string // Rate limit status from response headers

	// Agent observability labels (PRI-343)
	toolName  string // Tool name for tool_call/tool_result events
	isRetry   string // "true" or "false" for retry detection
	errorType string // rate_limit, context_length, invalid_request, server_error

	// Request replay support
	requestSHA string // SHA256 of raw request body for deterministic replay

	// Transport label distinguishes Bedrock vs direct API traffic
	transport     string // "direct" or "bedrock"
	modelOverride string // Caller-injected model ID (Bedrock: from URL path, not body)
}

// LokiExporter handles async batching and pushing logs to Loki
type LokiExporter struct {
	config     LokiExporterConfig
	client     *http.Client
	entryChan  chan lokiEntry
	closeChan  chan struct{}
	closedChan chan struct{}
	closeOnce  sync.Once

	// Stats counters (accessed atomically)
	entriesSent    int64
	entriesFailed  int64
	entriesDropped int64
	batchesSent    int64
}

// NewLokiExporter creates a new LokiExporter with the given configuration
func NewLokiExporter(cfg LokiExporterConfig) (*LokiExporter, error) {
	// Validate required fields
	if cfg.URL == "" {
		return nil, fmt.Errorf("LokiExporter: URL is required")
	}
	if _, err := validateLokiURL(cfg.URL, cfg.AllowInsecureHTTP); err != nil {
		return nil, err
	}

	// Apply defaults
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1000
	}
	if cfg.BatchWait <= 0 {
		cfg.BatchWait = 5 * time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 5
	}
	if cfg.RetryWait <= 0 {
		cfg.RetryWait = 100 * time.Millisecond
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 10000
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
	// UseGzip is a boolean - its zero value is false.
	// Application-level default of true is set in config.go's DefaultConfig().

	exporter := &LokiExporter{
		config:     cfg,
		client:     &http.Client{Timeout: 30 * time.Second},
		entryChan:  make(chan lokiEntry, cfg.BufferSize),
		closeChan:  make(chan struct{}),
		closedChan: make(chan struct{}),
	}

	// Start background worker
	go exporter.run()

	return exporter, nil
}

func validateLokiURL(raw string, allowInsecureHTTP bool) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("LokiExporter: invalid URL %s", sanitizeURLForLog(raw))
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("LokiExporter: credentials in URL are not allowed for %s; use auth_token instead", sanitizeURLForLog(raw))
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return parsed, nil
	case "http":
		if allowInsecureHTTP {
			return parsed, nil
		}
		return nil, fmt.Errorf("LokiExporter: plain HTTP requires allow_insecure_http=true for %s", sanitizeURLForLog(raw))
	default:
		return nil, fmt.Errorf("LokiExporter: unsupported URL scheme %q for %s", parsed.Scheme, sanitizeURLForLog(raw))
	}
}

// extractExtendedLabels extracts additional low-cardinality labels from log entries.
// Returns empty strings for labels that don't apply to the given log type.
func extractExtendedLabels(entry map[string]interface{}, logType string) (model, statusBucket, stream, hasTools, stopReason, ratelimitStatus string) {
	switch logType {
	case "request":
		// Parse request body to extract model, stream, and tools
		if bodyStr, ok := entry["body"].(string); ok && bodyStr != "" {
			var body map[string]interface{}
			if err := json.Unmarshal([]byte(bodyStr), &body); err == nil {
				// Extract model
				if m, ok := body["model"].(string); ok {
					model = m
				}
				// Extract stream boolean
				if s, ok := body["stream"].(bool); ok {
					if s {
						stream = "true"
					} else {
						stream = "false"
					}
				}
				// Check for tools presence
				if tools, ok := body["tools"]; ok && tools != nil {
					if toolsArr, ok := tools.([]interface{}); ok && len(toolsArr) > 0 {
						hasTools = "true"
					} else {
						hasTools = "false"
					}
				} else {
					hasTools = "false"
				}
			}
		}

	case "response":
		// Extract status bucket from HTTP status code
		if status, ok := entry["status"].(float64); ok {
			statusCode := int(status)
			if statusCode >= 200 && statusCode < 300 {
				statusBucket = "2xx"
			} else if statusCode >= 400 && statusCode < 500 {
				statusBucket = "4xx"
			} else if statusCode >= 500 {
				statusBucket = "5xx"
			}
		} else if status, ok := entry["status"].(int); ok {
			if status >= 200 && status < 300 {
				statusBucket = "2xx"
			} else if status >= 400 && status < 500 {
				statusBucket = "4xx"
			} else if status >= 500 {
				statusBucket = "5xx"
			}
		}

		// Extract rate limit status from headers
		if headers, ok := entry["headers"].(http.Header); ok {
			if rlStatus := headers.Get("Anthropic-Ratelimit-Unified-Status"); rlStatus != "" {
				ratelimitStatus = strings.ToLower(rlStatus)
			}
		} else if headers, ok := entry["headers"].(map[string]interface{}); ok {
			// Headers may come as map[string]interface{} when decoded from JSON
			if rlStatus, ok := headers["Anthropic-Ratelimit-Unified-Status"].(string); ok {
				ratelimitStatus = strings.ToLower(rlStatus)
			} else if rlStatusArr, ok := headers["Anthropic-Ratelimit-Unified-Status"].([]interface{}); ok && len(rlStatusArr) > 0 {
				if s, ok := rlStatusArr[0].(string); ok {
					ratelimitStatus = strings.ToLower(s)
				}
			}
		}

		// Extract stop_reason from response body or chunks
		stopReason = extractStopReason(entry)
	}

	return
}

// extractStopReason extracts the stop_reason from a response entry.
// For streaming responses, it looks in the final chunk's delta.
// For non-streaming, it looks in the body directly.
func extractStopReason(entry map[string]interface{}) string {
	// Try body first (non-streaming)
	if sr := extractStopReasonFromBody(entry["body"]); sr != "" {
		return sr
	}

	// For streaming responses, extract raw chunk data and search for stop_reason
	chunkData := extractChunkRawData(entry["chunks"])
	return findStopReasonInChunks(chunkData)
}

// extractStopReasonFromBody parses body JSON and extracts stop_reason
func extractStopReasonFromBody(body interface{}) string {
	bodyStr, ok := body.(string)
	if !ok || bodyStr == "" {
		return ""
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(bodyStr), &parsed); err != nil {
		return ""
	}
	if sr, ok := parsed["stop_reason"].(string); ok {
		return sr
	}
	return ""
}

// extractChunkRawData normalizes chunk data from different sources into a slice of raw JSON strings.
// Handles both []StreamChunk (direct) and []interface{} (JSON-decoded).
func extractChunkRawData(chunks interface{}) []string {
	if chunks == nil {
		return nil
	}

	// Direct StreamChunk slice (from multi_writer.go)
	if streamChunks, ok := chunks.([]StreamChunk); ok {
		result := make([]string, len(streamChunks))
		for i, c := range streamChunks {
			result[i] = c.Raw
		}
		return result
	}

	// JSON-decoded slice (from tests or serialization)
	if interfaceSlice, ok := chunks.([]interface{}); ok {
		result := make([]string, 0, len(interfaceSlice))
		for _, item := range interfaceSlice {
			if chunk, ok := item.(map[string]interface{}); ok {
				// Try "raw" (matches StreamChunk field name)
				if raw, ok := chunk["raw"].(string); ok {
					result = append(result, raw)
				}
			}
		}
		return result
	}

	return nil
}

// findStopReasonInChunks searches chunks (from end) for a message_delta event with stop_reason.
// Chunks may have an SSE "data: " prefix which must be stripped before JSON parsing.
func findStopReasonInChunks(chunkData []string) string {
	for i := len(chunkData) - 1; i >= 0; i-- {
		raw := chunkData[i]
		// Strip SSE "data: " prefix — chunks store raw SSE lines
		raw = strings.TrimPrefix(raw, "data: ")
		raw = strings.TrimSpace(raw)
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			continue
		}
		if event["type"] != "message_delta" {
			continue
		}
		delta, ok := event["delta"].(map[string]interface{})
		if !ok {
			continue
		}
		if sr, ok := delta["stop_reason"].(string); ok {
			return sr
		}
	}
	return ""
}

// Push adds a log entry to the queue for async export to Loki.
// This method is non-blocking - if the channel is full, the entry is dropped.
func (e *LokiExporter) Push(entry map[string]interface{}, provider string) {
	// Extract timestamp from _meta.ts
	timestamp := time.Now()
	if meta, ok := entry["_meta"].(map[string]interface{}); ok {
		if ts, ok := meta["ts"].(string); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				timestamp = parsed
			}
		}
	}

	// Extract log_type from entry type field
	logType := "unknown"
	if t, ok := entry["type"].(string); ok {
		logType = t
	}

	// Extract machine from _meta.machine
	machine := "unknown"
	if meta, ok := entry["_meta"].(map[string]interface{}); ok {
		if m, ok := meta["machine"].(string); ok {
			machine = m
		}
	}

	// Extract extended labels (PRI-298)
	model, statusBucket, stream, hasTools, stopReason, ratelimitStatus := extractExtendedLabels(entry, logType)

	// Extract transport and modelOverride from _meta
	transport := "direct"
	var modelOverride string
	if meta, ok := entry["_meta"].(map[string]interface{}); ok {
		if t, ok := meta["transport"].(string); ok && t != "" {
			transport = t
		}
		if mo, ok := meta["model_override"].(string); ok && mo != "" {
			modelOverride = mo
		}
	}

	// modelOverride takes precedence over body-parsed model
	if modelOverride != "" {
		model = modelOverride
	}

	// Extract request SHA for replay support
	var requestSHA string
	if logType == "request" {
		if sha, ok := entry["request_sha"].(string); ok {
			requestSHA = sha
		}
	}

	le := lokiEntry{
		entry:           entry,
		provider:        provider,
		timestamp:       timestamp,
		logType:         logType,
		machine:         machine,
		model:           model,
		statusBucket:    statusBucket,
		stream:          stream,
		hasTools:        hasTools,
		stopReason:      stopReason,
		ratelimitStatus: ratelimitStatus,
		requestSHA:      requestSHA,
		transport:       transport,
		modelOverride:   modelOverride,
	}

	// Non-blocking send with drop if full
	select {
	case e.entryChan <- le:
		// Entry queued successfully
	default:
		// Channel full, drop entry
		atomic.AddInt64(&e.entriesDropped, 1)
	}
}

// run is the background worker that batches and sends entries
func (e *LokiExporter) run() {
	defer close(e.closedChan)

	batch := make([]lokiEntry, 0, e.config.BatchSize)
	ticker := time.NewTicker(e.config.BatchWait)
	defer ticker.Stop()

	for {
		select {
		case entry := <-e.entryChan:
			batch = append(batch, entry)
			if len(batch) >= e.config.BatchSize {
				e.sendBatch(batch)
				batch = make([]lokiEntry, 0, e.config.BatchSize)
				// Reset ticker after size-triggered flush
				ticker.Reset(e.config.BatchWait)
			}

		case <-ticker.C:
			if len(batch) > 0 {
				e.sendBatch(batch)
				batch = make([]lokiEntry, 0, e.config.BatchSize)
			}

		case <-e.closeChan:
			// Drain remaining entries from channel
			draining := true
			for draining {
				select {
				case entry := <-e.entryChan:
					batch = append(batch, entry)
				default:
					draining = false
				}
			}
			// Send final batch
			if len(batch) > 0 {
				e.sendBatch(batch)
			}
			return
		}
	}
}

// sendBatch groups entries by labels and sends them to Loki with retries
func (e *LokiExporter) sendBatch(entries []lokiEntry) {
	if len(entries) == 0 {
		return
	}

	// Group entries by labels
	streams := make(map[string]*LokiStream)

	for _, entry := range entries {
		// Build labels (FR6 - low cardinality only)
		labels := map[string]string{
			"app":         "llm-proxy",
			"provider":    entry.provider,
			"environment": e.config.Environment,
			"machine":     entry.machine,
			"log_type":    entry.logType,
		}

		// Add extended labels only if they have values (PRI-298)
		if entry.model != "" {
			labels["model"] = entry.model
		}
		if entry.statusBucket != "" {
			labels["status_bucket"] = entry.statusBucket
		}
		if entry.stream != "" {
			labels["stream"] = entry.stream
		}
		if entry.hasTools != "" {
			labels["has_tools"] = entry.hasTools
		}
		if entry.stopReason != "" {
			labels["stop_reason"] = entry.stopReason
		}
		if entry.ratelimitStatus != "" {
			labels["ratelimit_status"] = entry.ratelimitStatus
		}

		// Add agent observability labels (PRI-343)
		if entry.toolName != "" {
			labels["tool_name"] = entry.toolName
		}
		if entry.isRetry != "" {
			labels["is_retry"] = entry.isRetry
		}
		if entry.errorType != "" {
			labels["error_type"] = entry.errorType
		}

		// Transport label distinguishes Bedrock vs direct API traffic
		if entry.transport != "" {
			labels["transport"] = entry.transport
		}

		// Create label key for grouping (include all labels for proper stream separation)
		labelKey := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s",
			labels["app"],
			labels["provider"],
			labels["environment"],
			labels["machine"],
			labels["log_type"],
			entry.model,
			entry.statusBucket,
			entry.stream,
			entry.hasTools,
			entry.stopReason,
			entry.ratelimitStatus,
			entry.toolName,
			entry.isRetry,
			entry.errorType,
			entry.transport,
		)

		// Get or create stream for this label set
		stream, ok := streams[labelKey]
		if !ok {
			stream = &LokiStream{
				Stream: labels,
				Values: [][]string{},
			}
			streams[labelKey] = stream
		}

		// Format timestamp as nanoseconds
		tsNano := fmt.Sprintf("%d", entry.timestamp.UnixNano())

		// Serialize entry to JSON for log line
		logLine, err := json.Marshal(entry.entry)
		if err != nil {
			atomic.AddInt64(&e.entriesFailed, 1)
			continue
		}

		stream.Values = append(stream.Values, []string{tsNano, string(logLine)})
	}

	// Build push request
	request := LokiPushRequest{
		Streams: make([]LokiStream, 0, len(streams)),
	}
	for _, stream := range streams {
		request.Streams = append(request.Streams, *stream)
	}

	// Send with retries
	var lastErr error
	entriesInBatch := len(entries)

	for attempt := 0; attempt <= e.config.RetryMax; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter
			delay := e.config.RetryWait * time.Duration(1<<(attempt-1))
			if delay > 10*time.Second {
				delay = 10 * time.Second
			}
			// Add 25% jitter
			jitter := time.Duration(float64(delay) * 0.25 * rand.Float64())
			time.Sleep(delay + jitter)
		}

		lastErr = e.doSend(request)
		if lastErr == nil {
			// Success
			atomic.AddInt64(&e.entriesSent, int64(entriesInBatch))
			atomic.AddInt64(&e.batchesSent, 1)
			return
		}
	}

	// All retries failed
	atomic.AddInt64(&e.entriesFailed, int64(entriesInBatch))
}

// doSend performs the HTTP POST to Loki
func (e *LokiExporter) doSend(payload LokiPushRequest) error {
	// Serialize to JSON
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	var body *bytes.Buffer
	var contentEncoding string

	if e.config.UseGzip {
		// Compress with gzip
		body = &bytes.Buffer{}
		gzipWriter := gzip.NewWriter(body)
		if _, err := gzipWriter.Write(data); err != nil {
			return fmt.Errorf("failed to compress payload: %w", err)
		}
		if err := gzipWriter.Close(); err != nil {
			return fmt.Errorf("failed to close gzip writer: %w", err)
		}
		contentEncoding = "gzip"
	} else {
		body = bytes.NewBuffer(data)
	}

	req, err := http.NewRequest("POST", e.config.URL, body)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	if e.config.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+e.config.AuthToken)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Loki returns 204 No Content on success
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	return fmt.Errorf("Loki returned status %d", resp.StatusCode)
}

// Stats returns the current statistics for the exporter
func (e *LokiExporter) Stats() LokiExporterStats {
	return LokiExporterStats{
		EntriesSent:    atomic.LoadInt64(&e.entriesSent),
		EntriesFailed:  atomic.LoadInt64(&e.entriesFailed),
		EntriesDropped: atomic.LoadInt64(&e.entriesDropped),
		BatchesSent:    atomic.LoadInt64(&e.batchesSent),
	}
}

// Close gracefully shuts down the exporter, draining the channel and flushing
// remaining entries. Returns an error if the shutdown times out.
func (e *LokiExporter) Close() error {
	var timeoutErr error

	e.closeOnce.Do(func() {
		close(e.closeChan)

		select {
		case <-e.closedChan:
			// Clean shutdown
		case <-time.After(e.config.ShutdownTimeout):
			timeoutErr = fmt.Errorf("shutdown timeout: %v", e.config.ShutdownTimeout)
		}
	})

	return timeoutErr
}

// forceClose immediately closes the exporter without waiting for flush
func (e *LokiExporter) forceClose() {
	e.closeOnce.Do(func() {
		close(e.closeChan)
	})
}

// classifyErrorType categorizes HTTP error responses into low-cardinality error types.
// Returns empty string for success responses (2xx).
func classifyErrorType(statusCode int, responseBody string) string {
	if statusCode >= 200 && statusCode < 300 {
		return ""
	}
	if statusCode == 429 {
		return "rate_limit"
	}
	if statusCode >= 500 {
		return "server_error"
	}
	if statusCode == 400 {
		// Check if context length error by looking for common patterns
		bodyLower := strings.ToLower(responseBody)
		if strings.Contains(bodyLower, "context") || strings.Contains(bodyLower, "too long") {
			return "context_length"
		}
		return "invalid_request"
	}
	// Other 4xx errors
	if statusCode >= 400 && statusCode < 500 {
		return "invalid_request"
	}
	return ""
}

// emitEvent is the internal method for pushing structured events to Loki.
// It constructs the entry with proper labels and JSON body, then queues it.
func (e *LokiExporter) emitEvent(sessionID, provider, machine, logType string, labels map[string]string, body map[string]interface{}) {
	timestamp := time.Now()

	// Build base labels
	baseLabels := map[string]string{
		"app":         "llm-proxy",
		"provider":    provider,
		"environment": e.config.Environment,
		"machine":     machine,
		"log_type":    logType,
	}

	// Merge additional labels
	for k, v := range labels {
		if v != "" {
			baseLabels[k] = v
		}
	}

	// Add session_id to body (high cardinality, not label)
	body["session_id"] = sessionID

	// Create entry for channel
	entry := lokiEntry{
		entry:     body,
		provider:  provider,
		timestamp: timestamp,
		logType:   logType,
		machine:   machine,
	}

	// Copy additional labels to lokiEntry fields
	if sr, ok := labels["stop_reason"]; ok {
		entry.stopReason = sr
	}
	if tn, ok := labels["tool_name"]; ok {
		entry.toolName = tn
	}
	if ir, ok := labels["is_retry"]; ok {
		entry.isRetry = ir
	}
	if et, ok := labels["error_type"]; ok {
		entry.errorType = et
	}

	// Build a complete entry map with type for JSON serialization
	body["type"] = logType

	// Non-blocking send
	select {
	case e.entryChan <- entry:
		// Entry queued
	default:
		atomic.AddInt64(&e.entriesDropped, 1)
	}
}

// EmitTurnStart emits a turn_start event when a request is received.
func (e *LokiExporter) EmitTurnStart(sessionID, provider, machine string, turnDepth int, errorRecovered bool) {
	labels := map[string]string{}

	body := map[string]interface{}{
		"turn_depth":      turnDepth,
		"error_recovered": errorRecovered,
	}

	e.emitEvent(sessionID, provider, machine, LogTypeTurnStart, labels, body)
}

// EmitTurnEnd emits a turn_end event when a response is complete.
func (e *LokiExporter) EmitTurnEnd(sessionID, provider, machine, stopReason string, isRetry bool, errorType string, patterns PatternData, tokens TokenData) {
	isRetryStr := "false"
	if isRetry {
		isRetryStr = "true"
	}

	labels := map[string]string{
		"stop_reason": stopReason,
		"is_retry":    isRetryStr,
	}
	if errorType != "" {
		labels["error_type"] = errorType
	}

	body := map[string]interface{}{
		"turn_depth":                  patterns.TurnDepth,
		"tool_streak":                 patterns.ToolStreak,
		"retry_count":                 patterns.RetryCount,
		"session_tool_count":          patterns.SessionToolCount,
		"input_tokens":                tokens.InputTokens,
		"output_tokens":               tokens.OutputTokens,
		"cache_read_input_tokens":     tokens.CacheReadInputTokens,
		"cache_creation_input_tokens": tokens.CacheCreationInputTokens,
	}

	e.emitEvent(sessionID, provider, machine, LogTypeTurnEnd, labels, body)
}

// EmitToolCall emits a tool_call event for each tool_use in a response.
func (e *LokiExporter) EmitToolCall(sessionID, provider, machine, toolName string, toolIndex int, toolUseID string) {
	labels := map[string]string{
		"tool_name": toolName,
	}

	body := map[string]interface{}{
		"tool_index":  toolIndex,
		"tool_use_id": toolUseID,
	}

	e.emitEvent(sessionID, provider, machine, LogTypeToolCall, labels, body)
}

// EmitToolResult emits a tool_result event for each tool_result in a request.
func (e *LokiExporter) EmitToolResult(sessionID, provider, machine, toolName, toolUseID string, isError bool) {
	labels := map[string]string{
		"tool_name": toolName,
	}

	body := map[string]interface{}{
		"tool_use_id": toolUseID,
		"is_error":    isError,
	}

	e.emitEvent(sessionID, provider, machine, LogTypeToolResult, labels, body)
}

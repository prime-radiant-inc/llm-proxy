// loki_exporter.go
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// LokiExporterConfig holds configuration for the Loki exporter
type LokiExporterConfig struct {
	URL             string        // Full push endpoint URL
	AuthToken       string        // Bearer token for auth (optional)
	BatchSize       int           // Number of entries per batch
	BatchWait       time.Duration // Duration to wait before flushing batch
	RetryMax        int           // Maximum retry attempts
	RetryWait       time.Duration // Base delay between retries
	UseGzip         bool          // Enable gzip compression
	Environment     string        // Environment label
	BufferSize      int           // Channel buffer size
	ShutdownTimeout time.Duration // Timeout for graceful shutdown
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

	le := lokiEntry{
		entry:     entry,
		provider:  provider,
		timestamp: timestamp,
		logType:   logType,
		machine:   machine,
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

		// Create label key for grouping
		labelKey := fmt.Sprintf("%s|%s|%s|%s|%s",
			labels["app"],
			labels["provider"],
			labels["environment"],
			labels["machine"],
			labels["log_type"],
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

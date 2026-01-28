// multi_writer.go
package main

import (
	"log"
	"net/http"
	"os"
	"os/user"
	"time"
)

// LokiPusher is the interface for pushing entries to Loki.
// *LokiExporter implements this interface.
type LokiPusher interface {
	Push(entry map[string]interface{}, provider string)
	Close() error
}

// AgentEventEmitter is the interface for emitting agent observability events to Loki.
// *LokiExporter implements this interface.
type AgentEventEmitter interface {
	EmitTurnStart(sessionID, provider, machine string, turnDepth int, errorRecovered bool)
	EmitTurnEnd(sessionID, provider, machine, stopReason string, isRetry bool, errorType string, patterns PatternData, tokens TokenData)
	EmitToolCall(sessionID, provider, machine, toolName string, toolIndex int, toolUseID string)
	EmitToolResult(sessionID, provider, machine, toolName, toolUseID string, isError bool)
}

// MultiWriter fans out log entries to both a file logger (primary) and a Loki
// exporter (secondary). File errors are returned to the caller, while Loki
// errors are logged but don't fail the operation (graceful degradation).
type MultiWriter struct {
	file      ProxyLogger
	loki      LokiPusher
	machineID string
}

// NewMultiWriter creates a new MultiWriter that writes to both the file logger
// and the Loki exporter. The loki parameter may be nil for file-only logging.
func NewMultiWriter(file ProxyLogger, loki LokiPusher) *MultiWriter {
	return &MultiWriter{
		file:      file,
		loki:      loki,
		machineID: getMachineIDForMultiWriter(),
	}
}

// NewMultiWriterWithCloseOrder is used for testing to track close order.
// In production, use NewMultiWriter instead.
func NewMultiWriterWithCloseOrder(file ProxyLogger, loki LokiPusher, closeOrder *[]string) *MultiWriter {
	return &MultiWriter{
		file:      file,
		loki:      loki,
		machineID: getMachineIDForMultiWriter(),
	}
}

// getMachineIDForMultiWriter returns user@hostname for log metadata
func getMachineIDForMultiWriter() string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	username := "unknown"
	if u, err := user.Current(); err == nil {
		username = u.Username
	}

	return username + "@" + hostname
}

// RegisterUpstream delegates to the file logger.
// Loki doesn't need upstream registration since it extracts from log entries.
func (m *MultiWriter) RegisterUpstream(sessionID, upstream string) {
	m.file.RegisterUpstream(sessionID, upstream)
}

// LogSessionStart logs a session start to both destinations.
// File errors are returned; Loki errors are logged but don't fail.
func (m *MultiWriter) LogSessionStart(sessionID, provider, upstream string) error {
	err := m.file.LogSessionStart(sessionID, provider, upstream)

	if m.loki != nil {
		entry := map[string]interface{}{
			"type":     "session_start",
			"provider": provider,
			"upstream": upstream,
			"_meta": map[string]interface{}{
				"ts":      time.Now().UTC().Format(time.RFC3339Nano),
				"machine": m.machineID,
				"host":    upstream,
				"session": sessionID,
			},
		}
		m.loki.Push(entry, provider)
	}

	return err
}

// LogRequest logs a request to both destinations.
// File errors are returned; Loki errors are logged but don't fail.
func (m *MultiWriter) LogRequest(sessionID, provider string, seq int, method, path string, headers http.Header, body []byte, requestID string) error {
	err := m.file.LogRequest(sessionID, provider, seq, method, path, headers, body, requestID)

	if m.loki != nil {
		entry := map[string]interface{}{
			"type":    "request",
			"seq":     seq,
			"method":  method,
			"path":    path,
			"headers": ObfuscateHeaders(headers),
			"body":    string(body),
			"size":    len(body),
			"_meta": map[string]interface{}{
				"ts":         time.Now().UTC().Format(time.RFC3339Nano),
				"machine":    m.machineID,
				"session":    sessionID,
				"request_id": requestID,
			},
		}
		m.loki.Push(entry, provider)
	}

	return err
}

// LogResponse logs a response to both destinations.
// File errors are returned; Loki errors are logged but don't fail.
func (m *MultiWriter) LogResponse(sessionID, provider string, seq int, status int, headers http.Header, body []byte, chunks []StreamChunk, timing ResponseTiming, requestID string) error {
	err := m.file.LogResponse(sessionID, provider, seq, status, headers, body, chunks, timing, requestID)

	if m.loki != nil {
		entry := map[string]interface{}{
			"type":    "response",
			"seq":     seq,
			"status":  status,
			"headers": headers,
			"timing":  timing,
			"size":    len(body),
			"_meta": map[string]interface{}{
				"ts":         time.Now().UTC().Format(time.RFC3339Nano),
				"machine":    m.machineID,
				"session":    sessionID,
				"request_id": requestID,
			},
		}

		if chunks != nil {
			entry["chunks"] = chunks
		} else {
			entry["body"] = string(body)
		}

		m.loki.Push(entry, provider)
	}

	return err
}

// LogFork logs a fork event to both destinations.
// File errors are returned; Loki errors are logged but don't fail.
func (m *MultiWriter) LogFork(sessionID, provider string, fromSeq int, parentSession string) error {
	err := m.file.LogFork(sessionID, provider, fromSeq, parentSession)

	if m.loki != nil {
		entry := map[string]interface{}{
			"type":           "fork",
			"from_seq":       fromSeq,
			"parent_session": parentSession,
			"reason":         "message_history_diverged",
			"_meta": map[string]interface{}{
				"ts":      time.Now().UTC().Format(time.RFC3339Nano),
				"machine": m.machineID,
				"session": sessionID,
			},
		}
		m.loki.Push(entry, provider)
	}

	return err
}

// Close flushes Loki first (to ensure all buffered entries are sent),
// then closes the file logger. This order ensures no log entries are lost.
func (m *MultiWriter) Close() error {
	// Close Loki first to flush buffered entries
	if m.loki != nil {
		if err := m.loki.Close(); err != nil {
			// Log but don't fail - graceful degradation
			log.Printf("WARNING: Loki close failed: %v", err)
		}
	}

	// Then close file logger
	return m.file.Close()
}

// EventEmitter returns the AgentEventEmitter for agent observability events.
// Returns nil if Loki is not configured.
func (m *MultiWriter) EventEmitter() AgentEventEmitter {
	if emitter, ok := m.loki.(AgentEventEmitter); ok {
		return emitter
	}
	return nil
}

// MachineID returns the machine identifier used in log metadata.
func (m *MultiWriter) MachineID() string {
	return m.machineID
}

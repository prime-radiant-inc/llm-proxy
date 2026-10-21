// multi_writer.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"os/user"
	"strings"
	"sync"
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

	// includeBody controls whether LogRequest/LogResponse put the raw request/
	// response body (and, for streaming responses, the raw chunk text) into
	// the entry map pushed to Loki. Default false (zero value): PRI-3040/
	// PRI-2489 is that Loki gets metadata only. This NEVER affects what goes
	// to m.file - the file logger is always called with the full, unmodified
	// body/chunks, via a separate map/args built independently below, never
	// derived from (or shared with) the Loki-bound entry. Set via server.go
	// from cfg.Loki.IncludeBody, same-package direct field assignment.
	includeBody bool

	// requestContexts stores per-request metadata keyed by requestID.
	// Set by serveBedrock before logging; consumed by LogRequest/LogResponse.
	requestContexts sync.Map

	// upstreams mirrors the file logger's session→upstream registration so the
	// Loki-side entries can stamp the same capture facts.
	upstreams sync.Map
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

// SetBedrockContext stores Bedrock metadata for a request, so LogRequest
// and LogResponse can add transport and model_override to _meta.
func (m *MultiWriter) SetBedrockContext(requestID, modelID string) {
	m.SetRequestLogContext(requestID, RequestLogContext{
		Transport:     "bedrock",
		ModelOverride: modelID,
	})
}

// ClearBedrockContext removes Bedrock metadata for a completed request.
func (m *MultiWriter) ClearBedrockContext(requestID string) {
	m.ClearRequestLogContext(requestID)
}

func (m *MultiWriter) SetRequestLogContext(requestID string, ctx RequestLogContext) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	m.requestContexts.Store(requestID, ctx)
	if file, ok := m.file.(requestLogContextLogger); ok {
		file.SetRequestLogContext(requestID, ctx)
	}
}

func (m *MultiWriter) ClearRequestLogContext(requestID string) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	m.requestContexts.Delete(requestID)
	if file, ok := m.file.(requestLogContextLogger); ok {
		file.ClearRequestLogContext(requestID)
	}
}

func (m *MultiWriter) addRequestLogContextMeta(meta map[string]interface{}, requestID string) {
	if ctx, ok := m.requestContexts.Load(strings.TrimSpace(requestID)); ok {
		if requestCtx, ok := ctx.(RequestLogContext); ok {
			addRequestLogContextMeta(meta, requestCtx)
		}
	}
}

// addBedrockMeta adds transport and model_override to _meta based on path.
// Used by LogRequest which has the request path available.
func addBedrockMeta(meta map[string]interface{}, path string) {
	if !strings.HasPrefix(path, "/model/") {
		return
	}
	meta["transport"] = "bedrock"
	modelID, err := extractModelID(path)
	if err == nil && modelID != "" {
		meta["model_override"] = modelID
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

// RegisterUpstream records the upstream for capture-fact stamping on the
// Loki side and delegates to the file logger.
func (m *MultiWriter) RegisterUpstream(sessionID, upstream string) {
	m.upstreams.Store(sessionID, upstream)
	m.file.RegisterUpstream(sessionID, upstream)
}

// LogSessionStart logs a session start to both destinations.
// File errors are returned; Loki errors are logged but don't fail.
func (m *MultiWriter) LogSessionStart(sessionID, provider, upstream string) error {
	m.upstreams.Store(sessionID, upstream)

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
		// Compute SHA256 of raw request body for deterministic replay verification
		bodyHash := sha256.Sum256(body)
		bodySHA := hex.EncodeToString(bodyHash[:])

		meta := map[string]interface{}{
			"ts":         time.Now().UTC().Format(time.RFC3339Nano),
			"machine":    m.machineID,
			"session":    sessionID,
			"request_id": requestID,
		}
		addBedrockMeta(meta, path)
		m.addRequestLogContextMeta(meta, requestID)

		// entry is built fresh here, independent of (and never shared with) the
		// call to m.file.LogRequest above, which already ran against the raw
		// body argument. Nothing below can leak into or strip from the file log.
		entry := map[string]interface{}{
			"type":        "request",
			"seq":         seq,
			"method":      method,
			"path":        path,
			"headers":     ObfuscateHeaders(headers),
			"size":        len(body),
			"request_sha": bodySHA,
			"_meta":       meta,
		}

		// PRI-3040/PRI-2489: Loki gets metadata only by default. body_omitted
		// is set so dashboards/queries can see when a body was withheld, rather
		// than silently having less data than expected.
		if m.includeBody {
			entry["body"] = string(body)
		} else {
			entry["body_omitted"] = true
		}

		if up, ok := m.upstreams.Load(sessionID); ok {
			upstream, _ := up.(string)
			entry["upstream"] = upstream
			entry["capture_version"] = CaptureVersion
			if mp := meteringProviderFromUpstream(upstream); mp != "" {
				entry["metering_provider"] = mp
			}
		}

		m.loki.Push(entry, provider)
	}

	return err
}

// LogResponse logs a response to both destinations.
// File errors are returned; Loki errors are logged but don't fail.
func (m *MultiWriter) LogResponse(sessionID, provider string, seq int, status int, headers http.Header, body []byte, chunks []StreamChunk, timing ResponseTiming, requestID string, capture ResponseCapture) error {
	err := m.file.LogResponse(sessionID, provider, seq, status, headers, body, chunks, timing, requestID, capture)

	if m.loki != nil {
		meta := map[string]interface{}{
			"ts":         time.Now().UTC().Format(time.RFC3339Nano),
			"machine":    m.machineID,
			"session":    sessionID,
			"request_id": requestID,
		}
		m.addRequestLogContextMeta(meta, requestID)

		// entry is built fresh here, independent of (and never shared with) the
		// call to m.file.LogResponse above, which already ran against the raw
		// body/chunks arguments. Nothing below can leak into or strip from the
		// file log.
		entry := map[string]interface{}{
			"type":    "response",
			"seq":     seq,
			"status":  status,
			"headers": headers,
			"timing":  timing,
			"size":    len(body),
			"_meta":   meta,
		}

		// PRI-3040/PRI-2489: Loki gets metadata only by default. body_omitted
		// is set so dashboards/queries can see when a body/chunks were withheld,
		// rather than silently having less data than expected.
		if m.includeBody {
			if chunks != nil {
				entry["chunks"] = chunks
			} else {
				entry["body"] = string(body)
			}
		} else {
			entry["body_omitted"] = true
		}

		if up, ok := m.upstreams.Load(sessionID); ok {
			upstream, _ := up.(string)
			entry["upstream"] = upstream
			entry["capture_version"] = CaptureVersion
			if mp := meteringProviderFromUpstream(upstream); mp != "" {
				entry["metering_provider"] = mp
			}
		}
		if capture.Path != "" {
			entry["path"] = capture.Path
		}
		if capture.Termination != "" {
			entry["termination"] = capture.Termination
		}
		if capture.TerminationError != "" {
			entry["termination_error"] = capture.TerminationError
		}

		m.loki.Push(entry, provider)
	}

	return err
}

// LogObservation logs a free-form observation entry - used by the Bedrock/
// Mantle passthrough path (mantle.go's writeMantleObservation) for request,
// response and error records - to both destinations.
//
// Unlike LogRequest/LogResponse, the caller hands this method ONE entry map
// that is used for both destinations, so (PRI-3040/PRI-2489: Loki gets
// metadata only) we cannot strip body/chunks from it in place - that would
// corrupt the file log too, since m.file.LogObservation above got the exact
// same map reference. stripObservationBody therefore returns a deep-enough
// COPY (top level, plus fresh copies of the nested "request"/"response"
// maps mantle.go nests body/chunks under) with body/chunks replaced by
// body_omitted: true, leaving the original entry - and what the file logger
// already wrote from it - untouched.
func (m *MultiWriter) LogObservation(sessionID, provider string, entry map[string]any) error {
	err := m.file.LogObservation(sessionID, provider, entry)

	if m.loki != nil {
		lokiEntry := entry
		if !m.includeBody {
			lokiEntry = stripObservationBody(entry)
		}
		m.loki.Push(lokiEntry, provider)
	}

	return err
}

// stripBodyFieldsCopy returns a shallow copy of in with any "body"/"chunks"
// keys removed and, if either was present, "body_omitted": true added. The
// input map is never mutated.
func stripBodyFieldsCopy(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	stripped := false
	if _, ok := out["body"]; ok {
		delete(out, "body")
		stripped = true
	}
	if _, ok := out["chunks"]; ok {
		delete(out, "chunks")
		stripped = true
	}
	if stripped {
		out["body_omitted"] = true
	}
	return out
}

// stripObservationBody returns a copy of a mantle-shaped observation entry
// with body/chunks text removed - at the top level (defensive; no current
// mantle.go call site puts them there) and inside the nested "request"/
// "response" maps (mantle.go's logMantleRequestObservation puts
// request["body"]; logMantleResponseObservation and
// logMantleAbortedResponseObservation put response["body"]/
// response["chunks"]). Every map touched is copied, never mutated in place,
// so the original entry (and the file logger's copy of it) is unaffected.
func stripObservationBody(entry map[string]any) map[string]any {
	out := stripBodyFieldsCopy(entry)

	for _, key := range []string{"request", "response"} {
		if nested, ok := out[key].(map[string]any); ok {
			out[key] = stripBodyFieldsCopy(nested)
		}
	}

	return out
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

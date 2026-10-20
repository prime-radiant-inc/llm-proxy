// logger.go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ResponseTiming struct {
	TTFBMs  int64 `json:"ttfb_ms"`
	TotalMs int64 `json:"total_ms"`
}

type StreamChunk struct {
	Timestamp time.Time `json:"ts"`
	DeltaMs   int64     `json:"delta_ms"`
	Raw       string    `json:"raw"`
}

type RequestLogContext struct {
	Correlation   ModelCallCorrelation
	Transport     string
	ModelOverride string
	RunID         string
	ResolvedRunID string
	LegacyRunID   string
	ClientFPHash  string
	Project       string
	ProviderRoute string
	WireAPI       string
}

type requestLogContextLogger interface {
	SetRequestLogContext(requestID string, ctx RequestLogContext)
	ClearRequestLogContext(requestID string)
}

type Logger struct {
	baseDir   string
	machineID string // user@hostname for log aggregation
	mu        sync.Mutex
	files     map[string]*os.File
	upstreams map[string]string // sessionID -> upstream
	contexts  sync.Map          // requestID -> RequestLogContext

	// idleTimeout and sweepInterval govern the background sweep started by
	// Start (see CloseIdleFiles). Defaults are set in NewLogger; tests in
	// this package shrink them to exercise the goroutine without a real
	// wait. now defaults to time.Now and is likewise only ever overridden
	// by tests, to fake elapsed idle time and date rollover without
	// touching the system clock.
	idleTimeout   time.Duration
	sweepInterval time.Duration
	now           func() time.Time

	started bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

func getMachineID() string {
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

// defaultSessionIdleTimeout is how long a session's log file can go
// without a write before CloseIdleFiles closes it. 15 minutes comfortably
// outlives a normal pause between turns in an interactive coding session
// (seconds to low single-digit minutes) while still reclaiming the
// descriptor - and, for a file the pruner (PRI-3457) has since unlinked,
// the disk space it held - well within an operator's debugging window
// instead of only at the next process restart.
const defaultSessionIdleTimeout = 15 * time.Minute

// defaultSessionSweepInterval is how often Start's background goroutine
// calls CloseIdleFiles. A third of defaultSessionIdleTimeout bounds how
// long a file can overstay its idle threshold before the next sweep
// catches it, without walking the open-files map often enough to show up
// as real overhead on a busy proxy.
const defaultSessionSweepInterval = 5 * time.Minute

func NewLogger(baseDir string) (*Logger, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	return &Logger{
		baseDir:       baseDir,
		machineID:     getMachineID(),
		files:         make(map[string]*os.File),
		upstreams:     make(map[string]string),
		idleTimeout:   defaultSessionIdleTimeout,
		sweepInterval: defaultSessionSweepInterval,
		now:           time.Now,
	}, nil
}

// Start launches the background goroutine that closes idle or
// date-rolled-over session files (see CloseIdleFiles) on sweepInterval.
// NewServer calls Start once, right after NewLogger, so the sweep is
// running before the proxy serves its first request. A second call is a
// no-op (idempotent).
func (l *Logger) Start() {
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return
	}
	l.started = true
	l.stopCh = make(chan struct{})
	l.doneCh = make(chan struct{})
	stopCh, doneCh := l.stopCh, l.doneCh
	interval := l.sweepInterval
	l.mu.Unlock()

	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				l.CloseIdleFiles()
			}
		}
	}()
}

// Stop signals the background sweep goroutine to exit and waits for it to
// finish. Idempotent; safe to call even if Start was never called. Close
// calls Stop itself, so callers that only ever use Close (every existing
// caller except NewServer) see no behavior change.
func (l *Logger) Stop() {
	l.mu.Lock()
	if !l.started {
		l.mu.Unlock()
		return
	}
	stopCh, doneCh := l.stopCh, l.doneCh
	l.started = false
	l.mu.Unlock()

	close(stopCh)
	<-doneCh
}

// CloseIdleFiles closes every open session log file that hasn't been
// written to in idleTimeout, or whose date directory no longer matches
// today (the session kept writing across midnight and rolled into a new
// <date> directory that was never opened under the old handle - see
// getFile's own rollover check for the write-path side of this). Closing
// here is safe even if the session writes again afterward: getFile
// reopens the file with O_APPEND, so no data is lost or overwritten - a
// session that resumes after a quiet spell just pays for one extra open().
//
// This is what actually frees disk space for a file the pruner (PRI-3457)
// has deleted: unlinking a directory entry does not reclaim the blocks of
// a file any process - including this one - still holds open. Returns the
// number of files closed, for tests.
func (l *Logger) CloseIdleFiles() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.files == nil {
		return 0
	}

	now := l.now()
	today := now.Format("2006-01-02")
	closed := 0
	for sessionID, f := range l.files {
		rolled := filepath.Base(filepath.Dir(f.Name())) != today

		idle := true
		if info, err := f.Stat(); err == nil {
			idle = now.Sub(info.ModTime()) >= l.idleTimeout
		}

		if !rolled && !idle {
			continue
		}

		f.Close()
		delete(l.files, sessionID)
		closed++
	}
	return closed
}

func (l *Logger) Close() error {
	l.Stop()

	l.mu.Lock()
	defer l.mu.Unlock()

	for _, f := range l.files {
		f.Close()
	}
	l.files = nil
	l.upstreams = nil
	return nil
}

// getFileLocked returns the open file for sessionID, opening (or
// reopening, across a date rollover) it as needed. Callers must hold
// l.mu; it is deliberately not a self-locking method. writeEntry and
// CloseIdleFiles both need "pick/open the file" and "use or close it" to
// happen as one atomic step under the same lock - splitting them (as a
// self-locking getFile once did, dropping the lock between choosing f and
// writing to it) left a window where the idle sweep could close f out from
// under an in-flight write ("file already closed"), caught by -race plus a
// concurrent-writers-and-sweep stress test (TestLogger_ConcurrentWritesAndSweep).
func (l *Logger) getFileLocked(sessionID string) (*os.File, error) {
	if l.files == nil {
		return nil, fmt.Errorf("logger is closed")
	}

	dateStr := l.now().Format("2006-01-02")

	if f, ok := l.files[sessionID]; ok {
		if filepath.Base(filepath.Dir(f.Name())) == dateStr {
			return f, nil
		}
		// The date rolled over while this session's file was still open.
		// Close it - it's done, and holding it open would both leak the fd
		// and keep writing into yesterday's file - and fall through to open
		// today's.
		f.Close()
		delete(l.files, sessionID)
	}

	// Look up the upstream for this session
	upstream, ok := l.upstreams[sessionID]
	if !ok {
		return nil, fmt.Errorf("no upstream registered for session %s", sessionID)
	}

	// Create directory: <baseDir>/<upstream>/<YYYY-MM-DD>/
	logDir := filepath.Join(l.baseDir, upstream, dateStr)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, err
	}

	// Open file for append
	path := filepath.Join(logDir, sessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	l.files[sessionID] = f
	return f, nil
}

func (l *Logger) writeEntry(sessionID string, entry interface{}) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := l.getFileLocked(sessionID)
	if err != nil {
		return err
	}

	_, err = f.Write(append(data, '\n'))
	return err
}

// RegisterUpstream registers an upstream host for a session.
// This is used when a forked session needs to write without a session_start.
func (l *Logger) RegisterUpstream(sessionID, upstream string) {
	l.mu.Lock()
	if l.upstreams != nil {
		l.upstreams[sessionID] = upstream
	}
	l.mu.Unlock()
}

func (l *Logger) SetRequestLogContext(requestID string, ctx RequestLogContext) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	l.contexts.Store(requestID, ctx)
}

func (l *Logger) ClearRequestLogContext(requestID string) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	l.contexts.Delete(requestID)
}

func (l *Logger) addRequestLogContext(meta map[string]interface{}, requestID string) {
	if ctx, ok := l.contexts.Load(strings.TrimSpace(requestID)); ok {
		if requestCtx, ok := ctx.(RequestLogContext); ok {
			addRequestLogContextMeta(meta, requestCtx)
		}
	}
}

func addRequestLogContextMeta(meta map[string]interface{}, ctx RequestLogContext) {
	addMetaString(meta, "lace_session_id", ctx.Correlation.SessionID)
	addMetaString(meta, "lace_turn_id", ctx.Correlation.TurnID)
	addMetaString(meta, "model_call_id", ctx.Correlation.ModelCallID)
	addMetaString(meta, "transport", ctx.Transport)
	addMetaString(meta, "model_override", ctx.ModelOverride)
	addMetaString(meta, "run_id", ctx.RunID)
	addMetaString(meta, "resolved_run_id", ctx.ResolvedRunID)
	addMetaString(meta, "cloud_build_run_id", ctx.LegacyRunID)
	addMetaString(meta, "client_fp_hash", ctx.ClientFPHash)
	addMetaString(meta, "project", ctx.Project)
	addMetaString(meta, "provider_route", ctx.ProviderRoute)
	addMetaString(meta, "wire_api", ctx.WireAPI)
}

func addMetaString(meta map[string]interface{}, key, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		meta[key] = value
	}
}

func (l *Logger) LogSessionStart(sessionID, provider, upstream string) error {
	// Register the upstream for this session
	l.RegisterUpstream(sessionID, upstream)

	entry := map[string]interface{}{
		"type":     "session_start",
		"provider": provider,
		"upstream": upstream,
		"_meta": map[string]interface{}{
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"machine": l.machineID,
			"host":    upstream,
			"session": sessionID,
		},
	}
	return l.writeEntry(sessionID, entry)
}

func (l *Logger) LogRequest(sessionID, provider string, seq int, method, path string, headers http.Header, body []byte, requestID string) error {
	upstream := l.upstreams[sessionID]
	meta := map[string]interface{}{
		"ts":         time.Now().UTC().Format(time.RFC3339Nano),
		"machine":    l.machineID,
		"host":       upstream,
		"session":    sessionID,
		"request_id": requestID,
	}
	l.addRequestLogContext(meta, requestID)

	entry := map[string]interface{}{
		"type":    "request",
		"seq":     seq,
		"method":  method,
		"path":    path,
		"headers": ObfuscateHeaders(headers),
		"body":    string(body),
		"size":    len(body),
		"_meta":   meta,
	}
	entry["upstream"] = upstream
	entry["capture_version"] = CaptureVersion
	if mp := meteringProviderFromUpstream(upstream); mp != "" {
		entry["metering_provider"] = mp
	}
	return l.writeEntry(sessionID, entry)
}

func (l *Logger) LogResponse(sessionID, provider string, seq int, status int, headers http.Header, body []byte, chunks []StreamChunk, timing ResponseTiming, requestID string, capture ResponseCapture) error {
	upstream := l.upstreams[sessionID]
	meta := map[string]interface{}{
		"ts":         time.Now().UTC().Format(time.RFC3339Nano),
		"machine":    l.machineID,
		"host":       upstream,
		"session":    sessionID,
		"request_id": requestID,
	}
	l.addRequestLogContext(meta, requestID)

	entry := map[string]interface{}{
		"type":    "response",
		"seq":     seq,
		"status":  status,
		"headers": headers,
		"timing":  timing,
		"size":    len(body),
		"_meta":   meta,
	}

	if chunks != nil {
		entry["chunks"] = chunks
	} else {
		entry["body"] = string(body)
	}

	entry["upstream"] = upstream
	entry["capture_version"] = CaptureVersion
	if mp := meteringProviderFromUpstream(upstream); mp != "" {
		entry["metering_provider"] = mp
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

	return l.writeEntry(sessionID, entry)
}

func (l *Logger) LogObservation(sessionID, provider string, entry map[string]any) error {
	return l.writeEntry(sessionID, entry)
}

// LogFork records a fork event when conversation history diverges
func (l *Logger) LogFork(sessionID, provider string, fromSeq int, parentSession string) error {
	upstream := l.upstreams[sessionID]

	entry := map[string]interface{}{
		"type":           "fork",
		"from_seq":       fromSeq,
		"parent_session": parentSession,
		"reason":         "message_history_diverged",
		"_meta": map[string]interface{}{
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"machine": l.machineID,
			"host":    upstream,
			"session": sessionID,
		},
	}
	return l.writeEntry(sessionID, entry)
}

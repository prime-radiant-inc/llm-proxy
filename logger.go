// logger.go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
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

type Logger struct {
	baseDir   string
	machineID string // user@hostname for log aggregation
	mu        sync.Mutex
	files     map[string]*os.File
	upstreams map[string]string // sessionID -> upstream
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

func NewLogger(baseDir string) (*Logger, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	return &Logger{
		baseDir:   baseDir,
		machineID: getMachineID(),
		files:     make(map[string]*os.File),
		upstreams: make(map[string]string),
	}, nil
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, f := range l.files {
		f.Close()
	}
	l.files = nil
	l.upstreams = nil
	return nil
}

func (l *Logger) getFile(sessionID string) (*os.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.files == nil {
		return nil, fmt.Errorf("logger is closed")
	}

	if f, ok := l.files[sessionID]; ok {
		return f, nil
	}

	// Look up the upstream for this session
	upstream, ok := l.upstreams[sessionID]
	if !ok {
		return nil, fmt.Errorf("no upstream registered for session %s", sessionID)
	}

	// Create directory: <baseDir>/<upstream>/<YYYY-MM-DD>/
	dateStr := time.Now().Format("2006-01-02")
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
	f, err := l.getFile(sessionID)
	if err != nil {
		return err
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

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
			"machine":    l.machineID,
			"host":       upstream,
			"session":    sessionID,
			"request_id": requestID,
		},
	}
	return l.writeEntry(sessionID, entry)
}

func (l *Logger) LogResponse(sessionID, provider string, seq int, status int, headers http.Header, body []byte, chunks []StreamChunk, timing ResponseTiming, requestID string) error {
	upstream := l.upstreams[sessionID]

	entry := map[string]interface{}{
		"type":    "response",
		"seq":     seq,
		"status":  status,
		"headers": headers,
		"timing":  timing,
		"size":    len(body),
		"_meta": map[string]interface{}{
			"ts":         time.Now().UTC().Format(time.RFC3339Nano),
			"machine":    l.machineID,
			"host":       upstream,
			"session":    sessionID,
			"request_id": requestID,
		},
	}

	if chunks != nil {
		entry["chunks"] = chunks
	} else {
		entry["body"] = string(body)
	}

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

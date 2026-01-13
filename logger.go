// logger.go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
	baseDir string
	mu      sync.Mutex
	files   map[string]*os.File
}

func NewLogger(baseDir string) (*Logger, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	return &Logger{
		baseDir: baseDir,
		files:   make(map[string]*os.File),
	}, nil
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, f := range l.files {
		f.Close()
	}
	l.files = nil
	return nil
}

func (l *Logger) getFile(sessionID, provider string) (*os.File, error) {
	key := provider + "/" + sessionID

	l.mu.Lock()
	defer l.mu.Unlock()

	if f, ok := l.files[key]; ok {
		return f, nil
	}

	// Create provider directory
	providerDir := filepath.Join(l.baseDir, provider)
	if err := os.MkdirAll(providerDir, 0755); err != nil {
		return nil, err
	}

	// Open file for append
	path := filepath.Join(providerDir, sessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	l.files[key] = f
	return f, nil
}

func (l *Logger) writeEntry(sessionID, provider string, entry interface{}) error {
	f, err := l.getFile(sessionID, provider)
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

func (l *Logger) LogSessionStart(sessionID, provider, upstream string) error {
	entry := map[string]interface{}{
		"type":     "session_start",
		"ts":       time.Now().UTC().Format(time.RFC3339Nano),
		"provider": provider,
		"upstream": upstream,
	}
	return l.writeEntry(sessionID, provider, entry)
}

func (l *Logger) LogRequest(sessionID, provider string, seq int, method, path string, headers http.Header, body []byte) error {
	entry := map[string]interface{}{
		"type":    "request",
		"ts":      time.Now().UTC().Format(time.RFC3339Nano),
		"seq":     seq,
		"method":  method,
		"path":    path,
		"headers": ObfuscateHeaders(headers),
		"body":    string(body),
		"size":    len(body),
	}
	return l.writeEntry(sessionID, provider, entry)
}

func (l *Logger) LogResponse(sessionID, provider string, seq int, status int, headers http.Header, body []byte, chunks []StreamChunk, timing ResponseTiming) error {
	entry := map[string]interface{}{
		"type":    "response",
		"ts":      time.Now().UTC().Format(time.RFC3339Nano),
		"seq":     seq,
		"status":  status,
		"headers": headers,
		"timing":  timing,
		"size":    len(body),
	}

	if chunks != nil {
		entry["chunks"] = chunks
	} else {
		entry["body"] = string(body)
	}

	return l.writeEntry(sessionID, provider, entry)
}

// LogFork records a fork event when conversation history diverges
func (l *Logger) LogFork(sessionID, provider string, fromSeq int, parentSession string) error {
	entry := map[string]interface{}{
		"type":           "fork",
		"ts":             time.Now().UTC().Format(time.RFC3339Nano),
		"from_seq":       fromSeq,
		"parent_session": parentSession,
		"reason":         "message_history_diverged",
	}
	return l.writeEntry(sessionID, provider, entry)
}

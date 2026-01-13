// session.go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type SessionManager struct {
	baseDir string
	db      *SessionDB
	logger  *Logger // For logging fork events
	mu      sync.Mutex
}

func NewSessionManager(baseDir string, logger *Logger) (*SessionManager, error) {
	dbPath := filepath.Join(baseDir, "sessions.db")

	db, err := NewSessionDB(dbPath)
	if err != nil {
		return nil, err
	}

	return &SessionManager{
		baseDir: baseDir,
		db:      db,
		logger:  logger,
	}, nil
}

func (sm *SessionManager) Close() error {
	return sm.db.Close()
}

// GetOrCreateSession determines if this request continues an existing session,
// forks from an earlier point, or starts a new session.
// Returns: sessionID, sequence number, isNewSession, error
func (sm *SessionManager) GetOrCreateSession(body []byte, provider, upstream string, headers http.Header) (string, int, bool, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// First, check if the client provided a session ID (e.g., Claude Code via metadata.user_id)
	clientSessionID := ExtractClientSessionID(body, provider, headers)
	if clientSessionID != "" {
		return sm.getOrCreateByClientSessionID(clientSessionID, provider, upstream)
	}

	// Fall back to fingerprint-based session tracking
	return sm.getOrCreateByFingerprint(body, provider, upstream)
}

// getOrCreateByClientSessionID handles session tracking when the client provides a session ID
func (sm *SessionManager) getOrCreateByClientSessionID(clientSessionID, provider, upstream string) (string, int, bool, error) {
	// Check if we've seen this client session ID before
	existingSession, err := sm.db.FindByClientSessionID(clientSessionID)
	if err != nil {
		return "", 0, false, err
	}

	if existingSession != "" {
		// Continue existing session
		_, _, _, lastSeq, err := sm.db.GetSessionWithClientID(existingSession)
		if err != nil {
			return "", 0, false, err
		}
		return existingSession, lastSeq + 1, false, nil
	}

	// New client session - create our own session ID but track the client's ID
	return sm.createNewSessionWithClientID(clientSessionID, provider, upstream)
}

// getOrCreateByFingerprint handles fingerprint-based session tracking (fallback)
func (sm *SessionManager) getOrCreateByFingerprint(body []byte, provider, upstream string) (string, int, bool, error) {
	// Compute fingerprint of prior messages (conversation state before this turn)
	priorFP, err := ComputePriorFingerprint(body, provider)
	if err != nil {
		return "", 0, false, fmt.Errorf("failed to compute fingerprint: %w", err)
	}

	// First message in conversation (no prior state)
	if priorFP == "" {
		return sm.createNewSession(provider, upstream)
	}

	// Look up prior fingerprint
	existingSession, existingSeq, err := sm.db.FindByFingerprint(priorFP)
	if err != nil {
		return "", 0, false, err
	}

	// No match = new session
	if existingSession == "" {
		return sm.createNewSession(provider, upstream)
	}

	// Found a match - check if it's the latest state (continuation) or earlier (fork)
	latestFP, latestSeq, err := sm.db.GetLatestFingerprint(existingSession)
	if err != nil {
		return "", 0, false, err
	}

	if priorFP == latestFP {
		// Continuation - same session, next sequence
		return existingSession, latestSeq + 1, false, nil
	}

	// Fork - prior state matches but not the latest
	// Create new branch session, copying history up to fork point
	return sm.createForkSession(existingSession, existingSeq, provider, upstream)
}

func (sm *SessionManager) createNewSession(provider, upstream string) (string, int, bool, error) {
	sessionID := generateSessionID()
	filePath := filepath.Join(provider, sessionID+".jsonl")

	// Create provider directory
	providerDir := filepath.Join(sm.baseDir, provider)
	if err := os.MkdirAll(providerDir, 0755); err != nil {
		return "", 0, false, err
	}

	// Create session in DB
	if err := sm.db.CreateSession(sessionID, provider, upstream, filePath); err != nil {
		return "", 0, false, err
	}

	return sessionID, 1, true, nil
}

func (sm *SessionManager) createNewSessionWithClientID(clientSessionID, provider, upstream string) (string, int, bool, error) {
	sessionID := generateSessionID()
	filePath := filepath.Join(provider, sessionID+".jsonl")

	// Create provider directory
	providerDir := filepath.Join(sm.baseDir, provider)
	if err := os.MkdirAll(providerDir, 0755); err != nil {
		return "", 0, false, err
	}

	// Create session in DB with client session ID
	if err := sm.db.CreateSessionWithClientID(sessionID, clientSessionID, provider, upstream, filePath); err != nil {
		return "", 0, false, err
	}

	return sessionID, 1, true, nil
}

func (sm *SessionManager) createForkSession(parentSession string, forkSeq int, provider, upstream string) (string, int, bool, error) {
	// Generate new session ID for branch
	sessionID := generateSessionID()
	filePath := filepath.Join(provider, sessionID+".jsonl")

	// Get parent session info
	_, _, parentFile, err := sm.db.GetSession(parentSession)
	if err != nil {
		return "", 0, false, err
	}

	// Copy parent log file up to fork point
	if err := sm.copyLogToForkPoint(parentFile, filePath, forkSeq); err != nil {
		return "", 0, false, err
	}

	// Create branch session in DB
	if err := sm.db.CreateSession(sessionID, provider, upstream, filePath); err != nil {
		return "", 0, false, err
	}

	// Log the fork event
	if sm.logger != nil {
		sm.logger.LogFork(sessionID, provider, forkSeq, parentSession)
	}

	return sessionID, 1, true, nil
}

func (sm *SessionManager) copyLogToForkPoint(srcPath, dstPath string, forkSeq int) error {
	srcFullPath := filepath.Join(sm.baseDir, srcPath)
	dstFullPath := filepath.Join(sm.baseDir, dstPath)

	// Create destination directory
	if err := os.MkdirAll(filepath.Dir(dstFullPath), 0755); err != nil {
		return err
	}

	src, err := os.Open(srcFullPath)
	if err != nil {
		// Source doesn't exist yet, nothing to copy
		return nil
	}
	defer src.Close()

	dst, err := os.Create(dstFullPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	// Parse JSONL and copy entries up to fork point
	scanner := bufio.NewScanner(src)
	for scanner.Scan() {
		line := scanner.Bytes()

		var entry map[string]interface{}
		if err := json.Unmarshal(line, &entry); err != nil {
			// Can't parse line, skip it
			continue
		}

		// Always copy session_start entries
		if entry["type"] == "session_start" {
			dst.Write(line)
			dst.Write([]byte("\n"))
			continue
		}

		// For request/response entries, check the sequence number
		if seq, ok := entry["seq"].(float64); ok {
			if int(seq) > forkSeq {
				// Stop at fork point - don't copy entries past forkSeq
				break
			}
		}

		dst.Write(line)
		dst.Write([]byte("\n"))
	}

	return scanner.Err()
}

// RecordResponse records the fingerprint after a response, for continuation tracking
func (sm *SessionManager) RecordResponse(sessionID string, seq int, requestBody, responseBody []byte, provider string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Extract assistant's reply from API response
	assistantMsg, err := ExtractAssistantMessage(responseBody, provider)
	if err != nil {
		return fmt.Errorf("failed to extract assistant message: %w", err)
	}

	// Get original messages from request
	messages, err := ExtractMessages(requestBody, provider)
	if err != nil {
		return fmt.Errorf("failed to extract messages: %w", err)
	}

	// Build complete state: request messages + assistant reply
	fullState := append(messages, assistantMsg)

	// Fingerprint the full state
	stateJSON, err := json.Marshal(fullState)
	if err != nil {
		return err
	}
	fingerprint := FingerprintMessages(stateJSON)

	return sm.db.UpdateSessionFingerprint(sessionID, seq, fingerprint)
}

func generateSessionID() string {
	now := time.Now()
	return now.Format("20060102-150405") + "-" + randomHex(4)
}

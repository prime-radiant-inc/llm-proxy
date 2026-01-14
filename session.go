// session.go
package main

import (
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

// GetOrCreateSession determines if this request continues an existing session or starts a new one.
// Returns: sessionID, sequence number, isNewSession, error
func (sm *SessionManager) GetOrCreateSession(body []byte, provider, upstream string, headers http.Header, path string) (string, int, bool, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Check if the client provided a session ID (e.g., Claude Code via metadata.user_id)
	clientSessionID := ExtractClientSessionID(body, provider, headers, path)
	if clientSessionID != "" {
		return sm.getOrCreateByClientSessionID(clientSessionID, provider, upstream)
	}

	// No client session ID - create a new session for this request.
	// We intentionally don't use fingerprint-based fallback because it causes
	// incorrect session merging when different sessions have similar messages.
	return sm.createNewSession(provider, upstream)
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
		nextSeq := lastSeq + 1
		// Update the sequence number in the DB
		if err := sm.db.UpdateSessionSeq(existingSession, nextSeq); err != nil {
			return "", 0, false, err
		}
		return existingSession, nextSeq, false, nil
	}

	// New client session - create our own session ID but track the client's ID
	return sm.createNewSessionWithClientID(clientSessionID, provider, upstream)
}

func (sm *SessionManager) createNewSession(provider, upstream string) (string, int, bool, error) {
	sessionID := generateSessionID()
	// New path structure: <upstream>/<YYYY-MM-DD>/<sessionID>.jsonl
	dateStr := time.Now().Format("2006-01-02")
	filePath := filepath.Join(upstream, dateStr, sessionID+".jsonl")

	// Create directory for upstream/date
	logDir := filepath.Join(sm.baseDir, upstream, dateStr)
	if err := os.MkdirAll(logDir, 0755); err != nil {
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
	// New path structure: <upstream>/<YYYY-MM-DD>/<sessionID>.jsonl
	dateStr := time.Now().Format("2006-01-02")
	filePath := filepath.Join(upstream, dateStr, sessionID+".jsonl")

	// Create directory for upstream/date
	logDir := filepath.Join(sm.baseDir, upstream, dateStr)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return "", 0, false, err
	}

	// Create session in DB with client session ID
	if err := sm.db.CreateSessionWithClientID(sessionID, clientSessionID, provider, upstream, filePath); err != nil {
		return "", 0, false, err
	}

	return sessionID, 1, true, nil
}

func generateSessionID() string {
	now := time.Now()
	return now.Format("20060102-150405") + "-" + randomHex(4)
}

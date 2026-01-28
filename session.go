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

// LoadPatternState loads pattern tracking state for a session.
// Returns a new default PatternState if session doesn't exist.
func (sm *SessionManager) LoadPatternState(sessionID string) (*PatternState, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	state, err := sm.db.LoadPatternState(sessionID)
	if err != nil {
		return nil, err
	}
	if state == nil {
		// Return default state for new sessions
		return &PatternState{
			PendingToolIDs: make(map[string]string),
		}, nil
	}
	return state, nil
}

// UpdatePatternState persists pattern tracking state for a session.
func (sm *SessionManager) UpdatePatternState(sessionID string, state *PatternState) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.db.UpdatePatternState(sessionID, state)
}

// ComputePatterns updates pattern state based on response data.
// firstToolName is the first tool_use in the response (empty if no tools).
// Returns isRetry for use in turn_end event.
// Note: LastWasError is managed by the request processing flow (processToolResultsAndEmitEvents),
// not here. This function only reads it for retry detection.
func ComputePatterns(state *PatternState, firstToolName string) bool {
	var isRetry bool

	if firstToolName == "" {
		// No tools in response - reset streak but keep other state
		state.ToolStreak = 0
		state.RetryCount = 0
		return false
	}

	// Check if this is a retry: same first tool AND previous tool had error
	// LastWasError was set by processToolResultsAndEmitEvents based on tool_results in this request
	isRetry = (firstToolName == state.LastToolName) && state.LastWasError

	// Update streak logic
	if firstToolName == state.LastToolName {
		state.ToolStreak++
	} else {
		state.ToolStreak = 1
	}

	// Update retry count
	if isRetry {
		state.RetryCount++
	} else {
		state.RetryCount = 0
	}

	// Update last tool name
	state.LastToolName = firstToolName

	return isRetry
}

// ClearMatchedToolID removes a tool ID from pending_tool_ids and returns the tool name.
func (sm *SessionManager) ClearMatchedToolID(sessionID, toolUseID string) (string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.db.ClearMatchedToolID(sessionID, toolUseID)
}

// db.go
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// PatternState holds agent behavior tracking data for a session.
// Used for computing tool streaks, retries, and turn depth metrics.
type PatternState struct {
	TurnCount        int               // Number of request-response cycles in this session
	LastToolName     string            // First tool name from previous turn (for streak detection)
	ToolStreak       int               // Consecutive turns where first tool is the same
	RetryCount       int               // Consecutive retry attempts (same tool after error)
	SessionToolCount int               // Total tool calls in session so far
	LastWasError     bool              // Previous turn's tool resulted in error
	PendingToolIDs   map[string]string // tool_use_id -> tool_name for result matching
}

type SessionDB struct {
	db *sql.DB
}

func NewSessionDB(path string) (*SessionDB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Create tables
	schema := `
	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		upstream TEXT NOT NULL,
		created_at TEXT NOT NULL,
		last_activity TEXT NOT NULL,
		last_seq INTEGER NOT NULL DEFAULT 0,
		last_fingerprint TEXT NOT NULL DEFAULT '',
		file_path TEXT NOT NULL,
		client_session_id TEXT
	);

	CREATE TABLE IF NOT EXISTS fingerprints (
		fingerprint TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		FOREIGN KEY (session_id) REFERENCES sessions(id)
	);

	CREATE INDEX IF NOT EXISTS idx_fingerprints_session ON fingerprints(session_id);
	CREATE INDEX IF NOT EXISTS idx_sessions_provider ON sessions(provider);
	CREATE INDEX IF NOT EXISTS idx_sessions_client_id ON sessions(client_session_id);
	`

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}

	// Migrations: add columns if they don't exist (ignore "duplicate column" errors)
	migrations := []string{
		"ALTER TABLE sessions ADD COLUMN client_session_id TEXT",
		"ALTER TABLE sessions ADD COLUMN turn_count INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE sessions ADD COLUMN last_tool_name TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE sessions ADD COLUMN tool_streak INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE sessions ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE sessions ADD COLUMN session_tool_count INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE sessions ADD COLUMN last_was_error INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE sessions ADD COLUMN pending_tool_ids TEXT NOT NULL DEFAULT '{}'",
	}

	for _, migration := range migrations {
		db.Exec(migration) // Ignore errors - column may already exist
	}

	// Create index for client_session_id (may already exist from schema)
	db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_client_id ON sessions(client_session_id)")

	return &SessionDB{db: db}, nil
}

func (s *SessionDB) Close() error {
	return s.db.Close()
}

func (s *SessionDB) CreateSession(id, provider, upstream, filePath string) error {
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := s.db.Exec(`
		INSERT INTO sessions (id, provider, upstream, created_at, last_activity, file_path, last_seq)
		VALUES (?, ?, ?, ?, ?, ?, 1)
	`, id, provider, upstream, now, now, filePath)

	return err
}

// UpdateSessionSeq updates the session's last sequence number
func (s *SessionDB) UpdateSessionSeq(sessionID string, seq int) error {
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := s.db.Exec(`
		UPDATE sessions
		SET last_activity = ?, last_seq = ?
		WHERE id = ?
	`, now, seq, sessionID)
	return err
}

func (s *SessionDB) UpdateSessionFingerprint(sessionID string, seq int, fingerprint string) error {
	now := time.Now().UTC().Format(time.RFC3339)

	// Update session
	_, err := s.db.Exec(`
		UPDATE sessions
		SET last_activity = ?, last_seq = ?, last_fingerprint = ?
		WHERE id = ?
	`, now, seq, fingerprint, sessionID)
	if err != nil {
		return err
	}

	// Insert fingerprint mapping
	_, err = s.db.Exec(`
		INSERT OR REPLACE INTO fingerprints (fingerprint, session_id, seq)
		VALUES (?, ?, ?)
	`, fingerprint, sessionID, seq)

	return err
}

func (s *SessionDB) FindByFingerprint(fingerprint string) (sessionID string, seq int, err error) {
	row := s.db.QueryRow(`
		SELECT session_id, seq FROM fingerprints WHERE fingerprint = ?
	`, fingerprint)

	err = row.Scan(&sessionID, &seq)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	return sessionID, seq, err
}

func (s *SessionDB) GetLatestFingerprint(sessionID string) (fingerprint string, seq int, err error) {
	row := s.db.QueryRow(`
		SELECT last_fingerprint, last_seq FROM sessions WHERE id = ?
	`, sessionID)

	err = row.Scan(&fingerprint, &seq)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	return fingerprint, seq, err
}

func (s *SessionDB) GetSession(id string) (provider, upstream, filePath string, err error) {
	row := s.db.QueryRow(`
		SELECT provider, upstream, file_path FROM sessions WHERE id = ?
	`, id)

	err = row.Scan(&provider, &upstream, &filePath)
	return
}

// CreateSessionWithClientID creates a new session with a client-provided session ID
func (s *SessionDB) CreateSessionWithClientID(id, clientSessionID, provider, upstream, filePath string) error {
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := s.db.Exec(`
		INSERT INTO sessions (id, client_session_id, provider, upstream, created_at, last_activity, file_path, last_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1)
	`, id, clientSessionID, provider, upstream, now, now, filePath)

	return err
}

// FindByClientSessionID finds a session by its client-provided session ID
func (s *SessionDB) FindByClientSessionID(clientSessionID string) (sessionID string, err error) {
	row := s.db.QueryRow(`
		SELECT id FROM sessions WHERE client_session_id = ?
	`, clientSessionID)

	err = row.Scan(&sessionID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sessionID, err
}

// GetSessionWithClientID gets a session including its client session ID and last sequence
func (s *SessionDB) GetSessionWithClientID(id string) (provider, upstream, filePath string, lastSeq int, err error) {
	row := s.db.QueryRow(`
		SELECT provider, upstream, file_path, last_seq FROM sessions WHERE id = ?
	`, id)

	err = row.Scan(&provider, &upstream, &filePath, &lastSeq)
	return
}

// LoadPatternState loads pattern tracking state for a session.
// Returns nil with no error if session doesn't exist.
func (s *SessionDB) LoadPatternState(sessionID string) (*PatternState, error) {
	row := s.db.QueryRow(`
		SELECT turn_count, last_tool_name, tool_streak, retry_count,
		       session_tool_count, last_was_error, pending_tool_ids
		FROM sessions WHERE id = ?
	`, sessionID)

	var turnCount, toolStreak, retryCount, sessionToolCount int
	var lastToolName, pendingToolIDsJSON string
	var lastWasError int

	err := row.Scan(&turnCount, &lastToolName, &toolStreak, &retryCount,
		&sessionToolCount, &lastWasError, &pendingToolIDsJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	pendingToolIDs := make(map[string]string)
	if pendingToolIDsJSON != "" && pendingToolIDsJSON != "{}" {
		if err := json.Unmarshal([]byte(pendingToolIDsJSON), &pendingToolIDs); err != nil {
			return nil, fmt.Errorf("failed to unmarshal pending_tool_ids: %w", err)
		}
	}

	return &PatternState{
		TurnCount:        turnCount,
		LastToolName:     lastToolName,
		ToolStreak:       toolStreak,
		RetryCount:       retryCount,
		SessionToolCount: sessionToolCount,
		LastWasError:     lastWasError != 0,
		PendingToolIDs:   pendingToolIDs,
	}, nil
}

// UpdatePatternState persists pattern tracking state for a session.
func (s *SessionDB) UpdatePatternState(sessionID string, state *PatternState) error {
	pendingToolIDsJSON, err := json.Marshal(state.PendingToolIDs)
	if err != nil {
		return fmt.Errorf("failed to marshal pending_tool_ids: %w", err)
	}

	lastWasError := 0
	if state.LastWasError {
		lastWasError = 1
	}

	_, err = s.db.Exec(`
		UPDATE sessions
		SET turn_count = ?, last_tool_name = ?, tool_streak = ?, retry_count = ?,
		    session_tool_count = ?, last_was_error = ?, pending_tool_ids = ?
		WHERE id = ?
	`, state.TurnCount, state.LastToolName, state.ToolStreak, state.RetryCount,
		state.SessionToolCount, lastWasError, string(pendingToolIDsJSON), sessionID)

	return err
}

// ClearMatchedToolID removes a tool ID from pending_tool_ids and returns the tool name.
// Returns empty string if the tool ID was not found.
func (s *SessionDB) ClearMatchedToolID(sessionID, toolUseID string) (string, error) {
	state, err := s.LoadPatternState(sessionID)
	if err != nil {
		return "", err
	}
	if state == nil {
		return "", nil
	}

	toolName, exists := state.PendingToolIDs[toolUseID]
	if !exists {
		return "", nil
	}

	delete(state.PendingToolIDs, toolUseID)

	if err := s.UpdatePatternState(sessionID, state); err != nil {
		return "", err
	}

	return toolName, nil
}

// db.go
package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

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

	// Migration: add client_session_id column if it doesn't exist
	migration := `
	ALTER TABLE sessions ADD COLUMN client_session_id TEXT;
	`

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}

	// Run migration (ignore error if column already exists)
	db.Exec(migration)

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

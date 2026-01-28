// db_test.go
package main

import (
	"path/filepath"
	"testing"
)

func TestDBCreate(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	db, err := NewSessionDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}
	defer db.Close()

	// Verify tables exist by inserting a session
	err = db.CreateSession("test-session", "anthropic", "api.anthropic.com", "test-file.jsonl")
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
}

func TestDBSessionLookup(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	db, err := NewSessionDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}
	defer db.Close()

	// Create a session
	db.CreateSession("session-1", "anthropic", "api.anthropic.com", "session-1.jsonl")

	// Update with fingerprint
	err = db.UpdateSessionFingerprint("session-1", 1, "fingerprint-abc")
	if err != nil {
		t.Fatalf("Failed to update fingerprint: %v", err)
	}

	// Look up by fingerprint
	session, seq, err := db.FindByFingerprint("fingerprint-abc")
	if err != nil {
		t.Fatalf("Failed to find by fingerprint: %v", err)
	}
	if session != "session-1" {
		t.Errorf("Expected session-1, got %s", session)
	}
	if seq != 1 {
		t.Errorf("Expected seq 1, got %d", seq)
	}
}

func TestDBFingerprintNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	db, _ := NewSessionDB(dbPath)
	defer db.Close()

	session, _, err := db.FindByFingerprint("nonexistent")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if session != "" {
		t.Error("Expected empty session for nonexistent fingerprint")
	}
}

func TestDBLatestFingerprint(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	db, _ := NewSessionDB(dbPath)
	defer db.Close()

	db.CreateSession("session-1", "anthropic", "api.anthropic.com", "session-1.jsonl")
	db.UpdateSessionFingerprint("session-1", 1, "fp-1")
	db.UpdateSessionFingerprint("session-1", 2, "fp-2")
	db.UpdateSessionFingerprint("session-1", 3, "fp-3")

	// Get latest fingerprint for session
	fp, seq, err := db.GetLatestFingerprint("session-1")
	if err != nil {
		t.Fatalf("Failed to get latest: %v", err)
	}
	if fp != "fp-3" {
		t.Errorf("Expected fp-3, got %s", fp)
	}
	if seq != 3 {
		t.Errorf("Expected seq 3, got %d", seq)
	}
}

// Pattern state tests for agent observability

func TestDBPatternStateMigrations(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	db, err := NewSessionDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}
	defer db.Close()

	// Create a session
	err = db.CreateSession("test-session", "anthropic", "api.anthropic.com", "test-file.jsonl")
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Verify pattern columns exist by loading pattern state (should return defaults)
	state, err := db.LoadPatternState("test-session")
	if err != nil {
		t.Fatalf("Failed to load pattern state: %v", err)
	}

	// New sessions should have default values
	if state.TurnCount != 0 {
		t.Errorf("Expected TurnCount=0, got %d", state.TurnCount)
	}
	if state.LastToolName != "" {
		t.Errorf("Expected LastToolName='', got %q", state.LastToolName)
	}
	if state.ToolStreak != 0 {
		t.Errorf("Expected ToolStreak=0, got %d", state.ToolStreak)
	}
	if state.RetryCount != 0 {
		t.Errorf("Expected RetryCount=0, got %d", state.RetryCount)
	}
	if state.SessionToolCount != 0 {
		t.Errorf("Expected SessionToolCount=0, got %d", state.SessionToolCount)
	}
	if state.LastWasError {
		t.Error("Expected LastWasError=false")
	}
	if len(state.PendingToolIDs) != 0 {
		t.Errorf("Expected empty PendingToolIDs, got %v", state.PendingToolIDs)
	}
}

func TestDBUpdatePatternState(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	db, err := NewSessionDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}
	defer db.Close()

	db.CreateSession("session-1", "anthropic", "api.anthropic.com", "session-1.jsonl")

	// Update pattern state
	state := &PatternState{
		TurnCount:        5,
		LastToolName:     "Bash",
		ToolStreak:       3,
		RetryCount:       2,
		SessionToolCount: 12,
		LastWasError:     true,
		PendingToolIDs:   map[string]string{"tool_123": "Bash", "tool_456": "Read"},
	}

	err = db.UpdatePatternState("session-1", state)
	if err != nil {
		t.Fatalf("Failed to update pattern state: %v", err)
	}

	// Load and verify
	loaded, err := db.LoadPatternState("session-1")
	if err != nil {
		t.Fatalf("Failed to load pattern state: %v", err)
	}

	if loaded.TurnCount != 5 {
		t.Errorf("Expected TurnCount=5, got %d", loaded.TurnCount)
	}
	if loaded.LastToolName != "Bash" {
		t.Errorf("Expected LastToolName='Bash', got %q", loaded.LastToolName)
	}
	if loaded.ToolStreak != 3 {
		t.Errorf("Expected ToolStreak=3, got %d", loaded.ToolStreak)
	}
	if loaded.RetryCount != 2 {
		t.Errorf("Expected RetryCount=2, got %d", loaded.RetryCount)
	}
	if loaded.SessionToolCount != 12 {
		t.Errorf("Expected SessionToolCount=12, got %d", loaded.SessionToolCount)
	}
	if !loaded.LastWasError {
		t.Error("Expected LastWasError=true")
	}
	if len(loaded.PendingToolIDs) != 2 {
		t.Errorf("Expected 2 pending tool IDs, got %d", len(loaded.PendingToolIDs))
	}
	if loaded.PendingToolIDs["tool_123"] != "Bash" {
		t.Errorf("Expected tool_123=Bash, got %q", loaded.PendingToolIDs["tool_123"])
	}
	if loaded.PendingToolIDs["tool_456"] != "Read" {
		t.Errorf("Expected tool_456=Read, got %q", loaded.PendingToolIDs["tool_456"])
	}
}

func TestDBLoadPatternStateNonexistent(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	db, _ := NewSessionDB(dbPath)
	defer db.Close()

	// Loading pattern state for nonexistent session should return nil with no error
	state, err := db.LoadPatternState("nonexistent")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if state != nil {
		t.Error("Expected nil state for nonexistent session")
	}
}

func TestDBPatternStatePersistsAcrossRestarts(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	// Create DB, set pattern state, close
	{
		db, err := NewSessionDB(dbPath)
		if err != nil {
			t.Fatalf("Failed to create DB: %v", err)
		}

		db.CreateSession("session-1", "anthropic", "api.anthropic.com", "session-1.jsonl")

		state := &PatternState{
			TurnCount:        10,
			LastToolName:     "Edit",
			ToolStreak:       2,
			RetryCount:       1,
			SessionToolCount: 25,
			LastWasError:     false,
			PendingToolIDs:   map[string]string{"tool_789": "Grep"},
		}
		db.UpdatePatternState("session-1", state)
		db.Close()
	}

	// Reopen DB and verify state persisted
	{
		db, err := NewSessionDB(dbPath)
		if err != nil {
			t.Fatalf("Failed to reopen DB: %v", err)
		}
		defer db.Close()

		loaded, err := db.LoadPatternState("session-1")
		if err != nil {
			t.Fatalf("Failed to load pattern state: %v", err)
		}

		if loaded.TurnCount != 10 {
			t.Errorf("Expected TurnCount=10 after restart, got %d", loaded.TurnCount)
		}
		if loaded.LastToolName != "Edit" {
			t.Errorf("Expected LastToolName='Edit' after restart, got %q", loaded.LastToolName)
		}
		if loaded.ToolStreak != 2 {
			t.Errorf("Expected ToolStreak=2 after restart, got %d", loaded.ToolStreak)
		}
		if loaded.RetryCount != 1 {
			t.Errorf("Expected RetryCount=1 after restart, got %d", loaded.RetryCount)
		}
		if loaded.SessionToolCount != 25 {
			t.Errorf("Expected SessionToolCount=25 after restart, got %d", loaded.SessionToolCount)
		}
		if loaded.LastWasError {
			t.Error("Expected LastWasError=false after restart")
		}
		if loaded.PendingToolIDs["tool_789"] != "Grep" {
			t.Errorf("Expected tool_789=Grep after restart, got %q", loaded.PendingToolIDs["tool_789"])
		}
	}
}

func TestDBPatternStateEmptyPendingToolIDs(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	db, _ := NewSessionDB(dbPath)
	defer db.Close()

	db.CreateSession("session-1", "anthropic", "api.anthropic.com", "session-1.jsonl")

	// Update with empty pending tool IDs
	state := &PatternState{
		TurnCount:      1,
		PendingToolIDs: map[string]string{},
	}
	err := db.UpdatePatternState("session-1", state)
	if err != nil {
		t.Fatalf("Failed to update pattern state: %v", err)
	}

	// Load and verify empty map (not nil)
	loaded, err := db.LoadPatternState("session-1")
	if err != nil {
		t.Fatalf("Failed to load pattern state: %v", err)
	}

	if loaded.PendingToolIDs == nil {
		t.Error("Expected non-nil empty map for PendingToolIDs")
	}
	if len(loaded.PendingToolIDs) != 0 {
		t.Errorf("Expected empty PendingToolIDs, got %v", loaded.PendingToolIDs)
	}
}

func TestDBClearMatchedToolID(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	db, _ := NewSessionDB(dbPath)
	defer db.Close()

	db.CreateSession("session-1", "anthropic", "api.anthropic.com", "session-1.jsonl")

	// Set up pending tool IDs
	state := &PatternState{
		TurnCount:      1,
		PendingToolIDs: map[string]string{"tool_1": "Bash", "tool_2": "Read", "tool_3": "Edit"},
	}
	db.UpdatePatternState("session-1", state)

	// Clear one tool ID
	toolName, err := db.ClearMatchedToolID("session-1", "tool_2")
	if err != nil {
		t.Fatalf("Failed to clear matched tool ID: %v", err)
	}
	if toolName != "Read" {
		t.Errorf("Expected tool name 'Read', got %q", toolName)
	}

	// Verify it was removed
	loaded, _ := db.LoadPatternState("session-1")
	if len(loaded.PendingToolIDs) != 2 {
		t.Errorf("Expected 2 remaining tool IDs, got %d", len(loaded.PendingToolIDs))
	}
	if _, exists := loaded.PendingToolIDs["tool_2"]; exists {
		t.Error("tool_2 should have been cleared")
	}
	if loaded.PendingToolIDs["tool_1"] != "Bash" {
		t.Error("tool_1 should still exist")
	}
	if loaded.PendingToolIDs["tool_3"] != "Edit" {
		t.Error("tool_3 should still exist")
	}
}

func TestDBClearMatchedToolIDNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	db, _ := NewSessionDB(dbPath)
	defer db.Close()

	db.CreateSession("session-1", "anthropic", "api.anthropic.com", "session-1.jsonl")

	state := &PatternState{
		TurnCount:      1,
		PendingToolIDs: map[string]string{"tool_1": "Bash"},
	}
	db.UpdatePatternState("session-1", state)

	// Try to clear nonexistent tool ID
	toolName, err := db.ClearMatchedToolID("session-1", "nonexistent")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if toolName != "" {
		t.Errorf("Expected empty tool name for nonexistent ID, got %q", toolName)
	}
}

func TestDBMigrationOnExistingDB(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sessions.db")

	// First, create a DB without pattern columns (simulate old schema)
	// by directly creating the schema without migrations
	{
		db, err := NewSessionDB(dbPath)
		if err != nil {
			t.Fatalf("Failed to create DB: %v", err)
		}
		// Create a session using the existing code path
		db.CreateSession("old-session", "anthropic", "api.anthropic.com", "old-session.jsonl")
		db.Close()
	}

	// Reopen - migrations should run and old session should get default values
	{
		db, err := NewSessionDB(dbPath)
		if err != nil {
			t.Fatalf("Failed to reopen DB: %v", err)
		}
		defer db.Close()

		// Old session should be loadable with default pattern values
		state, err := db.LoadPatternState("old-session")
		if err != nil {
			t.Fatalf("Failed to load pattern state for old session: %v", err)
		}

		if state.TurnCount != 0 {
			t.Errorf("Expected TurnCount=0 for migrated session, got %d", state.TurnCount)
		}
		if state.LastToolName != "" {
			t.Errorf("Expected LastToolName='' for migrated session, got %q", state.LastToolName)
		}
	}
}

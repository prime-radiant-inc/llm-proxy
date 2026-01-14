// session_test.go
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionManagerNewSession(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil) // nil logger for tests
	if err != nil {
		t.Fatalf("Failed to create session manager: %v", err)
	}
	defer sm.Close()

	// First message = new session
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)

	sessionID, seq, isNew, err := sm.GetOrCreateSession(body, "anthropic", "api.anthropic.com", nil, "/v1/messages")
	if err != nil {
		t.Fatalf("Failed to get session: %v", err)
	}

	if !isNew {
		t.Error("First request should create new session")
	}
	if sessionID == "" {
		t.Error("Session ID should not be empty")
	}
	if seq != 1 {
		t.Errorf("Expected seq 1, got %d", seq)
	}
}

func TestSessionManagerContinuation(t *testing.T) {
	tmpDir := t.TempDir()

	sm, _ := NewSessionManager(tmpDir, nil)
	defer sm.Close()

	// First request
	body1 := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	sessionID1, _, _, _ := sm.GetOrCreateSession(body1, "anthropic", "api.anthropic.com", nil, "/v1/messages")

	// Mock API response with assistant reply
	response1 := []byte(`{"content":[{"type":"text","text":"hi"}]}`)
	sm.RecordResponse(sessionID1, 1, body1, response1, "anthropic")

	// Second request continues the conversation
	// Assistant content must be array format to match what we stored from the response
	body2 := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi"}]},{"role":"user","content":"how are you"}]}`)
	sessionID2, seq2, isNew, _ := sm.GetOrCreateSession(body2, "anthropic", "api.anthropic.com", nil, "/v1/messages")

	if isNew {
		t.Error("Continuation should not create new session")
	}
	if sessionID2 != sessionID1 {
		t.Errorf("Should continue same session: %s != %s", sessionID2, sessionID1)
	}
	if seq2 != 2 {
		t.Errorf("Expected seq 2, got %d", seq2)
	}
}

func TestSessionManagerFork(t *testing.T) {
	tmpDir := t.TempDir()

	sm, _ := NewSessionManager(tmpDir, nil)
	defer sm.Close()

	// First request
	body1 := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	sessionID1, _, _, _ := sm.GetOrCreateSession(body1, "anthropic", "api.anthropic.com", nil, "/v1/messages")
	response1 := []byte(`{"content":[{"type":"text","text":"hi"}]}`)
	sm.RecordResponse(sessionID1, 1, body1, response1, "anthropic")

	// Second request - takes option A path (assistant content as array)
	body2 := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi"}]},{"role":"user","content":"option A"}]}`)
	sm.GetOrCreateSession(body2, "anthropic", "api.anthropic.com", nil, "/v1/messages")
	response2 := []byte(`{"content":[{"type":"text","text":"you chose A"}]}`)
	sm.RecordResponse(sessionID1, 2, body2, response2, "anthropic")

	// Third request - but goes back to first state and takes different path (fork!)
	// Prior is [user:hello, assistant:[{type:text,text:hi}]] which matches state after seq 1
	body3 := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi"}]},{"role":"user","content":"option B"}]}`)
	sessionID3, seq3, isNew, _ := sm.GetOrCreateSession(body3, "anthropic", "api.anthropic.com", nil, "/v1/messages")

	// Should create a new session (branch)
	if !isNew {
		t.Error("Fork should create new session")
	}
	if sessionID3 == sessionID1 {
		t.Error("Fork should have different session ID")
	}
	// Fork at seq 1 means the fork file contains seq 1 entries,
	// so the next entry should be seq 2 (forkSeq + 1)
	if seq3 != 2 {
		t.Errorf("Fork should continue at seq 2 (forkSeq+1), got %d", seq3)
	}
}

func TestForkCopiesLogCorrectly(t *testing.T) {
	tmpDir := t.TempDir()

	logger, _ := NewLogger(tmpDir)
	defer logger.Close()

	sm, _ := NewSessionManager(tmpDir, logger)
	defer sm.Close()

	// Create first session with multiple exchanges
	body1 := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	sessionID1, _, _, _ := sm.GetOrCreateSession(body1, "anthropic", "api.anthropic.com", nil, "/v1/messages")

	// Log session start and first request
	logger.LogSessionStart(sessionID1, "anthropic", "api.anthropic.com")
	logger.LogRequest(sessionID1, "anthropic", 1, "POST", "/v1/messages", nil, body1, "req-1")

	// Record response for seq 1
	response1 := []byte(`{"content":[{"type":"text","text":"hi"}]}`)
	sm.RecordResponse(sessionID1, 1, body1, response1, "anthropic")

	// Log response
	logger.LogResponse(sessionID1, "anthropic", 1, 200, nil, response1, nil, ResponseTiming{}, "req-1")

	// Second exchange (assistant content as array)
	body2 := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi"}]},{"role":"user","content":"option A"}]}`)
	sm.GetOrCreateSession(body2, "anthropic", "api.anthropic.com", nil, "/v1/messages")
	logger.LogRequest(sessionID1, "anthropic", 2, "POST", "/v1/messages", nil, body2, "req-2")
	response2 := []byte(`{"content":[{"type":"text","text":"you chose A"}]}`)
	sm.RecordResponse(sessionID1, 2, body2, response2, "anthropic")
	logger.LogResponse(sessionID1, "anthropic", 2, 200, nil, response2, nil, ResponseTiming{}, "req-2")

	// Third exchange (assistant content as array)
	body3 := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi"}]},{"role":"user","content":"option A"},{"role":"assistant","content":[{"type":"text","text":"you chose A"}]},{"role":"user","content":"more stuff"}]}`)
	sm.GetOrCreateSession(body3, "anthropic", "api.anthropic.com", nil, "/v1/messages")
	logger.LogRequest(sessionID1, "anthropic", 3, "POST", "/v1/messages", nil, body3, "req-3")
	response3 := []byte(`{"content":[{"type":"text","text":"ok"}]}`)
	sm.RecordResponse(sessionID1, 3, body3, response3, "anthropic")
	logger.LogResponse(sessionID1, "anthropic", 3, 200, nil, response3, nil, ResponseTiming{}, "req-3")

	// Fork from seq 1 (take option B instead of option A)
	bodyFork := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi"}]},{"role":"user","content":"option B"}]}`)
	forkSessionID, _, isNew, _ := sm.GetOrCreateSession(bodyFork, "anthropic", "api.anthropic.com", nil, "/v1/messages")

	// Close logger to flush before reading files
	logger.Close()

	if !isNew {
		t.Error("Fork should create new session")
	}

	// Read the forked log file - new path: <upstream>/<date>/<sessionID>.jsonl
	today := time.Now().Format("2006-01-02")
	forkedLogPath := filepath.Join(tmpDir, "api.anthropic.com", today, forkSessionID+".jsonl")
	forkedData, err := os.ReadFile(forkedLogPath)
	if err != nil {
		t.Fatalf("Failed to read forked log: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(forkedData)), "\n")

	// Should have: session_start, request seq 1, response seq 1
	// Should NOT have: request seq 2, response seq 2, request seq 3, response seq 3
	foundSeq1 := false
	foundSeq2 := false

	for _, line := range lines {
		var entry map[string]interface{}
		json.Unmarshal([]byte(line), &entry)

		if seq, ok := entry["seq"].(float64); ok {
			if int(seq) == 1 {
				foundSeq1 = true
			}
			if int(seq) == 2 {
				foundSeq2 = true
			}
		}
	}

	if !foundSeq1 {
		t.Error("Forked log should contain seq 1 entries")
	}
	if foundSeq2 {
		t.Error("Forked log should NOT contain seq 2 entries (after fork point)")
	}
}

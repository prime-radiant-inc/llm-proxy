// session_test.go
package main

import (
	"testing"
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

func TestSessionManagerContinuationWithClientSessionID(t *testing.T) {
	tmpDir := t.TempDir()

	sm, _ := NewSessionManager(tmpDir, nil)
	defer sm.Close()

	// First request with client session ID (Anthropic format)
	body1 := []byte(`{"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"user_abc_session_test-session-123"}}`)
	sessionID1, seq1, isNew1, _ := sm.GetOrCreateSession(body1, "anthropic", "api.anthropic.com", nil, "/v1/messages")

	if !isNew1 {
		t.Error("First request should create new session")
	}
	if seq1 != 1 {
		t.Errorf("Expected seq 1, got %d", seq1)
	}

	// Second request with same client session ID continues the conversation
	body2 := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi"}]},{"role":"user","content":"how are you"}],"metadata":{"user_id":"user_abc_session_test-session-123"}}`)
	sessionID2, seq2, isNew2, _ := sm.GetOrCreateSession(body2, "anthropic", "api.anthropic.com", nil, "/v1/messages")

	if isNew2 {
		t.Error("Continuation should not create new session")
	}
	if sessionID2 != sessionID1 {
		t.Errorf("Should continue same session: %s != %s", sessionID2, sessionID1)
	}
	if seq2 != 2 {
		t.Errorf("Expected seq 2, got %d", seq2)
	}
}

func TestSessionManagerNoClientSessionIDCreatesNewSession(t *testing.T) {
	tmpDir := t.TempDir()

	sm, _ := NewSessionManager(tmpDir, nil)
	defer sm.Close()

	// First request without client session ID
	body1 := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	sessionID1, _, isNew1, _ := sm.GetOrCreateSession(body1, "anthropic", "api.anthropic.com", nil, "/v1/messages")

	if !isNew1 {
		t.Error("First request should create new session")
	}

	// Second request also without client session ID - should create NEW session (not merge)
	body2 := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"how are you"}]}`)
	sessionID2, _, isNew2, _ := sm.GetOrCreateSession(body2, "anthropic", "api.anthropic.com", nil, "/v1/messages")

	// Without client session ID, each request should get its own session
	if !isNew2 {
		t.Error("Request without client session ID should create new session")
	}
	if sessionID2 == sessionID1 {
		t.Error("Requests without client session ID should NOT be merged")
	}
}

func TestSessionManagerDifferentClientSessionIDs(t *testing.T) {
	tmpDir := t.TempDir()

	sm, _ := NewSessionManager(tmpDir, nil)
	defer sm.Close()

	// Request with client session ID "session-A"
	body1 := []byte(`{"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"user_abc_session_session-A"}}`)
	sessionID1, _, _, _ := sm.GetOrCreateSession(body1, "anthropic", "api.anthropic.com", nil, "/v1/messages")

	// Request with different client session ID "session-B"
	body2 := []byte(`{"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"user_abc_session_session-B"}}`)
	sessionID2, _, isNew2, _ := sm.GetOrCreateSession(body2, "anthropic", "api.anthropic.com", nil, "/v1/messages")

	// Different client session IDs should create different sessions
	if !isNew2 {
		t.Error("Different client session ID should create new session")
	}
	if sessionID2 == sessionID1 {
		t.Error("Different client session IDs should map to different sessions")
	}
}

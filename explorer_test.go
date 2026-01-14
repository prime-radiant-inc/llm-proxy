package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplorerServerHealth(t *testing.T) {
	tmpDir := t.TempDir()

	explorer := NewExplorer(tmpDir)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	explorer.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}
}

func TestExplorerListsSessions(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a fake session log
	sessionDir := filepath.Join(tmpDir, "api.anthropic.com", "2026-01-14")
	os.MkdirAll(sessionDir, 0755)
	os.WriteFile(
		filepath.Join(sessionDir, "test-session.jsonl"),
		[]byte(`{"type":"session_start","_meta":{"ts":"2026-01-14T10:00:00Z","host":"api.anthropic.com"}}`),
		0644,
	)

	explorer := NewExplorer(tmpDir)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	explorer.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "test-session") {
		t.Error("Expected session list to contain test-session")
	}
}

func TestSessionInfoIncludesMessageCount(t *testing.T) {
	tmpDir := t.TempDir()

	// Create session with multiple entries
	sessionDir := filepath.Join(tmpDir, "api.anthropic.com", "2026-01-14")
	os.MkdirAll(sessionDir, 0755)

	content := `{"type":"session_start","_meta":{"ts":"2026-01-14T10:00:00Z"}}
{"type":"request","seq":1,"_meta":{"ts":"2026-01-14T10:00:01Z"}}
{"type":"response","seq":1,"_meta":{"ts":"2026-01-14T10:00:02Z"}}
{"type":"request","seq":2,"_meta":{"ts":"2026-01-14T10:05:00Z"}}
{"type":"response","seq":2,"_meta":{"ts":"2026-01-14T10:05:30Z"}}
`
	os.WriteFile(filepath.Join(sessionDir, "test-session.jsonl"), []byte(content), 0644)

	explorer := NewExplorer(tmpDir)
	sessions := explorer.listSessions()

	if len(sessions) != 1 {
		t.Fatalf("Expected 1 session, got %d", len(sessions))
	}

	if sessions[0].MessageCount != 2 {
		t.Errorf("Expected 2 messages, got %d", sessions[0].MessageCount)
	}

	if sessions[0].TimeRange == "" {
		t.Error("Expected non-empty time range")
	}
}

func TestSessionDetailShowsEntries(t *testing.T) {
	tmpDir := t.TempDir()

	sessionDir := filepath.Join(tmpDir, "api.anthropic.com", "2026-01-14")
	os.MkdirAll(sessionDir, 0755)

	content := `{"type":"session_start","_meta":{"ts":"2026-01-14T10:00:00Z","host":"api.anthropic.com","session":"abc123"}}
{"type":"request","seq":1,"body":"{\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}]}","_meta":{"ts":"2026-01-14T10:00:01Z"}}
{"type":"response","seq":1,"body":"{\"content\":[{\"type\":\"text\",\"text\":\"Hi there!\"}]}","_meta":{"ts":"2026-01-14T10:00:02Z"}}
`
	os.WriteFile(filepath.Join(sessionDir, "abc123.jsonl"), []byte(content), 0644)

	explorer := NewExplorer(tmpDir)

	req := httptest.NewRequest("GET", "/session/abc123", nil)
	w := httptest.NewRecorder()

	explorer.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Hello") {
		t.Error("Expected session detail to show user message")
	}
	if !strings.Contains(body, "Hi there") {
		t.Error("Expected session detail to show assistant response")
	}
}

func TestGroupAndParseTurns(t *testing.T) {
	tmpDir := t.TempDir()
	explorer := NewExplorer(tmpDir)

	entries := []LogEntry{
		{
			Type: "request",
			Seq:  1,
			Body: `{"model":"claude-3","messages":[{"role":"user","content":"Hello"}]}`,
		},
		{
			Type: "response",
			Seq:  1,
			Body: `{"content":[{"type":"text","text":"Hi there!"}],"usage":{"input_tokens":10,"output_tokens":5}}`,
		},
	}

	turns := explorer.groupAndParseTurns(entries, "api.anthropic.com")

	if len(turns) != 1 {
		t.Fatalf("Expected 1 turn, got %d", len(turns))
	}

	turn := turns[0]
	if turn.Seq != 1 {
		t.Errorf("Expected Seq 1, got %d", turn.Seq)
	}
	if turn.Request == nil {
		t.Error("Expected Request to be set")
	}
	if turn.Response == nil {
		t.Error("Expected Response to be set")
	}
	if turn.ReqParsed.Model != "claude-3" {
		t.Errorf("Expected model claude-3, got %s", turn.ReqParsed.Model)
	}
	if len(turn.ReqParsed.Messages) != 1 {
		t.Errorf("Expected 1 message, got %d", len(turn.ReqParsed.Messages))
	}
	if turn.ReqParsed.Messages[0].TextContent != "Hello" {
		t.Errorf("Expected message 'Hello', got '%s'", turn.ReqParsed.Messages[0].TextContent)
	}
	if len(turn.RespParsed.Content) != 1 {
		t.Errorf("Expected 1 content block, got %d", len(turn.RespParsed.Content))
	}
	if turn.RespParsed.Content[0].Text != "Hi there!" {
		t.Errorf("Expected 'Hi there!', got '%s'", turn.RespParsed.Content[0].Text)
	}
	if turn.RespParsed.Usage.InputTokens != 10 {
		t.Errorf("Expected 10 input tokens, got %d", turn.RespParsed.Usage.InputTokens)
	}
}

func TestGroupAndParseTurnsWithThinking(t *testing.T) {
	tmpDir := t.TempDir()
	explorer := NewExplorer(tmpDir)

	entries := []LogEntry{
		{
			Type: "request",
			Seq:  1,
			Body: `{"model":"claude-3","messages":[{"role":"user","content":"Think about this"}]}`,
		},
		{
			Type: "response",
			Seq:  1,
			Body: `{"content":[{"type":"thinking","thinking":"Let me consider..."},{"type":"text","text":"Here's my answer"}]}`,
		},
	}

	turns := explorer.groupAndParseTurns(entries, "api.anthropic.com")

	if len(turns) != 1 {
		t.Fatalf("Expected 1 turn, got %d", len(turns))
	}

	if len(turns[0].RespParsed.Content) != 2 {
		t.Fatalf("Expected 2 content blocks, got %d", len(turns[0].RespParsed.Content))
	}

	thinking := turns[0].RespParsed.Content[0]
	if thinking.Type != "thinking" {
		t.Errorf("Expected thinking block, got %s", thinking.Type)
	}
	if thinking.Thinking != "Let me consider..." {
		t.Errorf("Expected 'Let me consider...', got '%s'", thinking.Thinking)
	}

	text := turns[0].RespParsed.Content[1]
	if text.Type != "text" {
		t.Errorf("Expected text block, got %s", text.Type)
	}
}

func TestGroupAndParseTurnsWithToolUse(t *testing.T) {
	tmpDir := t.TempDir()
	explorer := NewExplorer(tmpDir)

	entries := []LogEntry{
		{
			Type: "request",
			Seq:  1,
			Body: `{"model":"claude-3","messages":[{"role":"user","content":"Read a file"}]}`,
		},
		{
			Type: "response",
			Seq:  1,
			Body: `{"content":[{"type":"tool_use","id":"toolu_123","name":"Read","input":{"file_path":"/test.txt"}}]}`,
		},
		{
			Type: "request",
			Seq:  2,
			Body: `{"model":"claude-3","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"file contents"}]}]}`,
		},
		{
			Type: "response",
			Seq:  2,
			Body: `{"content":[{"type":"text","text":"The file contains..."}]}`,
		},
	}

	turns := explorer.groupAndParseTurns(entries, "api.anthropic.com")

	if len(turns) != 2 {
		t.Fatalf("Expected 2 turns, got %d", len(turns))
	}

	// First turn should have tool_use
	toolUse := turns[0].RespParsed.Content[0]
	if toolUse.Type != "tool_use" {
		t.Errorf("Expected tool_use, got %s", toolUse.Type)
	}
	if toolUse.ToolName != "Read" {
		t.Errorf("Expected tool name 'Read', got '%s'", toolUse.ToolName)
	}
	if toolUse.ToolID != "toolu_123" {
		t.Errorf("Expected tool ID 'toolu_123', got '%s'", toolUse.ToolID)
	}

	// Second turn should have tool_result in request
	if len(turns[1].ReqParsed.Messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(turns[1].ReqParsed.Messages))
	}
	msg := turns[1].ReqParsed.Messages[0]
	if len(msg.Content) != 1 {
		t.Fatalf("Expected 1 content block, got %d", len(msg.Content))
	}
	toolResult := msg.Content[0]
	if toolResult.Type != "tool_result" {
		t.Errorf("Expected tool_result, got %s", toolResult.Type)
	}
	if toolResult.ToolID != "toolu_123" {
		t.Errorf("Expected tool ID 'toolu_123', got '%s'", toolResult.ToolID)
	}
}

func TestSessionRendersParsedContent(t *testing.T) {
	tmpDir := t.TempDir()

	sessionDir := filepath.Join(tmpDir, "api.anthropic.com", "2026-01-14")
	os.MkdirAll(sessionDir, 0755)

	content := `{"type":"session_start","_meta":{"ts":"2026-01-14T10:00:00Z","host":"api.anthropic.com","session":"parsed123"}}
{"type":"request","seq":1,"body":"{\"model\":\"claude-3\",\"system\":\"You are helpful\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}]}","_meta":{"ts":"2026-01-14T10:00:01Z","host":"api.anthropic.com"}}
{"type":"response","seq":1,"body":"{\"content\":[{\"type\":\"thinking\",\"thinking\":\"Let me think...\"},{\"type\":\"text\",\"text\":\"Hi there!\"}],\"usage\":{\"input_tokens\":10,\"output_tokens\":20}}","_meta":{"ts":"2026-01-14T10:00:02Z"}}
`
	os.WriteFile(filepath.Join(sessionDir, "parsed123.jsonl"), []byte(content), 0644)

	explorer := NewExplorer(tmpDir)

	req := httptest.NewRequest("GET", "/session/parsed123", nil)
	w := httptest.NewRecorder()

	explorer.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	body := w.Body.String()

	// Check for host display
	if !strings.Contains(body, "api.anthropic.com") {
		t.Error("Expected host to be displayed")
	}

	// Check for system prompt
	if !strings.Contains(body, "You are helpful") {
		t.Error("Expected system prompt to be displayed")
	}

	// Check for thinking block
	if !strings.Contains(body, "Thinking") {
		t.Error("Expected thinking block summary")
	}
	if !strings.Contains(body, "Let me think...") {
		t.Error("Expected thinking content")
	}

	// Check for user message
	if !strings.Contains(body, "Hello") {
		t.Error("Expected user message")
	}

	// Check for assistant response
	if !strings.Contains(body, "Hi there!") {
		t.Error("Expected assistant response text")
	}

	// Check for token usage
	if !strings.Contains(body, "10") && !strings.Contains(body, "20") {
		t.Error("Expected token usage to be displayed")
	}
}

func TestSessionRendersToolCalls(t *testing.T) {
	tmpDir := t.TempDir()

	sessionDir := filepath.Join(tmpDir, "api.anthropic.com", "2026-01-14")
	os.MkdirAll(sessionDir, 0755)

	content := `{"type":"session_start","_meta":{"ts":"2026-01-14T10:00:00Z","host":"api.anthropic.com","session":"tools123"}}
{"type":"request","seq":1,"body":"{\"model\":\"claude-3\",\"messages\":[{\"role\":\"user\",\"content\":\"Read test.txt\"}]}","_meta":{"ts":"2026-01-14T10:00:01Z","host":"api.anthropic.com"}}
{"type":"response","seq":1,"body":"{\"content\":[{\"type\":\"tool_use\",\"id\":\"toolu_abc\",\"name\":\"Read\",\"input\":{\"file_path\":\"/test.txt\"}}]}","_meta":{"ts":"2026-01-14T10:00:02Z"}}
{"type":"request","seq":2,"body":"{\"model\":\"claude-3\",\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"toolu_abc\",\"content\":\"file contents here\"}]}]}","_meta":{"ts":"2026-01-14T10:00:03Z","host":"api.anthropic.com"}}
{"type":"response","seq":2,"body":"{\"content\":[{\"type\":\"text\",\"text\":\"The file says...\"}]}","_meta":{"ts":"2026-01-14T10:00:04Z"}}
`
	os.WriteFile(filepath.Join(sessionDir, "tools123.jsonl"), []byte(content), 0644)

	explorer := NewExplorer(tmpDir)

	req := httptest.NewRequest("GET", "/session/tools123", nil)
	w := httptest.NewRecorder()

	explorer.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	body := w.Body.String()

	// Check for tool use display
	if !strings.Contains(body, "Read") {
		t.Error("Expected tool name 'Read' to be displayed")
	}

	// Check for tool result display
	if !strings.Contains(body, "Tool Result") || !strings.Contains(body, "file contents here") {
		t.Error("Expected tool result to be displayed")
	}
}

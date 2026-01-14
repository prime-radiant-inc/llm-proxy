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

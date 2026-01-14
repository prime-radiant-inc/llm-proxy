// service_test.go
package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestDynamicPortBinding(t *testing.T) {
	// Bind to port 0, OS assigns available port
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("Failed to bind: %v", err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	if port == 0 {
		t.Error("Expected non-zero port")
	}
}

func TestWritePortfile(t *testing.T) {
	tmpDir := t.TempDir()
	portfile := filepath.Join(tmpDir, "port")

	err := WritePortfile(portfile, 52847)
	if err != nil {
		t.Fatalf("WritePortfile failed: %v", err)
	}

	data, _ := os.ReadFile(portfile)
	if string(data) != "52847" {
		t.Errorf("Expected '52847', got %q", data)
	}
}

func TestWritePortfileCreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	portfile := filepath.Join(tmpDir, "nested", "dir", "port")

	err := WritePortfile(portfile, 12345)
	if err != nil {
		t.Fatalf("WritePortfile failed: %v", err)
	}

	data, _ := os.ReadFile(portfile)
	if string(data) != "12345" {
		t.Errorf("Expected '12345', got %q", data)
	}
}

func TestReadPortfile(t *testing.T) {
	tmpDir := t.TempDir()
	portfile := filepath.Join(tmpDir, "port")

	// Write a port file manually
	err := os.WriteFile(portfile, []byte("54321"), 0644)
	if err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	port, err := ReadPortfile(portfile)
	if err != nil {
		t.Fatalf("ReadPortfile failed: %v", err)
	}
	if port != 54321 {
		t.Errorf("Expected 54321, got %d", port)
	}
}

func TestReadPortfileNonExistent(t *testing.T) {
	tmpDir := t.TempDir()
	portfile := filepath.Join(tmpDir, "nonexistent")

	_, err := ReadPortfile(portfile)
	if err == nil {
		t.Error("Expected error for non-existent file")
	}
}

func TestReadPortfileInvalidContent(t *testing.T) {
	tmpDir := t.TempDir()
	portfile := filepath.Join(tmpDir, "port")

	// Write invalid content
	err := os.WriteFile(portfile, []byte("not-a-number"), 0644)
	if err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	_, err = ReadPortfile(portfile)
	if err == nil {
		t.Error("Expected error for invalid port content")
	}
}

func TestDefaultPortfilePath(t *testing.T) {
	path := DefaultPortfilePath()

	// Should contain the expected path components
	if !filepath.IsAbs(path) {
		t.Errorf("Expected absolute path, got %q", path)
	}
	if filepath.Base(path) != "port" {
		t.Errorf("Expected file named 'port', got %q", filepath.Base(path))
	}
	// Should be in .local/state/llm-proxy
	if !contains(path, ".local") || !contains(path, "state") || !contains(path, "llm-proxy") {
		t.Errorf("Expected path containing .local/state/llm-proxy, got %q", path)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsMiddle(s, substr)))
}

func containsMiddle(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

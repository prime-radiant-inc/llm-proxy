// setup_test.go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchShellRC(t *testing.T) {
	tmpDir := t.TempDir()
	bashrc := filepath.Join(tmpDir, ".bashrc")

	// Create existing bashrc
	os.WriteFile(bashrc, []byte("# existing content\n"), 0644)

	err := PatchShellRC(bashrc)
	if err != nil {
		t.Fatalf("PatchShellRC failed: %v", err)
	}

	content, _ := os.ReadFile(bashrc)
	if !strings.Contains(string(content), `eval "$(llm-proxy --env)"`) {
		t.Error("Missing eval line")
	}
	if !strings.Contains(string(content), "# existing content") {
		t.Error("Clobbered existing content")
	}
	if !strings.Contains(string(content), "# LLM Proxy") {
		t.Error("Missing marker comment")
	}
}

func TestPatchShellRCIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	bashrc := filepath.Join(tmpDir, ".bashrc")

	os.WriteFile(bashrc, []byte("# existing\n"), 0644)

	PatchShellRC(bashrc)
	PatchShellRC(bashrc) // Second call

	content, _ := os.ReadFile(bashrc)
	count := strings.Count(string(content), `eval "$(llm-proxy --env)"`)
	if count != 1 {
		t.Errorf("Expected 1 eval line, got %d", count)
	}
}

func TestPatchShellRCCreatesFileIfMissing(t *testing.T) {
	tmpDir := t.TempDir()
	bashrc := filepath.Join(tmpDir, ".bashrc")

	// Don't create the file - let PatchShellRC create it
	err := PatchShellRC(bashrc)
	if err != nil {
		t.Fatalf("PatchShellRC failed: %v", err)
	}

	content, err := os.ReadFile(bashrc)
	if err != nil {
		t.Fatalf("File was not created: %v", err)
	}
	if !strings.Contains(string(content), `eval "$(llm-proxy --env)"`) {
		t.Error("Missing eval line in newly created file")
	}
}

func TestPatchAllShells(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	// Create both rc files
	bashrc := filepath.Join(tmpDir, ".bashrc")
	zshrc := filepath.Join(tmpDir, ".zshrc")
	os.WriteFile(bashrc, []byte("# bash\n"), 0644)
	os.WriteFile(zshrc, []byte("# zsh\n"), 0644)

	err := PatchAllShells()
	if err != nil {
		t.Fatalf("PatchAllShells failed: %v", err)
	}

	// Check bashrc was patched
	bashContent, _ := os.ReadFile(bashrc)
	if !strings.Contains(string(bashContent), `eval "$(llm-proxy --env)"`) {
		t.Error("bashrc not patched")
	}
	if !strings.Contains(string(bashContent), "# bash") {
		t.Error("bashrc original content clobbered")
	}

	// Check zshrc was patched
	zshContent, _ := os.ReadFile(zshrc)
	if !strings.Contains(string(zshContent), `eval "$(llm-proxy --env)"`) {
		t.Error("zshrc not patched")
	}
	if !strings.Contains(string(zshContent), "# zsh") {
		t.Error("zshrc original content clobbered")
	}
}

func TestPatchAllShellsOnlyPatchesExisting(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	// Only create bashrc, not zshrc
	bashrc := filepath.Join(tmpDir, ".bashrc")
	zshrc := filepath.Join(tmpDir, ".zshrc")
	os.WriteFile(bashrc, []byte("# bash\n"), 0644)

	err := PatchAllShells()
	if err != nil {
		t.Fatalf("PatchAllShells failed: %v", err)
	}

	// bashrc should be patched
	bashContent, _ := os.ReadFile(bashrc)
	if !strings.Contains(string(bashContent), `eval "$(llm-proxy --env)"`) {
		t.Error("bashrc not patched")
	}

	// zshrc should not exist
	if _, err := os.Stat(zshrc); err == nil {
		t.Error("zshrc was created but shouldn't have been")
	}
}

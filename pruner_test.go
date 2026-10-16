// pruner_test.go
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeDatedLogFile creates <baseDir>/<upstream>/<date>/<session>.jsonl with
// the given content and backdates its mtime by age. age == 0 leaves the
// natural (just-written) mtime alone.
func writeDatedLogFile(t *testing.T, baseDir, upstream, date, session, content string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(baseDir, upstream, date)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	path := filepath.Join(dir, session+".jsonl")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	if age > 0 {
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatalf("Chtimes(%s): %v", path, err)
		}
	}
	return path
}

func noActiveFiles() map[string]struct{} { return nil }

func TestPruneOnce_AgeRemovesOldFiles(t *testing.T) {
	dir := t.TempDir()

	oldPath := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-01", "old-session", `{"type":"session_start"}`, 20*24*time.Hour)
	newPath := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-20", "new-session", `{"type":"session_start"}`, 1*time.Hour)

	p := NewPruner(dir, 14, 0, time.Hour, noActiveFiles)
	stats := p.PruneOnce()

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("expected old file to be removed, stat err = %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("expected new file to survive, stat err = %v", err)
	}
	if stats.AgeFilesDeleted != 1 {
		t.Errorf("AgeFilesDeleted = %d, want 1", stats.AgeFilesDeleted)
	}
	if stats.AgeBytesDeleted != int64(len(`{"type":"session_start"}`)) {
		t.Errorf("AgeBytesDeleted = %d, want %d", stats.AgeBytesDeleted, len(`{"type":"session_start"}`))
	}
	if stats.SizeFilesDeleted != 0 {
		t.Errorf("SizeFilesDeleted = %d, want 0", stats.SizeFilesDeleted)
	}
}

func TestPruneOnce_AgeZeroDisablesAgePruning(t *testing.T) {
	dir := t.TempDir()
	oldPath := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-01", "old-session", "x", 365*24*time.Hour)

	p := NewPruner(dir, 0, 0, time.Hour, noActiveFiles)
	stats := p.PruneOnce()

	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("age pruning disabled (0) but file was removed: %v", err)
	}
	if stats.AgeFilesDeleted != 0 {
		t.Errorf("AgeFilesDeleted = %d, want 0", stats.AgeFilesDeleted)
	}
}

func TestPruneOnce_SizeCapRemovesOldestFirst(t *testing.T) {
	dir := t.TempDir()

	// Three files, each 100 bytes, all within the age window (retention
	// disabled) so only the size cap is in play. Oldest mtime first.
	content := make([]byte, 100)
	for i := range content {
		content[i] = 'x'
	}
	oldest := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-01", "s1", string(content), 3*time.Hour)
	middle := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-02", "s2", string(content), 2*time.Hour)
	newest := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-03", "s3", string(content), 1*time.Hour)

	// Total is 300 bytes. Cap at 250: deleting just the oldest (100 bytes)
	// brings the total to 200, which is <= 250 - exactly one deletion, and
	// it must be the oldest.
	p := NewPruner(dir, 0, 250, time.Hour, noActiveFiles)
	stats := p.PruneOnce()

	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("expected oldest file removed, stat err = %v", err)
	}
	if _, err := os.Stat(middle); err != nil {
		t.Errorf("expected middle file to survive, stat err = %v", err)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Errorf("expected newest file to survive, stat err = %v", err)
	}
	if stats.SizeFilesDeleted != 1 {
		t.Errorf("SizeFilesDeleted = %d, want 1", stats.SizeFilesDeleted)
	}
	if stats.SizeBytesDeleted != 100 {
		t.Errorf("SizeBytesDeleted = %d, want 100", stats.SizeBytesDeleted)
	}
}

func TestPruneOnce_SizeCapZeroDisablesSizePruning(t *testing.T) {
	dir := t.TempDir()
	content := make([]byte, 1000)
	path := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-01", "s1", string(content), time.Hour)

	p := NewPruner(dir, 0, 0, time.Hour, noActiveFiles)
	stats := p.PruneOnce()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("size cap disabled (0) but file was removed: %v", err)
	}
	if stats.SizeFilesDeleted != 0 {
		t.Errorf("SizeFilesDeleted = %d, want 0", stats.SizeFilesDeleted)
	}
}

func TestPruneOnce_KeepsActiveFileDespiteAge(t *testing.T) {
	dir := t.TempDir()

	activePath := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-01", "active-session", "x", 365*24*time.Hour)
	inactivePath := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-01", "inactive-session", "x", 365*24*time.Hour)

	active := func() map[string]struct{} {
		return map[string]struct{}{filepath.Clean(activePath): {}}
	}

	p := NewPruner(dir, 14, 0, time.Hour, active)
	stats := p.PruneOnce()

	if _, err := os.Stat(activePath); err != nil {
		t.Errorf("active file was pruned despite being open for append: %v", err)
	}
	if _, err := os.Stat(inactivePath); !os.IsNotExist(err) {
		t.Errorf("expected inactive old file to be removed, stat err = %v", err)
	}
	if stats.AgeFilesDeleted != 1 {
		t.Errorf("AgeFilesDeleted = %d, want 1 (only the inactive file)", stats.AgeFilesDeleted)
	}
}

func TestPruneOnce_KeepsActiveFileDespiteSizeCap(t *testing.T) {
	dir := t.TempDir()

	content := make([]byte, 100)
	// Active file is the oldest and would normally be the first evicted by
	// the size cap - it must survive anyway.
	activePath := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-01", "active-session", string(content), 3*time.Hour)
	otherPath := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-02", "other-session", string(content), 2*time.Hour)

	active := func() map[string]struct{} {
		return map[string]struct{}{filepath.Clean(activePath): {}}
	}

	// Cap well under total (200 bytes of real data) to force eviction.
	p := NewPruner(dir, 0, 50, time.Hour, active)
	stats := p.PruneOnce()

	if _, err := os.Stat(activePath); err != nil {
		t.Errorf("active file was pruned by the size cap: %v", err)
	}
	if _, err := os.Stat(otherPath); !os.IsNotExist(err) {
		t.Errorf("expected the non-active file to be removed, stat err = %v", err)
	}
	if stats.SizeFilesDeleted != 1 {
		t.Errorf("SizeFilesDeleted = %d, want 1", stats.SizeFilesDeleted)
	}
}

func TestPruneOnce_RemovesEmptyDateDirsAfterAgePrune(t *testing.T) {
	dir := t.TempDir()

	oldPath := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-01", "old-session", "x", 20*24*time.Hour)
	oldDateDir := filepath.Dir(oldPath)
	newPath := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-20", "new-session", "x", time.Hour)
	newDateDir := filepath.Dir(newPath)

	p := NewPruner(dir, 14, 0, time.Hour, noActiveFiles)
	stats := p.PruneOnce()

	if _, err := os.Stat(oldDateDir); !os.IsNotExist(err) {
		t.Errorf("expected emptied date dir %s to be removed, stat err = %v", oldDateDir, err)
	}
	if _, err := os.Stat(newDateDir); err != nil {
		t.Errorf("expected non-empty date dir %s to survive, stat err = %v", newDateDir, err)
	}
	if stats.DirsRemoved != 1 {
		t.Errorf("DirsRemoved = %d, want 1", stats.DirsRemoved)
	}
}

func TestPruneOnce_RemovesEmptyDateDirsAfterSizeCapPrune(t *testing.T) {
	dir := t.TempDir()
	content := make([]byte, 100)

	oldest := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-01", "s1", string(content), 3*time.Hour)
	oldestDateDir := filepath.Dir(oldest)
	newest := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-02", "s2", string(content), 1*time.Hour)
	newestDateDir := filepath.Dir(newest)

	p := NewPruner(dir, 0, 100, time.Hour, noActiveFiles)
	p.PruneOnce()

	if _, err := os.Stat(oldestDateDir); !os.IsNotExist(err) {
		t.Errorf("expected emptied date dir %s to be removed after size-cap prune, stat err = %v", oldestDateDir, err)
	}
	if _, err := os.Stat(newestDateDir); err != nil {
		t.Errorf("expected surviving date dir %s to remain, stat err = %v", newestDateDir, err)
	}
}

func TestPruneOnce_MissingBaseDirIsNoOp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	p := NewPruner(dir, 14, 5*1024*1024*1024, time.Hour, noActiveFiles)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PruneOnce panicked on missing base dir: %v", r)
		}
	}()

	stats := p.PruneOnce()
	if stats.Errors != 0 {
		t.Errorf("missing base dir should not count as an error, got Errors = %d", stats.Errors)
	}
	if stats.AgeFilesDeleted != 0 || stats.SizeFilesDeleted != 0 {
		t.Errorf("expected no deletions against a missing dir, got %+v", stats)
	}
}

func TestPruneOnce_UnreadableDateDirDoesNotCrash(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}

	dir := t.TempDir()

	blockedDateDir := filepath.Join(dir, "api.anthropic.com", "2026-01-01")
	if err := os.MkdirAll(blockedDateDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blockedDateDir, "s1.jsonl"), []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Make the directory unreadable; restore permissions so TempDir cleanup
	// can still remove it.
	if err := os.Chmod(blockedDateDir, 0000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(blockedDateDir, 0755) })

	okPath := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-02-01", "s2", "x", 20*24*time.Hour)

	p := NewPruner(dir, 14, 0, time.Hour, noActiveFiles)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PruneOnce panicked on unreadable directory: %v", r)
		}
	}()

	stats := p.PruneOnce()

	if stats.Errors == 0 {
		t.Error("expected at least one recorded error for the unreadable directory")
	}
	// The readable directory's old file must still be pruned - one
	// unreadable directory must not block the rest of the pass.
	if _, err := os.Stat(okPath); !os.IsNotExist(err) {
		t.Errorf("expected old file in a readable dir to still be pruned, stat err = %v", err)
	}
}

func TestPruneOnce_IgnoresNonDirectoryTopLevelEntries(t *testing.T) {
	dir := t.TempDir()

	// sessions.db lives directly under baseDir, not inside an upstream/date
	// structure. It must never be touched by the pruner, no matter how old.
	dbPath := filepath.Join(dir, "sessions.db")
	if err := os.WriteFile(dbPath, []byte("not a log"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	oldTime := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(dbPath, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	p := NewPruner(dir, 14, 1, time.Hour, noActiveFiles)
	p.PruneOnce()

	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("sessions.db must never be pruned: %v", err)
	}
}

func TestNewPruner_DefaultsIntervalWhenNonPositive(t *testing.T) {
	p := NewPruner(t.TempDir(), 14, 0, 0, nil)
	if p.interval != time.Hour {
		t.Errorf("interval = %v, want default 1h", p.interval)
	}
	// activeFiles must never be nil - PruneOnce calls it unconditionally.
	if p.activeFiles == nil {
		t.Error("activeFiles callback should default to a non-nil no-op")
	}
	if got := p.activeFiles(); got != nil {
		t.Errorf("default activeFiles() = %v, want nil", got)
	}
}

func TestPruner_StartStop(t *testing.T) {
	dir := t.TempDir()
	oldPath := writeDatedLogFile(t, dir, "api.anthropic.com", "2026-01-01", "old-session", "x", 20*24*time.Hour)

	p := NewPruner(dir, 14, 0, 10*time.Millisecond, noActiveFiles)

	// Start's initial pass is synchronous precisely so there is no race for a
	// caller to wait out: by the time Start returns, the old file is already gone.
	p.Start()
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("expected the startup pass to have already pruned the old file by the time Start returns, stat err = %v", err)
	}

	// Second Start must be a safe no-op, not a second goroutine/double-close panic.
	p.Start()

	p.Stop()
	// Stop must be idempotent.
	p.Stop()
}

// logger_test.go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoggerWritesJSONL(t *testing.T) {
	tmpDir := t.TempDir()

	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close()

	sessionID := "20260113-102345-a7f3"
	provider := "anthropic"
	upstream := "api.anthropic.com"

	// Log a session start
	err = logger.LogSessionStart(sessionID, provider, upstream)
	if err != nil {
		t.Fatalf("Failed to log session start: %v", err)
	}

	// Log a request
	headers := http.Header{"X-Api-Key": []string{"sk-ant-secret123456"}}
	err = logger.LogRequest(sessionID, provider, 1, "POST", "/v1/messages", headers, []byte(`{"test":"data"}`), "test-request-id")
	if err != nil {
		t.Fatalf("Failed to log request: %v", err)
	}

	// Verify file was created - new path structure: <upstream>/<date>/<sessionID>.jsonl
	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(tmpDir, upstream, today, sessionID+".jsonl")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("Expected 2 lines, got %d", len(lines))
	}

	// Verify session_start entry
	var startEntry map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &startEntry); err != nil {
		t.Fatalf("Failed to parse session_start: %v", err)
	}
	if startEntry["type"] != "session_start" {
		t.Errorf("Expected type session_start, got %v", startEntry["type"])
	}

	// Verify request entry
	var reqEntry map[string]interface{}
	if err := json.Unmarshal([]byte(lines[1]), &reqEntry); err != nil {
		t.Fatalf("Failed to parse request: %v", err)
	}
	if reqEntry["type"] != "request" {
		t.Errorf("Expected type request, got %v", reqEntry["type"])
	}

	// Verify API key was obfuscated
	reqHeaders := reqEntry["headers"].(map[string]interface{})
	apiKey := reqHeaders["X-Api-Key"].([]interface{})[0].(string)
	if strings.Contains(apiKey, "secret") {
		t.Error("API key was not obfuscated in log")
	}
}

func TestLogPathStructure(t *testing.T) {
	tmpDir := t.TempDir()
	logger, _ := NewLogger(tmpDir)
	defer logger.Close()

	sessionID := "20260114-091523-abcd1234"
	upstream := "api.anthropic.com"

	logger.LogSessionStart(sessionID, "anthropic", upstream)

	// Wait for async write
	time.Sleep(50 * time.Millisecond)

	// Expect: tmpDir/api.anthropic.com/2026-01-14/20260114-091523-abcd1234.jsonl
	today := time.Now().Format("2006-01-02")
	expectedPath := filepath.Join(tmpDir, upstream, today, sessionID+".jsonl")

	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Errorf("Expected log at %s", expectedPath)
	}
}

func TestLoggerResponseWithTiming(t *testing.T) {
	tmpDir := t.TempDir()

	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close()

	sessionID := "20260113-102345-test"
	provider := "anthropic"
	upstream := "api.anthropic.com"

	logger.LogSessionStart(sessionID, provider, upstream)

	timing := ResponseTiming{
		TTFBMs:  150,
		TotalMs: 1200,
	}

	err = logger.LogResponse(sessionID, provider, 1, 200, http.Header{}, []byte(`{"response":"ok"}`), nil, timing, "test-request-id", ResponseCapture{Termination: TerminationEOF})
	if err != nil {
		t.Fatalf("Failed to log response: %v", err)
	}

	// Read and verify - new path structure: <upstream>/<date>/<sessionID>.jsonl
	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(tmpDir, upstream, today, sessionID+".jsonl")
	data, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	var respEntry map[string]interface{}
	json.Unmarshal([]byte(lines[1]), &respEntry)

	timingData := respEntry["timing"].(map[string]interface{})
	if timingData["ttfb_ms"].(float64) != 150 {
		t.Errorf("TTFB not logged correctly")
	}
}

func TestLogEntryHasMeta(t *testing.T) {
	tmpDir := t.TempDir()

	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close()

	sessionID := "test-session-meta"
	upstream := "api.anthropic.com"

	logger.LogSessionStart(sessionID, "anthropic", upstream)
	logger.LogRequest(sessionID, "anthropic", 1, "POST", "/v1/messages", nil, []byte(`{}`), "test-request-id")

	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(tmpDir, upstream, today, sessionID+".jsonl")
	data, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	// Check request entry has _meta
	var reqEntry map[string]interface{}
	json.Unmarshal([]byte(lines[1]), &reqEntry)

	meta, ok := reqEntry["_meta"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected _meta block in log entry")
	}

	// Verify required fields
	if _, ok := meta["ts"]; !ok {
		t.Error("_meta missing ts field")
	}
	if _, ok := meta["machine"]; !ok {
		t.Error("_meta missing machine field")
	}
	if _, ok := meta["host"]; !ok {
		t.Error("_meta missing host field")
	}
	if _, ok := meta["session"]; !ok {
		t.Error("_meta missing session field")
	}
	if _, ok := meta["request_id"]; !ok {
		t.Error("_meta missing request_id field")
	}

	// Verify machine format is user@host
	machine := meta["machine"].(string)
	if !strings.Contains(machine, "@") {
		t.Errorf("Expected machine format user@host, got %s", machine)
	}
}

func TestLoggerLogObservation(t *testing.T) {
	tmpDir := t.TempDir()

	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close()

	sessionID := "mantle-session-123"
	provider := "openai"
	upstream := "bedrock-mantle.us-west-2.api.aws"
	logger.RegisterUpstream(sessionID, upstream)

	entry := map[string]any{
		"type": "request",
		"_meta": map[string]any{
			"schema_version":     "telemetry-contract-v0",
			"cloud_build_run_id": "run-test",
			"provider":           provider,
			"provider_route":     "bedrock-mantle",
			"wire_api":           "openai-responses",
		},
		"request": map[string]any{
			"ingress_path":  "/cbrun/run-test/mantle/v1/responses",
			"raw_query":     "trace=1",
			"upstream_host": "bedrock-mantle.us-west-2.api.aws",
		},
	}

	if err := logger.LogObservation(sessionID, provider, entry); err != nil {
		t.Fatalf("Failed to log observation: %v", err)
	}

	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(tmpDir, upstream, today, sessionID+".jsonl")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("Expected 1 line, got %d", len(lines))
	}

	var logged map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &logged); err != nil {
		t.Fatalf("Failed to parse observation entry: %v", err)
	}

	request := logged["request"].(map[string]any)
	if got := request["ingress_path"]; got != "/cbrun/run-test/mantle/v1/responses" {
		t.Fatalf("request.ingress_path = %v, want /cbrun/run-test/mantle/v1/responses", got)
	}
	if got := request["raw_query"]; got != "trace=1" {
		t.Fatalf("request.raw_query = %v, want trace=1", got)
	}
	if got := request["upstream_host"]; got != "bedrock-mantle.us-west-2.api.aws" {
		t.Fatalf("request.upstream_host = %v, want bedrock-mantle.us-west-2.api.aws", got)
	}

	meta := logged["_meta"].(map[string]any)
	if got := meta["cloud_build_run_id"]; got != "run-test" {
		t.Fatalf("_meta.cloud_build_run_id = %v, want run-test", got)
	}
	if got := meta["provider_route"]; got != "bedrock-mantle" {
		t.Fatalf("_meta.provider_route = %v, want bedrock-mantle", got)
	}
	if got := meta["wire_api"]; got != "openai-responses" {
		t.Fatalf("_meta.wire_api = %v, want openai-responses", got)
	}
}

func TestRequestLogContextWritesNeutralRunMetadata(t *testing.T) {
	meta := map[string]interface{}{}
	addRequestLogContextMeta(meta, RequestLogContext{
		RunID:         "proj-20260628-run",
		ResolvedRunID: "resolved-other-run-id",
		ClientFPHash:  strings.Repeat("a", 64),
		Project:       "proj",
		ProviderRoute: "aws-bedrock",
		WireAPI:       "messages",
	})

	if got := meta["run_id"]; got != "proj-20260628-run" {
		t.Fatalf("run_id = %v, want proj-20260628-run", got)
	}
	if got := meta["resolved_run_id"]; got != "resolved-other-run-id" {
		t.Fatalf("resolved_run_id = %v, want resolved-other-run-id", got)
	}
	if _, ok := meta["cloud_build_run_id"]; ok {
		t.Fatalf("legacy cloud_build_run_id present in neutral context: %#v", meta)
	}
}

func TestRequestLogContextWritesLegacyRunMetadata(t *testing.T) {
	meta := map[string]interface{}{}
	addRequestLogContextMeta(meta, RequestLogContext{
		LegacyRunID: "legacy-run",
	})

	if got := meta["cloud_build_run_id"]; got != "legacy-run" {
		t.Fatalf("cloud_build_run_id = %v, want legacy-run", got)
	}
	if _, ok := meta["run_id"]; ok {
		t.Fatalf("run_id unexpectedly present in legacy context: %#v", meta)
	}
	if _, ok := meta["resolved_run_id"]; ok {
		t.Fatalf("resolved_run_id unexpectedly present in legacy context: %#v", meta)
	}
}

// --- File-handle leak regression tests ---
//
// The Logger never closed a session's log file until the whole process
// shut down (Close). Every session that ever wrote a line stayed in
// l.files, and its *os.File, for the life of the process - an unbounded
// leak that also meant a file deleted out from under it (manual cleanup,
// or the pruner in PRI-3457) never actually freed disk space, because the
// kernel won't reclaim a deleted file's blocks while any process still
// holds it open. These tests open sessions, fake the idle clock forward,
// and assert the Logger actually lets them go.

// openNSessions registers and writes one entry for n fresh sessions, each
// under its own upstream so they land in distinct dirs, and returns their
// IDs.
func openNSessions(t *testing.T, logger *Logger, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		sessionID := fmt.Sprintf("20260113-102345-sess%04d", i)
		upstream := fmt.Sprintf("upstream-%d.example.com", i)
		if err := logger.LogSessionStart(sessionID, "anthropic", upstream); err != nil {
			t.Fatalf("LogSessionStart(%s): %v", sessionID, err)
		}
		ids[i] = sessionID
	}
	return ids
}

// countOpenFDsUnder returns how many of this process's open file
// descriptors currently point somewhere under dir, by walking
// /proc/self/fd. Skips (rather than fails) on a kernel without /proc, so
// this stays portable to non-Linux CI while still being meaningful where
// it matters (PRI-3457's reported host is Linux).
func countOpenFDsUnder(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot read /proc/self/fd (non-Linux?): %v", err)
	}
	dir = filepath.Clean(dir)
	count := 0
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			continue
		}
		if strings.HasPrefix(link, dir) {
			count++
		}
	}
	return count
}

// TestLogger_Baseline_SettingNowAloneDoesNotCloseFiles is a baseline
// characterization test, not a regression test for the fix: it documents
// that closing is driven only by an explicit sweep (CloseIdleFiles,
// called directly or by the Start goroutine), never by merely overriding
// l.now. It passes before, during, and after the fix - including with
// CloseIdleFiles mutated to a no-op - because nothing in it ever triggers
// a sweep. See TestLogger_CloseIdleFiles_ClosesStaleSessions and
// TestLogger_StartStop_Idempotent for the tests that actually exercise
// (and fail without) the fix.
func TestLogger_Baseline_SettingNowAloneDoesNotCloseFiles(t *testing.T) {
	tmpDir := t.TempDir()
	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	const n = 50
	openNSessions(t, logger, n)

	logger.mu.Lock()
	openBefore := len(logger.files)
	logger.mu.Unlock()
	if openBefore != n {
		t.Fatalf("open files after writing %d sessions = %d, want %d", n, openBefore, n)
	}

	fdBefore := countOpenFDsUnder(t, tmpDir)
	if fdBefore < n {
		t.Fatalf("open fds under %s = %d, want at least %d", tmpDir, fdBefore, n)
	}

	// Advance the fake clock two hours. By itself this changes nothing:
	// no sweep has run, so nothing is closed - closing only ever happens
	// inside CloseIdleFiles.
	logger.now = func() time.Time { return time.Now().Add(2 * time.Hour) }

	logger.mu.Lock()
	openAfter := len(logger.files)
	logger.mu.Unlock()
	if openAfter != n {
		t.Fatalf("open files after advancing the clock with no sweep = %d, want still %d", openAfter, n)
	}

	fdAfter := countOpenFDsUnder(t, tmpDir)
	if fdAfter != fdBefore {
		t.Fatalf("open fds under %s after advancing the clock = %d, want still %d", tmpDir, fdAfter, fdBefore)
	}
}

// TestLogger_CloseIdleFiles_ClosesStaleSessions is the fix's core
// contract: open N sessions, advance the fake clock past idleTimeout, and
// assert CloseIdleFiles actually closes every one of them - both in the
// logger's own bookkeeping (l.files) and as real, OS-visible file
// descriptors.
func TestLogger_CloseIdleFiles_ClosesStaleSessions(t *testing.T) {
	tmpDir := t.TempDir()
	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	const n = 50
	openNSessions(t, logger, n)

	fdBefore := countOpenFDsUnder(t, tmpDir)
	if fdBefore < n {
		t.Fatalf("open fds under %s = %d, want at least %d", tmpDir, fdBefore, n)
	}

	base := time.Now()
	logger.now = func() time.Time { return base.Add(logger.idleTimeout + time.Minute) }

	closed := logger.CloseIdleFiles()
	if closed != n {
		t.Fatalf("CloseIdleFiles() = %d, want %d", closed, n)
	}

	logger.mu.Lock()
	openAfter := len(logger.files)
	logger.mu.Unlock()
	if openAfter != 0 {
		t.Fatalf("open files after CloseIdleFiles = %d, want 0", openAfter)
	}

	fdAfter := countOpenFDsUnder(t, tmpDir)
	if fdAfter != 0 {
		t.Fatalf("open fds under %s after CloseIdleFiles = %d, want 0", tmpDir, fdAfter)
	}
}

// TestLogger_CloseIdleFiles_KeepsRecentSessionsOpen is the companion
// negative case: a session that wrote within idleTimeout must not be
// closed out from under an in-flight conversation.
func TestLogger_CloseIdleFiles_KeepsRecentSessionsOpen(t *testing.T) {
	tmpDir := t.TempDir()
	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	openNSessions(t, logger, 3)

	base := time.Now()
	logger.now = func() time.Time { return base.Add(logger.idleTimeout / 2) }

	if closed := logger.CloseIdleFiles(); closed != 0 {
		t.Fatalf("CloseIdleFiles() = %d, want 0 (nothing is idle yet)", closed)
	}

	logger.mu.Lock()
	open := len(logger.files)
	logger.mu.Unlock()
	if open != 3 {
		t.Fatalf("open files = %d, want 3 (none should have been closed)", open)
	}
}

// TestLogger_CloseIdleFiles_ClosesOnDateRollover covers the other half of
// the fix: a session that stays open across midnight must have its
// yesterday-dated file closed, and the next write for that session must
// land in a freshly opened file under today's date dir - not keep
// appending to yesterday's.
func TestLogger_CloseIdleFiles_ClosesOnDateRollover(t *testing.T) {
	tmpDir := t.TempDir()
	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	day1 := time.Date(2026, 1, 13, 23, 55, 0, 0, time.UTC)
	logger.now = func() time.Time { return day1 }

	sessionID := "20260113-235500-rollover"
	upstream := "api.anthropic.com"
	if err := logger.LogSessionStart(sessionID, "anthropic", upstream); err != nil {
		t.Fatalf("LogSessionStart: %v", err)
	}

	day1Path := filepath.Join(tmpDir, upstream, "2026-01-13", sessionID+".jsonl")
	if _, err := os.Stat(day1Path); err != nil {
		t.Fatalf("expected day-1 file at %s: %v", day1Path, err)
	}

	// Roll the clock past midnight, within idleTimeout of the last write,
	// then sweep. A plain idle check alone would NOT close this file; the
	// rollover check must.
	day2 := day1.Add(10 * time.Minute) // 23:55 -> 00:05 next day
	logger.now = func() time.Time { return day2 }

	closed := logger.CloseIdleFiles()
	if closed != 1 {
		t.Fatalf("CloseIdleFiles() across date rollover = %d, want 1", closed)
	}

	logger.mu.Lock()
	_, stillOpen := logger.files[sessionID]
	logger.mu.Unlock()
	if stillOpen {
		t.Fatalf("session %s still has a file open after date rollover sweep", sessionID)
	}

	// Writing again reopens under today's dir, in append mode, without
	// touching yesterday's file or losing its content.
	if err := logger.LogFork(sessionID, "anthropic", 1, "parent-session"); err != nil {
		t.Fatalf("LogFork after rollover: %v", err)
	}

	day2Path := filepath.Join(tmpDir, upstream, "2026-01-14", sessionID+".jsonl")
	if _, err := os.Stat(day2Path); err != nil {
		t.Fatalf("expected day-2 file at %s: %v", day2Path, err)
	}

	day1Data, err := os.ReadFile(day1Path)
	if err != nil {
		t.Fatalf("day-1 file unreadable after rollover: %v", err)
	}
	if !strings.Contains(string(day1Data), "session_start") {
		t.Fatalf("day-1 file lost its content after rollover: %q", day1Data)
	}
}

// hasDeletedFD reports whether this process currently holds an fd whose
// /proc/self/fd symlink target is path suffixed with the kernel's
// "(deleted)" marker - the Linux signal that a file is unlinked but still
// held open by some descriptor.
func hasDeletedFD(t *testing.T, path string) bool {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot read /proc/self/fd (non-Linux?): %v", err)
	}
	want := path + " (deleted)"
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			continue
		}
		if link == want {
			return true
		}
	}
	return false
}

// TestLogger_DeletedIdleFile_ReleasesDescriptor is the disk-space half of
// the bug report: deleting a log file (what the PRI-3457 pruner does)
// does not free its blocks while this process still holds an open
// descriptor pointing at it. This proves both ends - that an open-but-
// unlinked descriptor is detectable via /proc/self/fd ("(deleted)"), and
// that CloseIdleFiles makes it go away.
func TestLogger_DeletedIdleFile_ReleasesDescriptor(t *testing.T) {
	tmpDir := t.TempDir()
	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	sessionID := "20260113-102345-deleted"
	upstream := "api.anthropic.com"
	if err := logger.LogSessionStart(sessionID, "anthropic", upstream); err != nil {
		t.Fatalf("LogSessionStart: %v", err)
	}

	today := time.Now().Format("2006-01-02")
	path := filepath.Join(tmpDir, upstream, today, sessionID+".jsonl")

	// Simulate the pruner deleting the file while the Logger still has it
	// open for append - exactly what PRI-3457's Pruner would do to any
	// file not reported by ActiveLogFiles.
	if err := os.Remove(path); err != nil {
		t.Fatalf("os.Remove(%s): %v", path, err)
	}

	if !hasDeletedFD(t, path) {
		t.Fatalf("expected an open fd pointing at deleted file %s before CloseIdleFiles; the kernel has nothing to detect the leak with if this isn't true", path)
	}

	base := time.Now()
	logger.now = func() time.Time { return base.Add(logger.idleTimeout + time.Minute) }

	if closed := logger.CloseIdleFiles(); closed != 1 {
		t.Fatalf("CloseIdleFiles() = %d, want 1", closed)
	}

	if hasDeletedFD(t, path) {
		t.Fatalf("fd pointing at deleted file %s is still open after CloseIdleFiles; disk space was not released", path)
	}
}

// TestLogger_StartStop_Idempotent exercises the real background goroutine
// end to end (not just CloseIdleFiles called directly): a short sweep
// interval, idle sessions, Start, and a wait for the sweep to actually
// run and close them - plus Start/Stop idempotency, matching the
// lifecycle contract the rest of this codebase uses for its other
// background loops.
func TestLogger_StartStop_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	logger.sweepInterval = 10 * time.Millisecond
	logger.idleTimeout = 10 * time.Millisecond

	openNSessions(t, logger, 5)

	logger.Start()
	logger.Start() // second call must be a no-op, not a second goroutine

	deadline := time.Now().Add(5 * time.Second)
	for {
		logger.mu.Lock()
		open := len(logger.files)
		logger.mu.Unlock()
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background sweep never closed idle sessions; %d still open", open)
		}
		time.Sleep(10 * time.Millisecond)
	}

	logger.Stop()
	logger.Stop() // idempotent

	// Close must not hang or panic after Stop already ran.
	if err := logger.Close(); err != nil {
		t.Fatalf("Close after Stop: %v", err)
	}
}

// TestLogger_ConcurrentWritesAndSweep writes from many goroutines while
// the idle sweep runs concurrently, to be run with -race: getFile's
// rollover/close path and CloseIdleFiles both mutate l.files and must
// never race or panic on a file closed out from under a concurrent
// writer.
func TestLogger_ConcurrentWritesAndSweep(t *testing.T) {
	tmpDir := t.TempDir()
	logger, err := NewLogger(tmpDir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	logger.sweepInterval = time.Millisecond
	logger.idleTimeout = time.Millisecond
	logger.Start()
	defer logger.Stop()

	const sessions = 8
	const writesPerSession = 50

	var wg sync.WaitGroup
	for i := 0; i < sessions; i++ {
		sessionID := fmt.Sprintf("20260113-102345-race%d", i)
		upstream := fmt.Sprintf("race-upstream-%d.example.com", i)
		wg.Add(1)
		go func(sessionID, upstream string) {
			defer wg.Done()
			if err := logger.LogSessionStart(sessionID, "anthropic", upstream); err != nil {
				t.Errorf("LogSessionStart(%s): %v", sessionID, err)
				return
			}
			for j := 0; j < writesPerSession; j++ {
				if err := logger.LogObservation(sessionID, "anthropic", map[string]any{"type": "observation", "n": j}); err != nil {
					t.Errorf("LogObservation(%s, %d): %v", sessionID, j, err)
					return
				}
			}
		}(sessionID, upstream)
	}
	wg.Wait()
}

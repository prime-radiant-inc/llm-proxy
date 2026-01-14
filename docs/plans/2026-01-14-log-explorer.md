# Log Explorer Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a web UI (`--explore`) for browsing and searching LLM API logs with conversation rendering.

**Architecture:** Server-rendered HTML using Go templates and `embed`. Three pages: session list, session detail with conversation view, and full-text search. Log entries get a `_meta` block with machine identifier, host, session ID, and timestamp for aggregation support.

**Tech Stack:** Go stdlib (`html/template`, `net/http`, `embed`), minimal CSS (Pico.css embedded), no JS build step.

---

## Task 1: Add _meta Block to Log Entries

**Files:**
- Modify: `logger.go:14-30` (add MachineID field to Logger)
- Modify: `logger.go:92-108` (add _meta to writeEntry)
- Modify: `logger_test.go` (update tests for new format)

### Step 1: Write failing test for _meta in log entries

Add to `logger_test.go`:

```go
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
	logger.LogRequest(sessionID, "anthropic", 1, "POST", "/v1/messages", nil, []byte(`{}`))

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

	// Verify machine format is user@host
	machine := meta["machine"].(string)
	if !strings.Contains(machine, "@") {
		t.Errorf("Expected machine format user@host, got %s", machine)
	}
}
```

### Step 2: Run test to verify it fails

```bash
go test -v -run TestLogEntryHasMeta ./...
```

Expected: FAIL with "_meta block" not found

### Step 3: Implement _meta support in Logger

Modify `logger.go`:

```go
// Add to Logger struct (around line 25):
type Logger struct {
	baseDir   string
	machineID string  // NEW: user@hostname
	mu        sync.Mutex
	files     map[string]*os.File
	upstreams map[string]string
}

// Add helper function after imports:
func getMachineID() string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	username := "unknown"
	if u, err := user.Current(); err == nil {
		username = u.Username
	}

	return username + "@" + hostname
}

// Update NewLogger (around line 32):
func NewLogger(baseDir string) (*Logger, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	return &Logger{
		baseDir:   baseDir,
		machineID: getMachineID(),  // NEW
		files:     make(map[string]*os.File),
		upstreams: make(map[string]string),
	}, nil
}
```

Add `"os/user"` to imports.

### Step 4: Restructure log entries to use _meta

Modify the log methods to move timestamp to `_meta` and add other fields. Update `LogRequest`:

```go
func (l *Logger) LogRequest(sessionID, provider string, seq int, method, path string, headers http.Header, body []byte) error {
	upstream := l.upstreams[sessionID]

	entry := map[string]interface{}{
		"type":    "request",
		"seq":     seq,
		"method":  method,
		"path":    path,
		"headers": ObfuscateHeaders(headers),
		"body":    string(body),
		"size":    len(body),
		"_meta": map[string]interface{}{
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"machine": l.machineID,
			"host":    upstream,
			"session": sessionID,
		},
	}
	return l.writeEntry(sessionID, entry)
}
```

Similarly update `LogSessionStart`, `LogResponse`, and `LogFork` to use `_meta`.

### Step 5: Run test to verify it passes

```bash
go test -v -run TestLogEntryHasMeta ./...
```

Expected: PASS

### Step 6: Update existing tests for new format

Some existing tests check for `ts` at top level. Update them to check `_meta.ts` instead, or make them tolerant of the new structure.

Run full test suite:

```bash
go test -v ./...
```

Fix any failures by updating assertions.

### Step 7: Commit

```bash
git add logger.go logger_test.go
git commit -m "feat: add _meta block to all log entries

Adds machine identifier (user@host), upstream host, session ID,
and timestamp to every log entry for aggregation support."
```

---

## Task 2: Add --explore Flag and Basic Server

**Files:**
- Modify: `main.go:17-27` (add Explore flag)
- Modify: `main.go:29-48` (parse Explore flag)
- Create: `explorer.go` (new file for explorer server)
- Create: `explorer_test.go` (tests)

### Step 1: Write failing test for explorer server

Create `explorer_test.go`:

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
```

Add `"strings"` to imports.

### Step 2: Run test to verify it fails

```bash
go test -v -run TestExplorer ./...
```

Expected: FAIL with "NewExplorer" undefined

### Step 3: Create basic explorer server

Create `explorer.go`:

```go
package main

import (
	"embed"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

type Explorer struct {
	logDir    string
	templates *template.Template
	mux       *http.ServeMux
}

type SessionInfo struct {
	ID        string
	Host      string
	Date      string
	Path      string
	ModTime   time.Time
}

func NewExplorer(logDir string) *Explorer {
	tmpl := template.Must(template.ParseFS(templateFS, "templates/*.html"))

	e := &Explorer{
		logDir:    logDir,
		templates: tmpl,
		mux:       http.NewServeMux(),
	}

	e.mux.HandleFunc("/", e.handleHome)
	e.mux.HandleFunc("/health", e.handleHealth)
	e.mux.HandleFunc("/session/", e.handleSession)
	e.mux.HandleFunc("/search", e.handleSearch)
	e.mux.Handle("/static/", http.FileServer(http.FS(staticFS)))

	return e
}

func (e *Explorer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mux.ServeHTTP(w, r)
}

func (e *Explorer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (e *Explorer) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	sessions := e.listSessions()

	e.templates.ExecuteTemplate(w, "home.html", map[string]interface{}{
		"Sessions": sessions,
	})
}

func (e *Explorer) listSessions() []SessionInfo {
	var sessions []SessionInfo

	// Walk: logDir/<host>/<date>/<session>.jsonl
	filepath.Walk(e.logDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}

		rel, _ := filepath.Rel(e.logDir, path)
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 3 {
			return nil
		}

		sessions = append(sessions, SessionInfo{
			ID:      strings.TrimSuffix(parts[2], ".jsonl"),
			Host:    parts[0],
			Date:    parts[1],
			Path:    path,
			ModTime: info.ModTime(),
		})
		return nil
	})

	// Sort by date descending, then mod time descending
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Date != sessions[j].Date {
			return sessions[i].Date > sessions[j].Date
		}
		return sessions[i].ModTime.After(sessions[j].ModTime)
	})

	return sessions
}

func (e *Explorer) handleSession(w http.ResponseWriter, r *http.Request) {
	// TODO: implement in Task 4
	http.Error(w, "Not implemented", http.StatusNotImplemented)
}

func (e *Explorer) handleSearch(w http.ResponseWriter, r *http.Request) {
	// TODO: implement in Task 6
	http.Error(w, "Not implemented", http.StatusNotImplemented)
}
```

### Step 4: Create minimal templates

Create `templates/home.html`:

```html
<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>LLM Proxy Explorer</title>
    <link rel="stylesheet" href="/static/style.css">
</head>
<body>
    <nav>
        <h1>LLM Proxy Explorer</h1>
        <form action="/search" method="get">
            <input type="text" name="q" placeholder="Search logs...">
            <button type="submit">Search</button>
        </form>
    </nav>
    <main>
        <h2>Sessions</h2>
        {{if not .Sessions}}
        <p>No sessions found.</p>
        {{else}}
        {{$currentDate := ""}}
        {{range .Sessions}}
            {{if ne .Date $currentDate}}
                {{$currentDate = .Date}}
                <h3>{{.Date}}</h3>
            {{end}}
            <div class="session">
                <a href="/session/{{.ID}}">{{.ID}}</a>
                <span class="host">{{.Host}}</span>
            </div>
        {{end}}
        {{end}}
    </main>
</body>
</html>
```

Create `templates/session.html` (placeholder):

```html
<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>Session - LLM Proxy Explorer</title>
    <link rel="stylesheet" href="/static/style.css">
</head>
<body>
    <nav>
        <a href="/">LLM Proxy Explorer</a>
        <form action="/search" method="get">
            <input type="text" name="q" placeholder="Search logs...">
            <button type="submit">Search</button>
        </form>
    </nav>
    <main>
        <h2>Session: {{.SessionID}}</h2>
        <p>Not yet implemented</p>
    </main>
</body>
</html>
```

Create `templates/search.html` (placeholder):

```html
<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>Search - LLM Proxy Explorer</title>
    <link rel="stylesheet" href="/static/style.css">
</head>
<body>
    <nav>
        <a href="/">LLM Proxy Explorer</a>
        <form action="/search" method="get">
            <input type="text" name="q" value="{{.Query}}" placeholder="Search logs...">
            <button type="submit">Search</button>
        </form>
    </nav>
    <main>
        <h2>Search Results</h2>
        <p>Not yet implemented</p>
    </main>
</body>
</html>
```

Create `static/style.css`:

```css
:root {
    --bg: #1a1a2e;
    --bg-secondary: #16213e;
    --text: #eee;
    --text-muted: #888;
    --accent: #4a9eff;
    --border: #333;
}

* { box-sizing: border-box; }

body {
    font-family: system-ui, -apple-system, sans-serif;
    background: var(--bg);
    color: var(--text);
    margin: 0;
    padding: 0;
    line-height: 1.6;
}

nav {
    background: var(--bg-secondary);
    padding: 1rem 2rem;
    display: flex;
    justify-content: space-between;
    align-items: center;
    border-bottom: 1px solid var(--border);
}

nav h1, nav a {
    color: var(--accent);
    text-decoration: none;
    margin: 0;
    font-size: 1.2rem;
}

nav form {
    display: flex;
    gap: 0.5rem;
}

nav input[type="text"] {
    padding: 0.5rem;
    border: 1px solid var(--border);
    border-radius: 4px;
    background: var(--bg);
    color: var(--text);
    width: 300px;
}

nav button {
    padding: 0.5rem 1rem;
    background: var(--accent);
    color: white;
    border: none;
    border-radius: 4px;
    cursor: pointer;
}

main {
    padding: 2rem;
    max-width: 1200px;
    margin: 0 auto;
}

h2, h3 {
    color: var(--text);
    border-bottom: 1px solid var(--border);
    padding-bottom: 0.5rem;
}

.session {
    padding: 0.5rem 0;
    display: flex;
    gap: 1rem;
    align-items: center;
}

.session a {
    color: var(--accent);
    font-family: monospace;
}

.session .host {
    color: var(--text-muted);
    font-size: 0.9rem;
}
```

### Step 5: Run test to verify it passes

```bash
go test -v -run TestExplorer ./...
```

Expected: PASS

### Step 6: Add --explore flag to main.go

Add to `CLIFlags` struct:

```go
Explore     bool
ExplorePort int
```

Add to `ParseCLIFlags`:

```go
fs.BoolVar(&flags.Explore, "explore", false, "Start log explorer web UI")
fs.IntVar(&flags.ExplorePort, "explore-port", 8080, "Port for explorer web UI")
```

Add to `MergeConfig` and `Config` struct as needed.

Add handler in `main()` after the other flag handlers:

```go
// Handle --explore: start log explorer
if cfg.Explore {
    home, _ := os.UserHomeDir()
    logDir := cfg.LogDir
    if logDir == "" {
        logDir = filepath.Join(home, ".llm-provider-logs")
    }

    explorer := NewExplorer(logDir)

    addr := fmt.Sprintf(":%d", cfg.ExplorePort)
    log.Printf("Starting LLM Proxy Explorer on http://localhost%s", addr)

    if err := http.ListenAndServe(addr, explorer); err != nil {
        log.Fatalf("Explorer server error: %v", err)
    }
    os.Exit(0)
}
```

### Step 7: Run full test suite

```bash
go test -v ./...
```

### Step 8: Commit

```bash
git add explorer.go explorer_test.go templates/ static/ main.go config.go
git commit -m "feat: add --explore flag with basic session list UI

Adds web-based log explorer with session listing grouped by date.
Templates use dark theme, search box in nav (not yet functional)."
```

---

## Task 3: Session List Enhancements

**Files:**
- Modify: `explorer.go` (add message counts, time ranges)
- Modify: `explorer_test.go` (add tests)
- Modify: `templates/home.html` (show more info)

### Step 1: Write failing test for session metadata

Add to `explorer_test.go`:

```go
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
```

### Step 2: Run test to verify it fails

```bash
go test -v -run TestSessionInfoIncludesMessageCount ./...
```

Expected: FAIL (MessageCount field doesn't exist)

### Step 3: Add metadata parsing to listSessions

Update `SessionInfo` struct:

```go
type SessionInfo struct {
	ID           string
	Host         string
	Date         string
	Path         string
	ModTime      time.Time
	MessageCount int
	TimeRange    string
	FirstTime    time.Time
	LastTime     time.Time
}
```

Update `listSessions` to parse each file and count messages:

```go
func (e *Explorer) listSessions() []SessionInfo {
	var sessions []SessionInfo

	filepath.Walk(e.logDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}

		rel, _ := filepath.Rel(e.logDir, path)
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 3 {
			return nil
		}

		session := SessionInfo{
			ID:      strings.TrimSuffix(parts[2], ".jsonl"),
			Host:    parts[0],
			Date:    parts[1],
			Path:    path,
			ModTime: info.ModTime(),
		}

		// Parse file for metadata
		e.parseSessionMetadata(&session)

		sessions = append(sessions, session)
		return nil
	})

	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Date != sessions[j].Date {
			return sessions[i].Date > sessions[j].Date
		}
		return sessions[i].ModTime.After(sessions[j].ModTime)
	})

	return sessions
}

func (e *Explorer) parseSessionMetadata(session *SessionInfo) {
	data, err := os.ReadFile(session.Path)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	var firstTs, lastTs time.Time
	msgCount := 0

	for _, line := range lines {
		if line == "" {
			continue
		}

		var entry map[string]interface{}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}

		// Count request/response pairs as messages
		if entry["type"] == "request" {
			msgCount++
		}

		// Extract timestamp
		if meta, ok := entry["_meta"].(map[string]interface{}); ok {
			if tsStr, ok := meta["ts"].(string); ok {
				if ts, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
					if firstTs.IsZero() || ts.Before(firstTs) {
						firstTs = ts
					}
					if ts.After(lastTs) {
						lastTs = ts
					}
				}
			}
		}
	}

	session.MessageCount = msgCount
	session.FirstTime = firstTs
	session.LastTime = lastTs

	if !firstTs.IsZero() && !lastTs.IsZero() {
		session.TimeRange = fmt.Sprintf("%s - %s",
			firstTs.Format("15:04"),
			lastTs.Format("15:04"))
	}
}
```

Add `"encoding/json"` to imports if not present.

### Step 4: Run test to verify it passes

```bash
go test -v -run TestSessionInfoIncludesMessageCount ./...
```

Expected: PASS

### Step 5: Update home template

Update `templates/home.html` to show the new fields:

```html
{{range .Sessions}}
    {{if ne .Date $currentDate}}
        {{$currentDate = .Date}}
        <h3>{{.Date}}</h3>
    {{end}}
    <div class="session">
        <a href="/session/{{.ID}}">{{.ID}}</a>
        <span class="host">{{.Host}}</span>
        <span class="count">{{.MessageCount}} msgs</span>
        <span class="time">{{.TimeRange}}</span>
    </div>
{{end}}
```

Add CSS for new elements:

```css
.session .count, .session .time {
    color: var(--text-muted);
    font-size: 0.85rem;
}
```

### Step 6: Run full tests and manual verification

```bash
go test -v ./...
go build && ./llm-proxy --explore
```

Open http://localhost:8080 and verify session list shows counts and times.

### Step 7: Commit

```bash
git add explorer.go explorer_test.go templates/home.html static/style.css
git commit -m "feat: show message counts and time ranges in session list"
```

---

## Task 4: Session Detail View - Basic Structure

**Files:**
- Modify: `explorer.go` (implement handleSession)
- Modify: `templates/session.html` (conversation layout)
- Add to: `explorer_test.go`

### Step 1: Write failing test for session detail

Add to `explorer_test.go`:

```go
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
```

### Step 2: Run test to verify it fails

```bash
go test -v -run TestSessionDetailShowsEntries ./...
```

Expected: FAIL (returns 501 Not Implemented)

### Step 3: Implement handleSession

Add types for parsed entries:

```go
type LogEntry struct {
	Type     string
	Seq      int
	Body     string
	Headers  map[string][]string
	Status   int
	Meta     EntryMeta
	Raw      string // Original JSON line
}

type EntryMeta struct {
	Timestamp time.Time
	Machine   string
	Host      string
	Session   string
}

type ConversationTurn struct {
	Request  *LogEntry
	Response *LogEntry
}
```

Implement `handleSession`:

```go
func (e *Explorer) handleSession(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/session/")
	if sessionID == "" {
		http.Error(w, "Session ID required", http.StatusBadRequest)
		return
	}

	// Find the session file
	sessionPath := e.findSessionFile(sessionID)
	if sessionPath == "" {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	entries, err := e.parseSessionFile(sessionPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Group into conversation turns
	turns := e.groupIntoTurns(entries)

	e.templates.ExecuteTemplate(w, "session.html", map[string]interface{}{
		"SessionID": sessionID,
		"Entries":   entries,
		"Turns":     turns,
	})
}

func (e *Explorer) findSessionFile(sessionID string) string {
	var found string
	filepath.Walk(e.logDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(info.Name(), sessionID+".jsonl") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func (e *Explorer) parseSessionFile(path string) ([]LogEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var entries []LogEntry
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}

		var raw map[string]interface{}
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}

		entry := LogEntry{
			Raw: line,
		}

		if t, ok := raw["type"].(string); ok {
			entry.Type = t
		}
		if s, ok := raw["seq"].(float64); ok {
			entry.Seq = int(s)
		}
		if b, ok := raw["body"].(string); ok {
			entry.Body = b
		}
		if s, ok := raw["status"].(float64); ok {
			entry.Status = int(s)
		}

		if meta, ok := raw["_meta"].(map[string]interface{}); ok {
			if ts, ok := meta["ts"].(string); ok {
				entry.Meta.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
			}
			if m, ok := meta["machine"].(string); ok {
				entry.Meta.Machine = m
			}
			if h, ok := meta["host"].(string); ok {
				entry.Meta.Host = h
			}
			if s, ok := meta["session"].(string); ok {
				entry.Meta.Session = s
			}
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

func (e *Explorer) groupIntoTurns(entries []LogEntry) []ConversationTurn {
	var turns []ConversationTurn
	turnMap := make(map[int]*ConversationTurn)

	for i := range entries {
		entry := &entries[i]
		if entry.Type == "request" {
			turn := &ConversationTurn{Request: entry}
			turnMap[entry.Seq] = turn
			turns = append(turns, *turn)
		} else if entry.Type == "response" {
			if turn, ok := turnMap[entry.Seq]; ok {
				turn.Response = entry
				// Update in slice
				for j := range turns {
					if turns[j].Request != nil && turns[j].Request.Seq == entry.Seq {
						turns[j].Response = entry
						break
					}
				}
			}
		}
	}

	return turns
}
```

### Step 4: Update session template

Replace `templates/session.html`:

```html
<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>Session {{.SessionID}} - LLM Proxy Explorer</title>
    <link rel="stylesheet" href="/static/style.css">
</head>
<body>
    <nav>
        <a href="/">LLM Proxy Explorer</a>
        <form action="/search" method="get">
            <input type="text" name="q" placeholder="Search logs...">
            <button type="submit">Search</button>
        </form>
    </nav>
    <main>
        <h2>Session: <code>{{.SessionID}}</code></h2>

        {{range .Turns}}
        <div class="turn">
            {{if .Request}}
            <div class="message request">
                <div class="role">Request #{{.Request.Seq}}</div>
                <div class="content">
                    <pre>{{.Request.Body}}</pre>
                </div>
                <details>
                    <summary>Raw JSON</summary>
                    <pre class="raw">{{.Request.Raw}}</pre>
                </details>
            </div>
            {{end}}

            {{if .Response}}
            <div class="message response">
                <div class="role">Response #{{.Response.Seq}} ({{.Response.Status}})</div>
                <div class="content">
                    <pre>{{.Response.Body}}</pre>
                </div>
                <details>
                    <summary>Raw JSON</summary>
                    <pre class="raw">{{.Response.Raw}}</pre>
                </details>
            </div>
            {{end}}
        </div>
        {{end}}
    </main>
</body>
</html>
```

Add CSS for messages:

```css
.turn {
    margin: 2rem 0;
    border: 1px solid var(--border);
    border-radius: 8px;
    overflow: hidden;
}

.message {
    padding: 1rem;
}

.message.request {
    background: var(--bg-secondary);
}

.message.response {
    background: var(--bg);
    border-top: 1px solid var(--border);
}

.message .role {
    font-weight: bold;
    margin-bottom: 0.5rem;
    color: var(--accent);
}

.message .content pre {
    white-space: pre-wrap;
    word-break: break-word;
    background: rgba(0,0,0,0.2);
    padding: 1rem;
    border-radius: 4px;
    overflow-x: auto;
}

details {
    margin-top: 0.5rem;
}

summary {
    cursor: pointer;
    color: var(--text-muted);
}

.raw {
    font-size: 0.8rem;
    color: var(--text-muted);
}
```

### Step 5: Run test to verify it passes

```bash
go test -v -run TestSessionDetailShowsEntries ./...
```

Expected: PASS

### Step 6: Commit

```bash
git add explorer.go explorer_test.go templates/session.html static/style.css
git commit -m "feat: add session detail view with request/response pairs"
```

---

## Task 5: Conversation Rendering - Parse Message Content

**Files:**
- Modify: `explorer.go` (add content parsing)
- Create: `parser.go` (Claude/OpenAI message parsing)
- Create: `parser_test.go`
- Modify: `templates/session.html`

### Step 1: Write failing test for message parsing

Create `parser_test.go`:

```go
package main

import (
	"testing"
)

func TestParseClaudeRequest(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-20250514",
		"max_tokens": 8096,
		"messages": [
			{"role": "user", "content": "What is 2+2?"}
		]
	}`

	parsed := ParseRequestBody(body, "api.anthropic.com")

	if parsed.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Expected claude-sonnet-4-20250514, got %s", parsed.Model)
	}

	if len(parsed.Messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(parsed.Messages))
	}

	if parsed.Messages[0].Role != "user" {
		t.Errorf("Expected role user, got %s", parsed.Messages[0].Role)
	}

	if parsed.Messages[0].TextContent != "What is 2+2?" {
		t.Errorf("Expected 'What is 2+2?', got %s", parsed.Messages[0].TextContent)
	}
}

func TestParseClaudeResponse(t *testing.T) {
	body := `{
		"content": [
			{"type": "text", "text": "2+2 equals 4."}
		],
		"usage": {"input_tokens": 10, "output_tokens": 8}
	}`

	parsed := ParseResponseBody(body, "api.anthropic.com")

	if len(parsed.Content) != 1 {
		t.Fatalf("Expected 1 content block, got %d", len(parsed.Content))
	}

	if parsed.Content[0].Type != "text" {
		t.Errorf("Expected type text, got %s", parsed.Content[0].Type)
	}

	if parsed.Content[0].Text != "2+2 equals 4." {
		t.Errorf("Expected '2+2 equals 4.', got %s", parsed.Content[0].Text)
	}
}

func TestParseClaudeThinkingBlock(t *testing.T) {
	body := `{
		"content": [
			{"type": "thinking", "thinking": "Let me calculate this step by step..."},
			{"type": "text", "text": "The answer is 4."}
		]
	}`

	parsed := ParseResponseBody(body, "api.anthropic.com")

	if len(parsed.Content) != 2 {
		t.Fatalf("Expected 2 content blocks, got %d", len(parsed.Content))
	}

	if parsed.Content[0].Type != "thinking" {
		t.Errorf("Expected thinking block first")
	}

	if parsed.Content[0].Thinking != "Let me calculate this step by step..." {
		t.Error("Thinking content not parsed correctly")
	}
}

func TestParseClaudeToolUse(t *testing.T) {
	body := `{
		"content": [
			{"type": "text", "text": "I'll read that file."},
			{"type": "tool_use", "id": "tool_123", "name": "Read", "input": {"path": "/tmp/test.txt"}}
		]
	}`

	parsed := ParseResponseBody(body, "api.anthropic.com")

	if len(parsed.Content) != 2 {
		t.Fatalf("Expected 2 content blocks, got %d", len(parsed.Content))
	}

	toolBlock := parsed.Content[1]
	if toolBlock.Type != "tool_use" {
		t.Errorf("Expected tool_use, got %s", toolBlock.Type)
	}

	if toolBlock.ToolName != "Read" {
		t.Errorf("Expected tool name Read, got %s", toolBlock.ToolName)
	}
}
```

### Step 2: Run test to verify it fails

```bash
go test -v -run TestParse ./...
```

Expected: FAIL (ParseRequestBody undefined)

### Step 3: Implement parser.go

Create `parser.go`:

```go
package main

import (
	"encoding/json"
)

type ParsedRequest struct {
	Model       string
	MaxTokens   int
	System      string
	Messages    []ParsedMessage
	Raw         map[string]interface{}
}

type ParsedMessage struct {
	Role        string
	TextContent string
	Content     []ContentBlock
	Raw         map[string]interface{}
}

type ParsedResponse struct {
	Content     []ContentBlock
	Usage       UsageInfo
	StopReason  string
	Raw         map[string]interface{}
}

type ContentBlock struct {
	Type      string
	Text      string
	Thinking  string
	ToolID    string
	ToolName  string
	ToolInput map[string]interface{}
	Raw       map[string]interface{}
}

type UsageInfo struct {
	InputTokens  int
	OutputTokens int
}

func ParseRequestBody(body string, host string) ParsedRequest {
	var raw map[string]interface{}
	if json.Unmarshal([]byte(body), &raw) != nil {
		return ParsedRequest{Raw: raw}
	}

	parsed := ParsedRequest{Raw: raw}

	if model, ok := raw["model"].(string); ok {
		parsed.Model = model
	}
	if maxTokens, ok := raw["max_tokens"].(float64); ok {
		parsed.MaxTokens = int(maxTokens)
	}
	if system, ok := raw["system"].(string); ok {
		parsed.System = system
	}

	if messages, ok := raw["messages"].([]interface{}); ok {
		for _, m := range messages {
			if msg, ok := m.(map[string]interface{}); ok {
				pm := ParsedMessage{Raw: msg}

				if role, ok := msg["role"].(string); ok {
					pm.Role = role
				}

				// Handle simple string content
				if content, ok := msg["content"].(string); ok {
					pm.TextContent = content
				}

				// Handle array content (tool results, etc)
				if content, ok := msg["content"].([]interface{}); ok {
					for _, c := range content {
						if block, ok := c.(map[string]interface{}); ok {
							pm.Content = append(pm.Content, parseContentBlock(block))
						}
					}
					// Set TextContent from first text block for convenience
					for _, cb := range pm.Content {
						if cb.Type == "text" && pm.TextContent == "" {
							pm.TextContent = cb.Text
						}
					}
				}

				parsed.Messages = append(parsed.Messages, pm)
			}
		}
	}

	return parsed
}

func ParseResponseBody(body string, host string) ParsedResponse {
	var raw map[string]interface{}
	if json.Unmarshal([]byte(body), &raw) != nil {
		return ParsedResponse{Raw: raw}
	}

	parsed := ParsedResponse{Raw: raw}

	if content, ok := raw["content"].([]interface{}); ok {
		for _, c := range content {
			if block, ok := c.(map[string]interface{}); ok {
				parsed.Content = append(parsed.Content, parseContentBlock(block))
			}
		}
	}

	if usage, ok := raw["usage"].(map[string]interface{}); ok {
		if in, ok := usage["input_tokens"].(float64); ok {
			parsed.Usage.InputTokens = int(in)
		}
		if out, ok := usage["output_tokens"].(float64); ok {
			parsed.Usage.OutputTokens = int(out)
		}
	}

	if stop, ok := raw["stop_reason"].(string); ok {
		parsed.StopReason = stop
	}

	return parsed
}

func parseContentBlock(block map[string]interface{}) ContentBlock {
	cb := ContentBlock{Raw: block}

	if t, ok := block["type"].(string); ok {
		cb.Type = t
	}

	switch cb.Type {
	case "text":
		if text, ok := block["text"].(string); ok {
			cb.Text = text
		}
	case "thinking":
		if thinking, ok := block["thinking"].(string); ok {
			cb.Thinking = thinking
		}
	case "tool_use":
		if id, ok := block["id"].(string); ok {
			cb.ToolID = id
		}
		if name, ok := block["name"].(string); ok {
			cb.ToolName = name
		}
		if input, ok := block["input"].(map[string]interface{}); ok {
			cb.ToolInput = input
		}
	case "tool_result":
		if id, ok := block["tool_use_id"].(string); ok {
			cb.ToolID = id
		}
		if content, ok := block["content"].(string); ok {
			cb.Text = content
		}
	}

	return cb
}
```

### Step 4: Run test to verify it passes

```bash
go test -v -run TestParse ./...
```

Expected: PASS

### Step 5: Commit

```bash
git add parser.go parser_test.go
git commit -m "feat: add parser for Claude request/response bodies

Handles text, thinking blocks, tool_use, and tool_result content types."
```

---

## Task 6: Conversation Rendering - Update Templates

**Files:**
- Modify: `explorer.go` (use parser in handleSession)
- Modify: `templates/session.html` (render parsed content)
- Add to: `static/style.css`

### Step 1: Update explorer to use parser

Modify `handleSession` to parse bodies:

```go
type ParsedTurn struct {
	Seq      int
	Request  *LogEntry
	Response *LogEntry
	ReqParsed  ParsedRequest
	RespParsed ParsedResponse
}

func (e *Explorer) handleSession(w http.ResponseWriter, r *http.Request) {
	// ... existing code to find and parse file ...

	// Get host from first entry
	host := ""
	for _, entry := range entries {
		if entry.Meta.Host != "" {
			host = entry.Meta.Host
			break
		}
	}

	// Group and parse
	turns := e.groupAndParseTurns(entries, host)

	e.templates.ExecuteTemplate(w, "session.html", map[string]interface{}{
		"SessionID": sessionID,
		"Host":      host,
		"Turns":     turns,
	})
}

func (e *Explorer) groupAndParseTurns(entries []LogEntry, host string) []ParsedTurn {
	var turns []ParsedTurn
	turnMap := make(map[int]*ParsedTurn)

	for i := range entries {
		entry := &entries[i]
		if entry.Type == "request" {
			turn := &ParsedTurn{
				Seq:       entry.Seq,
				Request:   entry,
				ReqParsed: ParseRequestBody(entry.Body, host),
			}
			turnMap[entry.Seq] = turn
			turns = append(turns, *turn)
		} else if entry.Type == "response" {
			if turn, ok := turnMap[entry.Seq]; ok {
				turn.Response = entry
				turn.RespParsed = ParseResponseBody(entry.Body, host)
				// Update in slice
				for j := range turns {
					if turns[j].Seq == entry.Seq {
						turns[j].Response = entry
						turns[j].RespParsed = turn.RespParsed
						break
					}
				}
			}
		}
	}

	return turns
}
```

### Step 2: Update session template for rich rendering

Replace `templates/session.html`:

```html
<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>Session {{.SessionID}} - LLM Proxy Explorer</title>
    <link rel="stylesheet" href="/static/style.css">
</head>
<body>
    <nav>
        <a href="/">LLM Proxy Explorer</a>
        <form action="/search" method="get">
            <input type="text" name="q" placeholder="Search logs...">
            <button type="submit">Search</button>
        </form>
    </nav>
    <main>
        <header class="session-header">
            <h2>Session: <code>{{.SessionID}}</code></h2>
            <span class="host">{{.Host}}</span>
        </header>

        {{range .Turns}}
        <div class="turn">
            <!-- Request: User messages -->
            {{if .Request}}
            <div class="message user">
                <div class="meta">
                    <span class="role">User</span>
                    <span class="model">{{.ReqParsed.Model}}</span>
                    <span class="seq">#{{.Seq}}</span>
                </div>

                {{if .ReqParsed.System}}
                <div class="system-prompt">
                    <strong>System:</strong>
                    <pre>{{.ReqParsed.System}}</pre>
                </div>
                {{end}}

                {{range .ReqParsed.Messages}}
                    {{if eq .Role "user"}}
                    <div class="content user-content">
                        {{if .TextContent}}
                            <pre class="text">{{.TextContent}}</pre>
                        {{end}}
                        {{range .Content}}
                            {{if eq .Type "tool_result"}}
                            <div class="tool-result">
                                <div class="tool-header">Tool Result ({{.ToolID}})</div>
                                <pre>{{.Text}}</pre>
                            </div>
                            {{end}}
                        {{end}}
                    </div>
                    {{end}}
                {{end}}

                <details>
                    <summary>Raw Request</summary>
                    <pre class="raw">{{.Request.Raw}}</pre>
                </details>
            </div>
            {{end}}

            <!-- Response: Assistant content -->
            {{if .Response}}
            <div class="message assistant">
                <div class="meta">
                    <span class="role">Assistant</span>
                    {{if .RespParsed.Usage.OutputTokens}}
                    <span class="tokens">{{.RespParsed.Usage.InputTokens}} in / {{.RespParsed.Usage.OutputTokens}} out</span>
                    {{end}}
                </div>

                {{range .RespParsed.Content}}
                    {{if eq .Type "thinking"}}
                    <details class="thinking-block">
                        <summary>Thinking...</summary>
                        <pre class="thinking">{{.Thinking}}</pre>
                    </details>
                    {{else if eq .Type "text"}}
                    <div class="content assistant-content">
                        <pre class="text">{{.Text}}</pre>
                    </div>
                    {{else if eq .Type "tool_use"}}
                    <div class="tool-call">
                        <div class="tool-header">🔧 {{.ToolName}}</div>
                        <pre class="tool-input">{{printf "%v" .ToolInput}}</pre>
                    </div>
                    {{end}}
                {{end}}

                <details>
                    <summary>Raw Response</summary>
                    <pre class="raw">{{.Response.Raw}}</pre>
                </details>
            </div>
            {{end}}
        </div>
        {{end}}
    </main>
</body>
</html>
```

### Step 3: Add CSS for conversation styling

Add to `static/style.css`:

```css
.session-header {
    display: flex;
    align-items: baseline;
    gap: 1rem;
    margin-bottom: 2rem;
}

.session-header .host {
    color: var(--text-muted);
}

.turn {
    margin: 1.5rem 0;
    border-radius: 8px;
    overflow: hidden;
    border: 1px solid var(--border);
}

.message {
    padding: 1rem 1.5rem;
}

.message.user {
    background: var(--bg-secondary);
}

.message.assistant {
    background: var(--bg);
    border-top: 1px solid var(--border);
}

.message .meta {
    display: flex;
    gap: 1rem;
    align-items: center;
    margin-bottom: 1rem;
    font-size: 0.9rem;
}

.message .role {
    font-weight: bold;
    color: var(--accent);
}

.message .model, .message .seq, .message .tokens {
    color: var(--text-muted);
}

.content pre.text {
    white-space: pre-wrap;
    word-break: break-word;
    margin: 0;
    font-family: system-ui, sans-serif;
    line-height: 1.6;
}

.system-prompt {
    background: rgba(255, 200, 0, 0.1);
    border-left: 3px solid #fc0;
    padding: 0.5rem 1rem;
    margin-bottom: 1rem;
}

.system-prompt pre {
    margin: 0.5rem 0 0 0;
    white-space: pre-wrap;
}

.thinking-block {
    background: rgba(100, 100, 100, 0.1);
    border-left: 3px solid #888;
    margin: 0.5rem 0;
}

.thinking-block summary {
    padding: 0.5rem 1rem;
    cursor: pointer;
    color: var(--text-muted);
    font-style: italic;
}

.thinking-block pre.thinking {
    padding: 0.5rem 1rem;
    margin: 0;
    white-space: pre-wrap;
    color: var(--text-muted);
    font-style: italic;
}

.tool-call, .tool-result {
    background: rgba(74, 158, 255, 0.1);
    border-left: 3px solid var(--accent);
    margin: 0.5rem 0;
    padding: 0.5rem 1rem;
}

.tool-header {
    font-weight: bold;
    margin-bottom: 0.5rem;
}

.tool-input, .tool-result pre {
    margin: 0;
    font-size: 0.85rem;
    white-space: pre-wrap;
}

details summary {
    cursor: pointer;
    color: var(--text-muted);
    font-size: 0.85rem;
    margin-top: 1rem;
}

pre.raw {
    font-size: 0.75rem;
    color: var(--text-muted);
    white-space: pre-wrap;
    word-break: break-all;
    max-height: 300px;
    overflow-y: auto;
}
```

### Step 4: Run tests

```bash
go test -v ./...
```

### Step 5: Manual verification

```bash
go build && ./llm-proxy --explore
```

Browse to a session with thinking blocks and tool calls to verify rendering.

### Step 6: Commit

```bash
git add explorer.go templates/session.html static/style.css
git commit -m "feat: render conversations with thinking blocks and tool calls

- Thinking blocks collapsed by default
- Tool use shows tool name and input
- Tool results shown in user messages
- Token usage displayed
- Raw JSON available via details toggle"
```

---

## Task 7: Full-Text Search

**Files:**
- Modify: `explorer.go` (implement handleSearch)
- Modify: `templates/search.html`
- Add to: `explorer_test.go`

### Step 1: Write failing test for search

Add to `explorer_test.go`:

```go
func TestSearchFindsMatchingContent(t *testing.T) {
	tmpDir := t.TempDir()

	sessionDir := filepath.Join(tmpDir, "api.anthropic.com", "2026-01-14")
	os.MkdirAll(sessionDir, 0755)

	content := `{"type":"session_start","_meta":{"ts":"2026-01-14T10:00:00Z"}}
{"type":"request","body":"{\"messages\":[{\"content\":\"Tell me about quantum computing\"}]}","_meta":{"ts":"2026-01-14T10:00:01Z"}}
{"type":"response","body":"{\"content\":[{\"text\":\"Quantum computing uses qubits...\"}]}","_meta":{"ts":"2026-01-14T10:00:02Z"}}
`
	os.WriteFile(filepath.Join(sessionDir, "quantum-session.jsonl"), []byte(content), 0644)

	// Another session without the search term
	content2 := `{"type":"session_start","_meta":{"ts":"2026-01-14T11:00:00Z"}}
{"type":"request","body":"{\"messages\":[{\"content\":\"Hello world\"}]}","_meta":{"ts":"2026-01-14T11:00:01Z"}}
`
	os.WriteFile(filepath.Join(sessionDir, "hello-session.jsonl"), []byte(content2), 0644)

	explorer := NewExplorer(tmpDir)

	req := httptest.NewRequest("GET", "/search?q=quantum", nil)
	w := httptest.NewRecorder()

	explorer.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "quantum-session") {
		t.Error("Expected search results to include quantum-session")
	}
	if strings.Contains(body, "hello-session") {
		t.Error("Did not expect hello-session in results")
	}
}
```

### Step 2: Run test to verify it fails

```bash
go test -v -run TestSearchFindsMatchingContent ./...
```

Expected: FAIL (returns 501 Not Implemented)

### Step 3: Implement search

Add types and implement `handleSearch`:

```go
type SearchResult struct {
	SessionID   string
	Host        string
	Date        string
	LineNumber  int
	Line        string
	Context     string // Surrounding text
	MatchStart  int
	MatchEnd    int
}

func (e *Explorer) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		e.templates.ExecuteTemplate(w, "search.html", map[string]interface{}{
			"Query":   "",
			"Results": nil,
		})
		return
	}

	results := e.search(query, 100) // Limit to 100 results

	e.templates.ExecuteTemplate(w, "search.html", map[string]interface{}{
		"Query":   query,
		"Results": results,
		"Count":   len(results),
	})
}

func (e *Explorer) search(query string, limit int) []SearchResult {
	var results []SearchResult
	queryLower := strings.ToLower(query)

	filepath.Walk(e.logDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}

		if len(results) >= limit {
			return filepath.SkipAll
		}

		rel, _ := filepath.Rel(e.logDir, path)
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 3 {
			return nil
		}

		host := parts[0]
		date := parts[1]
		sessionID := strings.TrimSuffix(parts[2], ".jsonl")

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if strings.Contains(strings.ToLower(line), queryLower) {
				// Find match position for highlighting
				matchStart := strings.Index(strings.ToLower(line), queryLower)
				matchEnd := matchStart + len(query)

				// Extract context (truncate long lines)
				context := line
				if len(context) > 200 {
					start := matchStart - 50
					if start < 0 {
						start = 0
					}
					end := matchStart + len(query) + 150
					if end > len(line) {
						end = len(line)
					}
					context = "..." + line[start:end] + "..."
					matchStart = matchStart - start + 3
					matchEnd = matchStart + len(query)
				}

				results = append(results, SearchResult{
					SessionID:  sessionID,
					Host:       host,
					Date:       date,
					LineNumber: i + 1,
					Line:       line,
					Context:    context,
					MatchStart: matchStart,
					MatchEnd:   matchEnd,
				})

				if len(results) >= limit {
					return filepath.SkipAll
				}
			}
		}

		return nil
	})

	return results
}
```

### Step 4: Update search template

Replace `templates/search.html`:

```html
<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>Search - LLM Proxy Explorer</title>
    <link rel="stylesheet" href="/static/style.css">
</head>
<body>
    <nav>
        <a href="/">LLM Proxy Explorer</a>
        <form action="/search" method="get">
            <input type="text" name="q" value="{{.Query}}" placeholder="Search logs...">
            <button type="submit">Search</button>
        </form>
    </nav>
    <main>
        <h2>Search Results</h2>

        {{if .Query}}
            {{if .Results}}
            <p class="result-count">Found {{.Count}} results for "{{.Query}}"</p>

            {{range .Results}}
            <div class="search-result">
                <div class="result-header">
                    <a href="/session/{{.SessionID}}">{{.SessionID}}</a>
                    <span class="host">{{.Host}}</span>
                    <span class="date">{{.Date}}</span>
                    <span class="line-num">Line {{.LineNumber}}</span>
                </div>
                <pre class="result-context">{{.Context}}</pre>
            </div>
            {{end}}
            {{else}}
            <p>No results found for "{{.Query}}"</p>
            {{end}}
        {{else}}
        <p>Enter a search term above.</p>
        {{end}}
    </main>
</body>
</html>
```

Add CSS:

```css
.result-count {
    color: var(--text-muted);
    margin-bottom: 1.5rem;
}

.search-result {
    margin: 1rem 0;
    padding: 1rem;
    background: var(--bg-secondary);
    border-radius: 8px;
    border: 1px solid var(--border);
}

.result-header {
    display: flex;
    gap: 1rem;
    align-items: center;
    margin-bottom: 0.5rem;
    flex-wrap: wrap;
}

.result-header a {
    color: var(--accent);
    font-family: monospace;
}

.result-header .host, .result-header .date, .result-header .line-num {
    color: var(--text-muted);
    font-size: 0.85rem;
}

.result-context {
    margin: 0;
    white-space: pre-wrap;
    word-break: break-word;
    font-size: 0.85rem;
    background: rgba(0,0,0,0.2);
    padding: 0.75rem;
    border-radius: 4px;
    overflow-x: auto;
}
```

### Step 5: Run test to verify it passes

```bash
go test -v -run TestSearchFindsMatchingContent ./...
```

Expected: PASS

### Step 6: Run full test suite

```bash
go test -v ./...
```

### Step 7: Commit

```bash
git add explorer.go explorer_test.go templates/search.html static/style.css
git commit -m "feat: add full-text search across all log files

- Case-insensitive substring search
- Results show context with session link
- Limited to 100 results for performance"
```

---

## Task 8: Polish and Integration

**Files:**
- Modify: `main.go` (auto-open browser)
- Modify: `explorer.go` (filter support)
- Modify: `templates/home.html` (host filter dropdown)
- Run full test suite

### Step 1: Add browser auto-open

Modify `main.go` where `--explore` is handled:

```go
import (
	"os/exec"
	"runtime"
)

// In the --explore handler:
if cfg.Explore {
    // ... existing setup code ...

    url := fmt.Sprintf("http://localhost:%d", cfg.ExplorePort)
    log.Printf("Starting LLM Proxy Explorer on %s", url)

    // Auto-open browser (best effort, don't fail if it doesn't work)
    go func() {
        time.Sleep(100 * time.Millisecond) // Wait for server to start
        openBrowser(url)
    }()

    if err := http.ListenAndServe(addr, explorer); err != nil {
        log.Fatalf("Explorer server error: %v", err)
    }
    os.Exit(0)
}

func openBrowser(url string) {
    var cmd *exec.Cmd
    switch runtime.GOOS {
    case "darwin":
        cmd = exec.Command("open", url)
    case "linux":
        cmd = exec.Command("xdg-open", url)
    default:
        return
    }
    cmd.Start()
}
```

### Step 2: Add host filter to session list

Update `handleHome` to accept filter parameter:

```go
func (e *Explorer) handleHome(w http.ResponseWriter, r *http.Request) {
    if r.URL.Path != "/" {
        http.NotFound(w, r)
        return
    }

    filter := r.URL.Query().Get("host")
    sessions := e.listSessions()

    // Get unique hosts for filter dropdown
    hostSet := make(map[string]bool)
    for _, s := range sessions {
        hostSet[s.Host] = true
    }
    var hosts []string
    for h := range hostSet {
        hosts = append(hosts, h)
    }
    sort.Strings(hosts)

    // Apply filter
    if filter != "" {
        var filtered []SessionInfo
        for _, s := range sessions {
            if s.Host == filter {
                filtered = append(filtered, s)
            }
        }
        sessions = filtered
    }

    e.templates.ExecuteTemplate(w, "home.html", map[string]interface{}{
        "Sessions":     sessions,
        "Hosts":        hosts,
        "CurrentHost":  filter,
    })
}
```

Update `templates/home.html` to include filter:

```html
<main>
    <div class="filters">
        <form method="get" action="/">
            <label>Filter by host:</label>
            <select name="host" onchange="this.form.submit()">
                <option value="">All</option>
                {{range .Hosts}}
                <option value="{{.}}" {{if eq . $.CurrentHost}}selected{{end}}>{{.}}</option>
                {{end}}
            </select>
        </form>
    </div>

    <h2>Sessions</h2>
    <!-- ... rest of template ... -->
</main>
```

Add CSS:

```css
.filters {
    margin-bottom: 1.5rem;
}

.filters select {
    padding: 0.5rem;
    background: var(--bg-secondary);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 4px;
}
```

### Step 3: Run full test suite

```bash
go test -v ./...
```

### Step 4: Manual end-to-end test

```bash
go build
./llm-proxy --explore
```

Verify:
- Browser opens automatically
- Session list shows with dates, hosts, message counts
- Host filter works
- Clicking session shows conversation with thinking/tools
- Search finds content across sessions

### Step 5: Commit

```bash
git add main.go explorer.go templates/home.html static/style.css
git commit -m "feat: add browser auto-open and host filtering

- Browser opens automatically on --explore
- Filter sessions by host via dropdown
- Completes log explorer feature"
```

---

## Task 9: Update README and Tests

**Files:**
- Modify: `README.md`
- Ensure all tests pass

### Step 1: Update README

Add section to README.md:

```markdown
## Log Explorer

Browse and search your LLM logs with a web UI:

```bash
llm-proxy --explore              # Opens browser to http://localhost:8080
llm-proxy --explore --port 9000  # Use specific port
```

Features:
- Session list grouped by date with message counts
- Filter by provider (Anthropic, OpenAI, etc.)
- Conversation view with thinking blocks and tool calls
- Full-text search across all logs
- Raw JSON view for debugging
```

### Step 2: Run full test suite

```bash
go test -v ./...
```

All tests should pass.

### Step 3: Commit

```bash
git add README.md
git commit -m "docs: add log explorer section to README"
```

---

## Summary

**Total tasks:** 9

**New files created:**
- `explorer.go` - HTTP server and handlers
- `explorer_test.go` - Tests
- `parser.go` - Claude/OpenAI message parsing
- `parser_test.go` - Parser tests
- `templates/home.html`
- `templates/session.html`
- `templates/search.html`
- `static/style.css`

**Modified files:**
- `logger.go` - Add `_meta` block with machine, host, session, ts
- `logger_test.go` - Update for new format
- `main.go` - Add `--explore` flag
- `config.go` - Add Explore config
- `README.md` - Document explorer

**Key features:**
- `_meta` block in all log entries for aggregation
- Machine identifier as `user@hostname`
- Web UI with session list, detail view, search
- Conversation rendering with thinking blocks and tool calls
- Full-text search across all log files

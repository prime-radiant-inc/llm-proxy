package main

import (
	"embed"
	"encoding/json"
	"fmt"
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

type LogEntry struct {
	Type    string
	Seq     int
	Body    string
	Headers map[string][]string
	Status  int
	Meta    EntryMeta
	Raw     string // Original JSON line
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

type SearchResult struct {
	SessionID  string
	Host       string
	Date       string
	LineNumber int
	Line       string
	Context    string
	MatchStart int
	MatchEnd   int
}

type ParsedTurn struct {
	Seq        int
	Request    *LogEntry
	Response   *LogEntry
	ReqParsed  ParsedRequest
	RespParsed ParsedResponse
	IsUtility  bool // True for utility requests (e.g., Haiku labeling)
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
		"Sessions":    sessions,
		"Hosts":       hosts,
		"CurrentHost": filter,
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

		session := SessionInfo{
			ID:      strings.TrimSuffix(parts[2], ".jsonl"),
			Host:    parts[0],
			Date:    parts[1],
			Path:    path,
			ModTime: info.ModTime(),
		}
		e.parseSessionMetadata(&session)
		sessions = append(sessions, session)
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

		// Count request entries as messages
		if entry["type"] == "request" {
			msgCount++
		}

		// Extract timestamp from _meta
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

	// Get host from first entry that has it
	host := ""
	for _, entry := range entries {
		if entry.Meta.Host != "" {
			host = entry.Meta.Host
			break
		}
	}

	// Group and parse into conversation turns
	turns := e.groupAndParseTurns(entries, host)

	e.templates.ExecuteTemplate(w, "session.html", map[string]interface{}{
		"SessionID": sessionID,
		"Host":      host,
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

func (e *Explorer) groupAndParseTurns(entries []LogEntry, host string) []ParsedTurn {
	var turns []ParsedTurn
	turnMap := make(map[int]*ParsedTurn)

	for i := range entries {
		entry := &entries[i]
		if entry.Type == "request" {
			reqParsed := ParseRequestBody(entry.Body, host)
			turn := &ParsedTurn{
				Seq:       entry.Seq,
				Request:   entry,
				ReqParsed: reqParsed,
				IsUtility: isUtilityRequest(reqParsed),
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

// isUtilityRequest detects auxiliary API calls that aren't part of the main conversation
// (e.g., Haiku model calls for session labeling)
func isUtilityRequest(req ParsedRequest) bool {
	// Haiku labeling requests
	if strings.Contains(strings.ToLower(req.Model), "haiku") {
		if strings.Contains(req.System, "new conversation topic") ||
			strings.Contains(req.System, "isNewTopic") {
			return true
		}
	}
	return false
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

	results := e.search(query, 100)

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
				matchStart := strings.Index(strings.ToLower(line), queryLower)
				matchEnd := matchStart + len(query)

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

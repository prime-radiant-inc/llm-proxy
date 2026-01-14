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
	ID      string
	Host    string
	Date    string
	Path    string
	ModTime time.Time
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

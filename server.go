package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type Server struct {
	config         Config
	mux            *http.ServeMux
	proxy          *Proxy
	fileLogger     *Logger
	lokiExporter   *LokiExporter
	multiWriter    *MultiWriter
	sessionManager *SessionManager
}

func NewServer(cfg Config) (*Server, error) {
	// Create file logger (primary)
	fileLogger, err := NewLogger(cfg.LogDir)
	if err != nil {
		return nil, err
	}

	// Create LokiExporter if enabled and URL is set
	var lokiExporter *LokiExporter
	if cfg.Loki.Enabled && cfg.Loki.URL != "" {
		lokiCfg := LokiExporterConfig{
			URL:         cfg.Loki.URL,
			AuthToken:   cfg.Loki.AuthToken,
			BatchSize:   cfg.Loki.BatchSize,
			RetryMax:    cfg.Loki.RetryMax,
			UseGzip:     cfg.Loki.UseGzip,
			Environment: cfg.Loki.Environment,
		}

		// Parse batch wait duration
		if cfg.Loki.BatchWaitStr != "" {
			if batchWait, err := time.ParseDuration(cfg.Loki.BatchWaitStr); err == nil {
				lokiCfg.BatchWait = batchWait
			}
		}

		var lokiErr error
		lokiExporter, lokiErr = NewLokiExporter(lokiCfg)
		if lokiErr != nil {
			// Graceful degradation: log warning and continue without Loki
			log.Printf("WARNING: Failed to create LokiExporter: %v", lokiErr)
			lokiExporter = nil
		}
	} else if cfg.Loki.Enabled && cfg.Loki.URL == "" {
		// Loki enabled but no URL - log warning
		log.Printf("WARNING: Loki enabled but URL is empty, continuing without Loki")
	}

	// Create MultiWriter wrapping both file logger and Loki exporter
	// Pass nil interface (not typed nil) when Loki is not enabled
	var lokiPusher LokiPusher
	if lokiExporter != nil {
		lokiPusher = lokiExporter
	}
	multiWriter := NewMultiWriter(fileLogger, lokiPusher)

	sessionManager, err := NewSessionManager(cfg.LogDir, fileLogger)
	if err != nil {
		if lokiExporter != nil {
			lokiExporter.Close()
		}
		fileLogger.Close()
		return nil, err
	}

	// Get event emitter from multiWriter (returns nil if Loki not configured)
	eventEmitter := multiWriter.EventEmitter()
	machineID := multiWriter.MachineID()

	s := &Server{
		config:         cfg,
		mux:            http.NewServeMux(),
		proxy:          NewProxyWithEventEmitter(multiWriter, sessionManager, eventEmitter, machineID),
		fileLogger:     fileLogger,
		lokiExporter:   lokiExporter,
		multiWriter:    multiWriter,
		sessionManager: sessionManager,
	}
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/health/loki", s.handleHealthLoki)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Check if it's a known endpoint
	if r.URL.Path == "/health" {
		s.handleHealth(w, r)
		return
	}
	if r.URL.Path == "/health/loki" {
		s.handleHealthLoki(w, r)
		return
	}

	// Otherwise, proxy the request
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// LokiHealthResponse is the JSON response for /health/loki endpoint
type LokiHealthResponse struct {
	Status         string  `json:"status"`
	EntriesSent    *int64  `json:"entries_sent,omitempty"`
	EntriesDropped *int64  `json:"entries_dropped,omitempty"`
	LastError      *string `json:"last_error"`
	LastErrorTime  *string `json:"last_error_time"`
}

func (s *Server) handleHealthLoki(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var response LokiHealthResponse

	if s.lokiExporter == nil {
		// Loki is disabled or failed to initialize
		response = LokiHealthResponse{
			Status:        "disabled",
			LastError:     nil,
			LastErrorTime: nil,
		}
	} else {
		// Loki is enabled - get stats
		stats := s.lokiExporter.Stats()
		response = LokiHealthResponse{
			Status:         "ok",
			EntriesSent:    &stats.EntriesSent,
			EntriesDropped: &stats.EntriesDropped,
			LastError:      nil,
			LastErrorTime:  nil,
		}
	}

	json.NewEncoder(w).Encode(response)
}

func (s *Server) Close() error {
	var err error
	if s.sessionManager != nil {
		err = s.sessionManager.Close()
	}
	// Close MultiWriter which handles Loki flush then file close
	if s.multiWriter != nil {
		if closeErr := s.multiWriter.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

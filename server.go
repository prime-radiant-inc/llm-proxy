package main

import (
	"net/http"
)

type Server struct {
	config Config
	mux    *http.ServeMux
	proxy  *Proxy
	logger *Logger
}

func NewServer(cfg Config) (*Server, error) {
	logger, err := NewLogger(cfg.LogDir)
	if err != nil {
		return nil, err
	}

	s := &Server{
		config: cfg,
		mux:    http.NewServeMux(),
		proxy:  NewProxyWithLogger(logger),
		logger: logger,
	}
	s.mux.HandleFunc("/health", s.handleHealth)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Check if it's a known endpoint
	if r.URL.Path == "/health" {
		s.handleHealth(w, r)
		return
	}

	// Otherwise, proxy the request
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *Server) Close() error {
	if s.logger != nil {
		return s.logger.Close()
	}
	return nil
}

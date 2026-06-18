package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

type ResolveContext struct {
	APIToken    string `json:"api_token"`
	ClientHost  string `json:"client_host"`
	Provider    string `json:"provider"`
	ProviderURL string `json:"provider_url"`
}

type ResolveResult struct {
	Token        string
	ClientFPHash string
	Project      string
	RunID        string
}

type resolverStdout struct {
	Token        string `json:"token"`
	ClientFPHash string `json:"client_fp_hash"`
	Project      string `json:"project"`
	RunID        string `json:"run_id"`
}

type cacheEntry struct {
	resolved ResolveResult
	expires  time.Time
}

type APITokenSubstituter struct {
	command string
	timeout time.Duration
	ttl     time.Duration
	maxSize int

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// limitedWriter buffers up to n bytes and silently discards the rest.
type limitedWriter struct {
	buf bytes.Buffer
	n   int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.n > 0 {
		take := len(p)
		if take > w.n {
			take = w.n
		}
		w.buf.Write(p[:take])
		w.n -= take
	}
	return len(p), nil
}

// providerURLRe permits only hostname[:port] characters — no path separators or shell metacharacters.
var providerURLRe = regexp.MustCompile(`^[A-Za-z0-9.\-:]+$`)

// NewAPITokenSubstituter constructs a substituter from cfg.
// cfg.Command must be a single executable path, NOT a shell command line;
// use a wrapper script if you need to pass arguments to the resolver.
func NewAPITokenSubstituter(cfg APITokenSubstitutionConfig) (*APITokenSubstituter, error) {
	if cfg.Command == "" {
		return nil, errors.New("api_token_substitution.command is required when enabled")
	}
	ttl, err := time.ParseDuration(cfg.CacheTTLStr)
	if err != nil {
		return nil, err
	}
	to, err := time.ParseDuration(cfg.TimeoutStr)
	if err != nil {
		return nil, err
	}
	size := cfg.CacheSize
	if size <= 0 {
		size = 10000
	}
	return &APITokenSubstituter{
		command: cfg.Command, timeout: to, ttl: ttl, maxSize: size,
		cache: make(map[string]cacheEntry),
	}, nil
}

func cacheKey(rc ResolveContext) string {
	return rc.Provider + "\x00" + rc.ProviderURL + "\x00" + rc.APIToken
}

func (s *APITokenSubstituter) cacheGet(k string) (ResolveResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[k]
	if !ok || time.Now().After(e.expires) {
		if ok {
			delete(s.cache, k)
		}
		return ResolveResult{}, false
	}
	return e.resolved, true
}

func (s *APITokenSubstituter) cachePut(k string, resolved ResolveResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= s.maxSize {
		for ek := range s.cache {
			delete(s.cache, ek)
			break
		}
	}
	s.cache[k] = cacheEntry{resolved: resolved, expires: time.Now().Add(s.ttl)}
}

// Resolve returns (resolved, httpStatus, err).
// httpStatus == 0 means success.
// httpStatus == 401: invalid provider_url, or resolver exited non-zero.
// httpStatus == 502: transient failure (timeout, spawn failure, command-not-found, or exit 0 with empty stdout).
//
// Concurrent calls for the same uncached key each invoke the resolver independently
// (no singleflight deduplication) — acceptable for a local resolver in v1.
func (s *APITokenSubstituter) Resolve(ctx context.Context, rc ResolveContext) (ResolveResult, int, error) {
	if !providerURLRe.MatchString(rc.ProviderURL) {
		return ResolveResult{}, 401, errors.New("invalid provider_url")
	}
	k := cacheKey(rc)
	if v, ok := s.cacheGet(k); ok {
		return v, 0, nil
	}

	payload, err := json.Marshal(rc)
	if err != nil {
		return ResolveResult{}, 502, err
	}
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, s.command)
	cmd.Stdin = bytes.NewReader(payload)
	outW := &limitedWriter{n: 64 * 1024}
	errW := &limitedWriter{n: 4 * 1024}
	cmd.Stdout = outW
	cmd.Stderr = errW
	runErr := cmd.Run()

	if cctx.Err() == context.DeadlineExceeded {
		return ResolveResult{}, 502, cctx.Err()
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return ResolveResult{}, 401, fmt.Errorf("resolver failed: %w: %s", runErr, strings.TrimSpace(errW.buf.String()))
		}
		return ResolveResult{}, 502, fmt.Errorf("resolver failed: %w: %s", runErr, strings.TrimSpace(errW.buf.String()))
	}
	raw := strings.TrimSpace(outW.buf.String())
	if raw == "" {
		return ResolveResult{}, 502, errors.New("resolver returned empty key")
	}

	resolved := ResolveResult{Token: raw}
	var parsed resolverStdout
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		if parsed.Token == "" {
			return ResolveResult{}, 502, errors.New("resolver returned json without token")
		}
		resolved = ResolveResult{
			Token:        parsed.Token,
			ClientFPHash: parsed.ClientFPHash,
			Project:      parsed.Project,
			RunID:        parsed.RunID,
		}
	}

	s.cachePut(k, resolved)
	return resolved, 0, nil
}

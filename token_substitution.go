package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

type cacheEntry struct {
	key     string
	expires time.Time
}

type APITokenSubstituter struct {
	command string
	timeout time.Duration
	ttl     time.Duration
	maxSize int

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// providerURLRe permits only hostname[:port] characters — no path separators or shell metacharacters.
var providerURLRe = regexp.MustCompile(`^[A-Za-z0-9.\-:]+$`)

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

func (s *APITokenSubstituter) cacheGet(k string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[k]
	if !ok || time.Now().After(e.expires) {
		if ok {
			delete(s.cache, k)
		}
		return "", false
	}
	return e.key, true
}

func (s *APITokenSubstituter) cachePut(k, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= s.maxSize {
		for ek := range s.cache {
			delete(s.cache, ek)
			break
		}
	}
	s.cache[k] = cacheEntry{key: v, expires: time.Now().Add(s.ttl)}
}

// Resolve returns (realKey, httpStatus, err).
// httpStatus == 0 means success.
// httpStatus == 401: invalid provider_url, or resolver exited non-zero.
// httpStatus == 502: transient failure (timeout, spawn failure, command-not-found, or exit 0 with empty stdout).
func (s *APITokenSubstituter) Resolve(ctx context.Context, rc ResolveContext) (string, int, error) {
	if !providerURLRe.MatchString(rc.ProviderURL) {
		return "", 401, errors.New("invalid provider_url")
	}
	k := cacheKey(rc)
	if v, ok := s.cacheGet(k); ok {
		return v, 0, nil
	}

	payload, err := json.Marshal(rc)
	if err != nil {
		return "", 502, err
	}
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, s.command)
	cmd.Stdin = bytes.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	runErr := cmd.Run()

	if cctx.Err() == context.DeadlineExceeded {
		return "", 502, cctx.Err()
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return "", 401, runErr
		}
		return "", 502, runErr
	}
	key := strings.TrimSpace(out.String())
	if key == "" {
		return "", 502, errors.New("resolver returned empty key")
	}
	s.cachePut(k, key)
	return key, 0, nil
}

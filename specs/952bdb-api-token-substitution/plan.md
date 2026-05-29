---
runId: 952bdb
feature: api-token-substitution
created: 2026-05-29
status: ready
linear: PRI-1897
---

# API Token Substitution — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. Work on branch `952bdb-api-token-substitution` (already created). Follow the repo constitution at `docs/constitutions/current/`. Spec: `specs/952bdb-api-token-substitution/spec.md`.

**Goal:** Add an opt-in mode where llm-proxy treats the inbound API token as a lookup key, resolves it to the real provider key via a configured local command (with an in-memory cache), substitutes it onto the outbound request, and fails closed — so the real key never lives in the client.

**Architecture:** A new `token_substitution.go` (Proxy layer) holds the resolver (execs the configured command, JSON context on stdin, exit-code-authoritative) and a TTL/size-bounded thread-safe cache keyed by `(provider, provider_url, api_token)`. It is wired into `Proxy` as a nullable `tokenSub` field (mirroring the existing `bedrock` field) and invoked in `ServeHTTP` after `copyHeaders` and before `client.Do`, on every `ParseProxyURL`-routed request. A separate top-level `listen_host` config lets the proxy bind a non-loopback address.

**Tech Stack:** Go 1.24, stdlib `net/http`, `os/exec`, `encoding/json`; TOML config (`config.go`); table-driven tests (`go test ./...`).

---

### Task 1: Config — `[api_token_substitution]` section + top-level `listen_host`

**Files:**
- Modify: `config.go` (struct at `:31-44`, `DefaultConfig` at `:46-59`, `LoadConfigFromEnv` at `:81-125`)
- Modify: `config.toml.example`
- Test: `config_test.go`

- [ ] **Step 1: Write the failing tests** — append to `config_test.go`:

```go
func TestDefaultConfigAPITokenSubstitution(t *testing.T) {
	c := DefaultConfig()
	if c.ListenHost != "localhost" {
		t.Errorf("ListenHost default = %q, want localhost", c.ListenHost)
	}
	if c.APITokenSubstitution.Enabled {
		t.Error("APITokenSubstitution should be disabled by default")
	}
	if c.APITokenSubstitution.CacheTTLStr != "5m" || c.APITokenSubstitution.CacheSize != 10000 || c.APITokenSubstitution.TimeoutStr != "2s" {
		t.Errorf("unexpected substitution defaults: %+v", c.APITokenSubstitution)
	}
}

func TestLoadConfigFromEnvAPITokenSubstitution(t *testing.T) {
	t.Setenv("LLM_PROXY_LISTEN_HOST", "10.0.100.1")
	t.Setenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_ENABLED", "true")
	t.Setenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_COMMAND", "/etc/llm-proxy/resolve-token")
	t.Setenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_CACHE_TTL", "30s")
	t.Setenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_CACHE_SIZE", "42")
	t.Setenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_TIMEOUT", "1s")
	c := LoadConfigFromEnv(DefaultConfig())
	if c.ListenHost != "10.0.100.1" {
		t.Errorf("ListenHost = %q", c.ListenHost)
	}
	if !c.APITokenSubstitution.Enabled || c.APITokenSubstitution.Command != "/etc/llm-proxy/resolve-token" {
		t.Errorf("substitution env not applied: %+v", c.APITokenSubstitution)
	}
	if c.APITokenSubstitution.CacheTTLStr != "30s" || c.APITokenSubstitution.CacheSize != 42 || c.APITokenSubstitution.TimeoutStr != "1s" {
		t.Errorf("substitution numeric/dur env not applied: %+v", c.APITokenSubstitution)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'APITokenSubstitution' ./... 2>&1 | head`
Expected: compile error (`APITokenSubstitution`/`ListenHost` undefined).

- [ ] **Step 3: Add the config types + defaults + env loading** — in `config.go`:

After the `LokiConfig` struct (`:20-29`) add:
```go
// APITokenSubstitutionConfig configures the opt-in API token substitution feature.
type APITokenSubstitutionConfig struct {
	Enabled     bool   `toml:"enabled"`
	Command     string `toml:"command"`    // local command; stdin gets the JSON context, stdout is the real key
	CacheTTLStr string `toml:"cache_ttl"`  // duration string, e.g. "5m"
	CacheSize   int    `toml:"cache_size"` // max cached entries (oldest evicted past this)
	TimeoutStr  string `toml:"timeout"`    // per-resolve duration string, e.g. "2s"
}
```

In the `Config` struct (`:31-44`) add two fields (alongside `Loki`):
```go
	ListenHost           string                     `toml:"listen_host"`
	APITokenSubstitution APITokenSubstitutionConfig `toml:"api_token_substitution"`
```

In `DefaultConfig()` (`:46-59`) add to the returned `Config{...}`:
```go
		ListenHost: "localhost",
		APITokenSubstitution: APITokenSubstitutionConfig{
			Enabled:     false,
			CacheTTLStr: "5m",
			CacheSize:   10000,
			TimeoutStr:  "2s",
		},
```

In `LoadConfigFromEnv` (before `return cfg`) add:
```go
	if host := os.Getenv("LLM_PROXY_LISTEN_HOST"); host != "" {
		cfg.ListenHost = host
	}
	if v := os.Getenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_ENABLED"); v != "" {
		cfg.APITokenSubstitution.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_COMMAND"); v != "" {
		cfg.APITokenSubstitution.Command = v
	}
	if v := os.Getenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_CACHE_TTL"); v != "" {
		cfg.APITokenSubstitution.CacheTTLStr = v
	}
	if v := os.Getenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_CACHE_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.APITokenSubstitution.CacheSize = n
		}
	}
	if v := os.Getenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_TIMEOUT"); v != "" {
		cfg.APITokenSubstitution.TimeoutStr = v
	}
```

- [ ] **Step 4: Document the section** — append to `config.toml.example`:
```toml

listen_host = "localhost"   # bind address; set to the bridge gateway IP for a shared-host deploy

[api_token_substitution]
enabled = false
command = ""          # local command: JSON context on stdin, real key on stdout (exit 0)
cache_ttl = "5m"
cache_size = 10000
timeout = "2s"
```

- [ ] **Step 5: Run to verify pass**

Run: `go test -run 'APITokenSubstitution' ./...`
Expected: PASS.

- [ ] **Step 6: Commit**
```bash
git add config.go config_test.go config.toml.example
git commit -m "feat(config): add api_token_substitution section + listen_host"
```

---

### Task 2: `token_substitution.go` — resolver command + cache

**Files:**
- Create: `token_substitution.go`
- Test: `token_substitution_test.go`

**Interface (used by Task 3):**
```go
type ResolveContext struct {
	APIToken    string `json:"api_token"`
	ClientHost  string `json:"client_host"`
	Provider    string `json:"provider"`
	ProviderURL string `json:"provider_url"`
}
// Resolve returns (realKey, httpStatus, err). httpStatus == 0 means success;
// 401 = reject (invalid provider_url, or resolver non-zero exit); 502 = transient
// (timeout, spawn failure, command-not-found, or exit 0 with empty stdout).
func (s *APITokenSubstituter) Resolve(ctx context.Context, rc ResolveContext) (string, int, error)
```

- [ ] **Step 1: Write the failing tests** — create `token_substitution_test.go`:

```go
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// writeScript writes an executable shell script and returns its path.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "resolve.sh")
	if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newSub(t *testing.T, cmd string) *APITokenSubstituter {
	t.Helper()
	s, err := NewAPITokenSubstituter(APITokenSubstitutionConfig{
		Enabled: true, Command: cmd, CacheTTLStr: "1m", CacheSize: 100, TimeoutStr: "5s",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestResolveSuccessReadsStdinContext(t *testing.T) {
	// Echo back a key derived from the provider so we can prove the context reached stdin.
	cmd := writeScript(t, `read -r line; echo "real-key-for-$(printf '%s' "$line" | python3 -c 'import json,sys;print(json.load(sys.stdin)["provider"])')"`)
	s := newSub(t, cmd)
	key, status, err := s.Resolve(context.Background(), ResolveContext{
		APIToken: "nonce123", ClientHost: "10.0.100.2:5", Provider: "anthropic", ProviderURL: "api.anthropic.com",
	})
	if err != nil || status != 0 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if key != "real-key-for-anthropic" {
		t.Fatalf("key=%q", key)
	}
}

func TestResolveInvalidProviderURLis401(t *testing.T) {
	s := newSub(t, writeScript(t, `echo should-not-run`))
	_, status, _ := s.Resolve(context.Background(), ResolveContext{
		APIToken: "n", Provider: "anthropic", ProviderURL: "api.anthropic.com/../../etc",
	})
	if status != 401 {
		t.Fatalf("status=%d, want 401", status)
	}
}

func TestResolveNonZeroExitIs401(t *testing.T) {
	s := newSub(t, writeScript(t, `exit 3`))
	_, status, _ := s.Resolve(context.Background(), ResolveContext{Provider: "anthropic", ProviderURL: "api.anthropic.com"})
	if status != 401 {
		t.Fatalf("status=%d, want 401", status)
	}
}

func TestResolveEmptyStdoutIs502(t *testing.T) {
	s := newSub(t, writeScript(t, `exit 0`)) // exit 0, no output
	_, status, _ := s.Resolve(context.Background(), ResolveContext{Provider: "anthropic", ProviderURL: "api.anthropic.com"})
	if status != 502 {
		t.Fatalf("status=%d, want 502", status)
	}
}

func TestResolveCommandNotFoundIs502(t *testing.T) {
	s := newSub(t, "/nonexistent/resolver-binary")
	_, status, _ := s.Resolve(context.Background(), ResolveContext{Provider: "anthropic", ProviderURL: "api.anthropic.com"})
	if status != 502 {
		t.Fatalf("status=%d, want 502", status)
	}
}

func TestResolveTimeoutIs502(t *testing.T) {
	s, _ := NewAPITokenSubstituter(APITokenSubstitutionConfig{
		Enabled: true, Command: writeScript(t, `sleep 5; echo k`), CacheTTLStr: "1m", CacheSize: 100, TimeoutStr: "100ms",
	})
	_, status, _ := s.Resolve(context.Background(), ResolveContext{Provider: "anthropic", ProviderURL: "api.anthropic.com"})
	if status != 502 {
		t.Fatalf("status=%d, want 502", status)
	}
}

func TestResolveCachesByContext(t *testing.T) {
	// Script appends a call marker to a counter file; a cache hit means only one call.
	counter := filepath.Join(t.TempDir(), "calls")
	cmd := writeScript(t, `echo x >> `+counter+`; echo the-key`)
	s := newSub(t, cmd)
	rc := ResolveContext{APIToken: "n1", Provider: "anthropic", ProviderURL: "api.anthropic.com"}
	for i := 0; i < 3; i++ {
		if _, st, _ := s.Resolve(context.Background(), rc); st != 0 {
			t.Fatalf("status=%d", st)
		}
	}
	b, _ := os.ReadFile(counter)
	if got := strings.Count(string(b), "x"); got != 1 {
		t.Fatalf("resolver ran %d times, want 1 (cache miss)", got)
	}
	// A different token is a different cache key → another call.
	rc.APIToken = "n2"
	s.Resolve(context.Background(), rc)
	b, _ = os.ReadFile(counter)
	if got := strings.Count(string(b), "x"); got != 2 {
		t.Fatalf("resolver ran %d times after new token, want 2", got)
	}
}

func TestResolveConcurrent(t *testing.T) {
	s := newSub(t, writeScript(t, `echo concurrent-key`))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Resolve(context.Background(), ResolveContext{APIToken: "n", Provider: "anthropic", ProviderURL: "api.anthropic.com"})
		}()
	}
	wg.Wait() // -race must stay clean
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'Resolve' ./... 2>&1 | head`
Expected: compile error (`APITokenSubstituter`/`NewAPITokenSubstituter`/`ResolveContext` undefined).

- [ ] **Step 3: Implement `token_substitution.go`**

```go
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

// ResolveContext is the identifying context handed to the resolver command (JSON on stdin).
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

// APITokenSubstituter resolves a presented token to the real provider key via a configured
// local command, caching the result by (provider, provider_url, api_token).
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
		// simplest bound: drop one arbitrary (oldest-ish) entry
		for ek := range s.cache {
			delete(s.cache, ek)
			break
		}
	}
	s.cache[k] = cacheEntry{key: v, expires: time.Now().Add(s.ttl)}
}

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
		return "", 502, cctx.Err() // timeout
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return "", 401, runErr // resolver ran and rejected (non-zero exit)
		}
		return "", 502, runErr // spawn failure / command-not-found
	}
	key := strings.TrimSpace(out.String())
	if key == "" {
		return "", 502, errors.New("resolver returned empty key") // exit 0, empty
	}
	s.cachePut(k, key)
	return key, 0, nil
}
```

- [ ] **Step 4: Run to verify pass (with race detector)**

Run: `go test -race -run 'Resolve' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**
```bash
git add token_substitution.go token_substitution_test.go
git commit -m "feat: API token substituter (resolver command + TTL/size cache, fail-closed)"
```

---

### Task 3: Wire substitution into `proxy.go`

**Files:**
- Modify: `proxy.go` (`Proxy` struct `:54-61`; `ServeHTTP` — after `copyHeaders`/set-Host, before the `shouldLog` block at `:341`)
- Test: `proxy_test.go`

- [ ] **Step 1: Write the failing tests** — append to `proxy_test.go`:

```go
// helper: a Proxy with substitution wired to the given resolver script.
func proxyWithSub(t *testing.T, scriptBody string) *Proxy {
	t.Helper()
	p := NewProxy()
	p.tokenSub = newSub(t, writeScript(t, scriptBody))
	return p
}

func TestSubstitutionReplacesKeyOnEveryEndpoint(t *testing.T) {
	// Upstream echoes back the x-api-key it received.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("X-Api-Key")))
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")
	p := proxyWithSub(t, `read -r _; echo REAL-KEY`)

	// count_tokens is NOT a conversation endpoint — must still be substituted.
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/models"} {
		req := httptest.NewRequest("POST", "/anthropic/"+host+path, strings.NewReader("{}"))
		req.Header.Set("X-Api-Key", "nonce-not-real")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if got := rec.Body.String(); got != "REAL-KEY" {
			t.Errorf("%s: upstream saw key %q, want REAL-KEY", path, got)
		}
	}
}

func TestSubstitutionStripsBothAuthHeaders(t *testing.T) {
	var seenAuth, seenKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenKey = r.Header.Get("X-Api-Key")
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")
	p := proxyWithSub(t, `read -r _; echo REAL-KEY`)
	req := httptest.NewRequest("POST", "/anthropic/"+host+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("X-Api-Key", "nonce") // x-api-key takes precedence
	req.Header.Set("Authorization", "Bearer nonce")
	p.ServeHTTP(httptest.NewRecorder(), req)
	if seenKey != "REAL-KEY" || seenAuth != "" {
		t.Errorf("x-api-key=%q authorization=%q; want REAL-KEY and empty", seenKey, seenAuth)
	}
}

func TestSubstitutionFailClosedNoUpstream(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")
	p := proxyWithSub(t, `exit 1`) // resolver rejects → 401
	req := httptest.NewRequest("POST", "/anthropic/"+host+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("X-Api-Key", "nonce")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("status=%d, want 401", rec.Code)
	}
	if called {
		t.Error("upstream was contacted on a fail-closed resolution")
	}
}

func TestSubstitutionDisabledIsPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("X-Api-Key")))
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")
	p := NewProxy() // tokenSub nil
	req := httptest.NewRequest("POST", "/anthropic/"+host+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("X-Api-Key", "client-key-verbatim")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if got := rec.Body.String(); got != "client-key-verbatim" {
		t.Errorf("passthrough changed the key: %q", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'Substitution' ./... 2>&1 | head`
Expected: compile error (`tokenSub` field unknown).

- [ ] **Step 3: Add the field + helpers + the ServeHTTP hook**

In the `Proxy` struct (`proxy.go:54-61`) add a field:
```go
	tokenSub *APITokenSubstituter
```

Add helpers (end of `proxy.go`):
```go
// readClientToken returns the presented token and which header it came from
// ("x-api-key" or "authorization"); x-api-key takes precedence. Empty headerName = none present.
func readClientToken(h http.Header) (token, headerName string) {
	if v := h.Get("X-Api-Key"); v != "" {
		return v, "x-api-key"
	}
	if v := h.Get("Authorization"); v != "" {
		return strings.TrimPrefix(v, "Bearer "), "authorization"
	}
	return "", ""
}

// setResolvedKey strips both auth headers and sets the resolved key on the header the client used
// (defaulting to x-api-key), so the opaque token never rides alongside the real key.
func setResolvedKey(h http.Header, headerName, key string) {
	h.Del("X-Api-Key")
	h.Del("Authorization")
	if headerName == "authorization" {
		h.Set("Authorization", "Bearer "+key)
	} else {
		h.Set("X-Api-Key", key)
	}
}
```

In `ServeHTTP`, immediately after `proxyReq.Host = upstream` (the set-Host line, ~`:333`) and **before** the `shouldLog` block, insert:
```go
	// API token substitution: resolve the presented token to the real provider key and
	// replace the auth header. Runs on every endpoint, before any logging, fail-closed.
	if p.tokenSub != nil {
		token, hdrName := readClientToken(r.Header)
		realKey, status, _ := p.tokenSub.Resolve(r.Context(), ResolveContext{
			APIToken:    token,
			ClientHost:  r.RemoteAddr,
			Provider:    provider,
			ProviderURL: upstream,
		})
		if status != 0 {
			http.Error(w, "api token substitution failed", status)
			return
		}
		setResolvedKey(proxyReq.Header, hdrName, realKey)
	}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test -race -run 'Substitution' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**
```bash
git add proxy.go proxy_test.go
git commit -m "feat(proxy): substitute the resolved key after copyHeaders, fail-closed, every endpoint"
```

---

### Task 4: Wire the substituter into `server.go`

**Files:**
- Modify: `server.go` (after the `NewProxyWithEventEmitter(...)` call at `:80`, alongside the `proxy.bedrock` wiring at `:83-95`)
- Test: `server_test.go`

- [ ] **Step 1: Write the failing test** — append to `server_test.go`:

```go
func TestNewServerWiresTokenSubWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.LogDir = dir
	cfg.APITokenSubstitution = APITokenSubstitutionConfig{
		Enabled: true, Command: "/bin/true", CacheTTLStr: "1m", CacheSize: 10, TimeoutStr: "1s",
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if srv.proxy.tokenSub == nil {
		t.Error("expected tokenSub to be wired when enabled")
	}
}

func TestNewServerNoTokenSubWhenDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogDir = t.TempDir()
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if srv.proxy.tokenSub != nil {
		t.Error("tokenSub should be nil when disabled")
	}
}
```

(If `Server` does not already expose `proxy`, confirm the field name in `server.go` and use it; the struct holds the `*Proxy` it serves.)

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'NewServerWiresTokenSub|NewServerNoTokenSub' ./... 2>&1 | head`
Expected: FAIL (tokenSub never set).

- [ ] **Step 3: Wire it** — in `server.go`, after `proxy := NewProxyWithEventEmitter(...)` and the bedrock block, add:
```go
	if cfg.APITokenSubstitution.Enabled {
		sub, err := NewAPITokenSubstituter(cfg.APITokenSubstitution)
		if err != nil {
			return nil, fmt.Errorf("api token substitution: %w", err)
		}
		proxy.tokenSub = sub
	}
```
(Ensure `fmt` is imported — it is used elsewhere in `server.go`.)

- [ ] **Step 4: Run to verify pass**

Run: `go test -run 'NewServer' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**
```bash
git add server.go server_test.go
git commit -m "feat(server): construct + inject the API token substituter from config"
```

---

### Task 5: Honor `listen_host` in `main.go`

**Files:**
- Modify: `main.go` (the listener at `:241`)
- Test: `main_test.go`

- [ ] **Step 1: Write the failing test** — append to `main_test.go` a unit test for a small extracted helper:

```go
func TestListenAddr(t *testing.T) {
	cases := []struct{ host string; port int; want string }{
		{"", 9999, "localhost:9999"},
		{"localhost", 9999, "localhost:9999"},
		{"10.0.100.1", 65500, "10.0.100.1:65500"},
	}
	for _, c := range cases {
		if got := listenAddr(c.host, c.port); got != c.want {
			t.Errorf("listenAddr(%q,%d)=%q want %q", c.host, c.port, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run TestListenAddr ./... 2>&1 | head`
Expected: compile error (`listenAddr` undefined).

- [ ] **Step 3: Add the helper + use it** — in `main.go` add:
```go
func listenAddr(host string, port int) string {
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("%s:%d", host, port)
}
```
and replace the `addr := fmt.Sprintf("localhost:%d", cfg.Port)` line (`:241`) with:
```go
	addr := listenAddr(cfg.ListenHost, cfg.Port)
```

- [ ] **Step 4: Run to verify pass + full suite**

Run: `go test -race ./...`
Expected: PASS (whole suite, including the existing localhost-default behavior unchanged).

- [ ] **Step 5: Commit**
```bash
git add main.go main_test.go
git commit -m "feat: bind the configured listen_host (default localhost)"
```

---

## Self-review notes

- **Spec coverage:** FR1 config (Task 1) + listen_host related-change (Tasks 1,5); FR2 disabled-passthrough (Task 3 test); FR3 resolve-early/every-endpoint (Task 3, count_tokens test); FR4 Set after copyHeaders + strip both (Task 3); FR5 fail-closed 401/502 before logging (Tasks 2,3 — hook precedes the `shouldLog` block); FR6 key-never-logged (the logger uses `r.Header`, not the mutated `proxyReq.Header`; no extra task needed, but Task 3's hook deliberately mutates only `proxyReq`); FR7 cache (Task 2). Resolver contract exit-code mapping (Task 2).
- **Constitution:** proxy-layer placement (new file + Proxy field); fail-closed; secrets never logged (only `proxyReq.Header` is mutated; error strings carry no key); table-driven + `-race` tests; config precedence env>TOML>defaults (Task 1).
- **Type consistency:** `APITokenSubstitutionConfig`, `APITokenSubstituter`, `ResolveContext`, `Resolve(ctx, rc) (string, int, error)`, `tokenSub` field, `readClientToken`/`setResolvedKey` are used identically across Tasks 1–4.
- **Open implementation check:** confirm `Server` exposes its `*Proxy` as `proxy` (Task 4 test references `srv.proxy`); if the field is named differently, adjust the test + keep the wiring.

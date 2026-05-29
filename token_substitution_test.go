package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

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
	s := newSub(t, writeScript(t, `exit 0`))
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
	wg.Wait()
}

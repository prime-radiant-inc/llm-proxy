package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	ctxFile := filepath.Join(t.TempDir(), "ctx.json")
	cmd := writeScript(t, `cat > `+ctxFile+`; echo the-real-key`)
	s := newSub(t, cmd)
	resolved, status, err := s.Resolve(context.Background(), ResolveContext{
		APIToken: "nonce123", ClientHost: "10.0.100.2:5", Provider: "anthropic", ProviderURL: "api.anthropic.com",
	})
	if err != nil || status != 0 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if resolved.Token != "the-real-key" {
		t.Fatalf("key=%q", resolved.Token)
	}
	got, err := os.ReadFile(ctxFile)
	if err != nil {
		t.Fatalf("reading ctx file: %v", err)
	}
	gotStr := string(got)
	if !strings.Contains(gotStr, `"provider":"anthropic"`) {
		t.Fatalf("ctx.json missing provider field: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"api_token":"nonce123"`) {
		t.Fatalf("ctx.json missing api_token field: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"provider_url":"api.anthropic.com"`) {
		t.Fatalf("ctx.json missing provider_url field: %s", gotStr)
	}
}

func TestAPITokenSubstituterAcceptsResolverJSONMetadata(t *testing.T) {
	s := newSub(t, writeScript(t, `echo '{"token":"real-bearer","client_fp_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","project":"proj","run_id":"run-test"}'`))

	resolved, status, err := s.Resolve(context.Background(), ResolveContext{
		APIToken: "nonce123", Provider: "anthropic", ProviderURL: "api.anthropic.com",
	})
	if err != nil || status != 0 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if resolved.Token != "real-bearer" {
		t.Fatalf("token=%q", resolved.Token)
	}
	if resolved.ClientFPHash != strings.Repeat("a", 64) {
		t.Fatalf("client_fp_hash=%q", resolved.ClientFPHash)
	}
	if resolved.Project != "proj" {
		t.Fatalf("project=%q", resolved.Project)
	}
	if resolved.RunID != "run-test" {
		t.Fatalf("run_id=%q", resolved.RunID)
	}
}

func TestAPITokenSubstituterStillAcceptsBareToken(t *testing.T) {
	s := newSub(t, writeScript(t, `echo real-bearer`))

	resolved, status, err := s.Resolve(context.Background(), ResolveContext{
		APIToken: "nonce123", Provider: "anthropic", ProviderURL: "api.anthropic.com",
	})
	if err != nil || status != 0 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if resolved.Token != "real-bearer" {
		t.Fatalf("token=%q", resolved.Token)
	}
	if resolved.ClientFPHash != "" || resolved.Project != "" || resolved.RunID != "" {
		t.Fatalf("unexpected metadata: %+v", resolved)
	}
}

func TestAPITokenSubstituterRejectsJSONWithoutToken(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{
			name:   "object",
			output: `{"client_fp_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","project":"proj","run_id":"run-test"}`,
		},
		{
			name:   "string",
			output: `"real-bearer"`,
		},
		{
			name:   "number",
			output: `123`,
		},
		{
			name:   "bool",
			output: `true`,
		},
		{
			name:   "array",
			output: `[]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSub(t, writeScript(t, `printf '%s\n' '`+tt.output+`'`))

			_, status, err := s.Resolve(context.Background(), ResolveContext{
				APIToken: "nonce123", Provider: "anthropic", ProviderURL: "api.anthropic.com",
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if status != 502 {
				t.Fatalf("status=%d, want 502", status)
			}
		})
	}
}

func TestAPITokenSubstituterCachesMetadataWithToken(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "calls")
	s := newSub(t, writeScript(t, `echo x >> `+counter+`; echo '{"token":"real-bearer","client_fp_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","project":"proj","run_id":"run-test"}'`))
	rc := ResolveContext{APIToken: "nonce123", Provider: "anthropic", ProviderURL: "api.anthropic.com"}

	first, status, err := s.Resolve(context.Background(), rc)
	if err != nil || status != 0 {
		t.Fatalf("first resolve status=%d err=%v", status, err)
	}
	second, status, err := s.Resolve(context.Background(), rc)
	if err != nil || status != 0 {
		t.Fatalf("second resolve status=%d err=%v", status, err)
	}
	if first != second {
		t.Fatalf("cached result mismatch: first=%+v second=%+v", first, second)
	}
	if second.ClientFPHash != strings.Repeat("a", 64) || second.Project != "proj" || second.RunID != "run-test" {
		t.Fatalf("cached metadata missing: %+v", second)
	}

	b, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("reading counter: %v", err)
	}
	if got := strings.Count(string(b), "x"); got != 1 {
		t.Fatalf("resolver ran %d times, want 1 (cache hit should preserve metadata)", got)
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
	// Changing APIToken alone causes a new resolver call.
	rc.APIToken = "n2"
	s.Resolve(context.Background(), rc)
	b, _ = os.ReadFile(counter)
	if got := strings.Count(string(b), "x"); got != 2 {
		t.Fatalf("resolver ran %d times after new token, want 2", got)
	}
	// Changing ProviderURL alone (with original token) also causes a new resolver call.
	rc2 := ResolveContext{APIToken: "n1", Provider: "anthropic", ProviderURL: "api2.anthropic.com"}
	s.Resolve(context.Background(), rc2)
	b, _ = os.ReadFile(counter)
	if got := strings.Count(string(b), "x"); got != 3 {
		t.Fatalf("resolver ran %d times after new ProviderURL, want 3", got)
	}
}

func TestResolveCacheTTLExpiry(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "calls")
	cmd := writeScript(t, `echo x >> `+counter+`; echo ttl-key`)
	s, err := NewAPITokenSubstituter(APITokenSubstitutionConfig{
		Enabled: true, Command: cmd, CacheTTLStr: "1ms", CacheSize: 100, TimeoutStr: "5s",
	})
	if err != nil {
		t.Fatal(err)
	}
	rc := ResolveContext{APIToken: "n1", Provider: "anthropic", ProviderURL: "api.anthropic.com"}

	if _, st, _ := s.Resolve(context.Background(), rc); st != 0 {
		t.Fatalf("first resolve status=%d", st)
	}
	// Wait for TTL to expire.
	time.Sleep(5 * time.Millisecond)
	if _, st, _ := s.Resolve(context.Background(), rc); st != 0 {
		t.Fatalf("second resolve status=%d", st)
	}
	b, _ := os.ReadFile(counter)
	if got := strings.Count(string(b), "x"); got != 2 {
		t.Fatalf("resolver ran %d times, want 2 (TTL expired)", got)
	}
}

func TestResolveCacheEviction(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "calls")
	cmd := writeScript(t, `echo x >> `+counter+`; echo evict-key`)
	s, err := NewAPITokenSubstituter(APITokenSubstitutionConfig{
		Enabled: true, Command: cmd, CacheTTLStr: "1m", CacheSize: 1, TimeoutStr: "5s",
	})
	if err != nil {
		t.Fatal(err)
	}
	rc1 := ResolveContext{APIToken: "first", Provider: "anthropic", ProviderURL: "api.anthropic.com"}
	rc2 := ResolveContext{APIToken: "second", Provider: "anthropic", ProviderURL: "api.anthropic.com"}

	// Resolve first key — fills the single cache slot.
	if _, st, _ := s.Resolve(context.Background(), rc1); st != 0 {
		t.Fatalf("rc1 first resolve status=%d", st)
	}
	// Resolve second key — evicts the first.
	if _, st, _ := s.Resolve(context.Background(), rc2); st != 0 {
		t.Fatalf("rc2 resolve status=%d", st)
	}
	// Re-resolve first key — must be a cache miss (evicted), so resolver runs again.
	if _, st, _ := s.Resolve(context.Background(), rc1); st != 0 {
		t.Fatalf("rc1 second resolve status=%d", st)
	}
	b, _ := os.ReadFile(counter)
	if got := strings.Count(string(b), "x"); got != 3 {
		t.Fatalf("resolver ran %d times, want 3 (first key evicted and re-fetched)", got)
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

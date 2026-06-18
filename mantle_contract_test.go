package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCountingResolver(t *testing.T, output string) (*APITokenSubstituter, string) {
	t.Helper()

	counter := filepath.Join(t.TempDir(), "resolver-count")
	script := writeScript(t, `echo x >> `+counter+`; printf '%s\n' '`+output+`'`)
	return newSub(t, script), counter
}

func readResolverCalls(t *testing.T, counter string) int {
	t.Helper()

	data, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read resolver count: %v", err)
	}
	return strings.Count(strings.TrimSpace(string(data)), "x")
}

func TestServeMantleRejectsBarePathWhenRunIDRequired(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
	proxy.tokenSub = counterSub
	proxy.mantleRequireCloudBuildRunID = true

	req := httptest.NewRequest("POST", "/mantle/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 0 {
		t.Fatalf("resolver should not run for bare /mantle path when run id is required")
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestServeMantleRejectsInvalidCBRunIDBeforeResolver(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
	proxy.tokenSub = counterSub

	req := httptest.NewRequest("POST", "/cbrun/-bad/mantle/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 0 {
		t.Fatalf("resolver should not run for invalid cloud build run id")
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestServeMantleRejectsEncodedSlashRunIDBeforeResolver(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
	proxy.tokenSub = counterSub

	req := httptest.NewRequest("POST", "/cbrun/run%2F123/mantle/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 0 {
		t.Fatalf("resolver should not run for encoded slash in cloud build run id")
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestServeMantleRejectsEncodedTraversalSuffixBeforeResolver(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
	proxy.tokenSub = counterSub
	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/cbrun/run-123/mantle/v1/%2e%2e/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 0 {
		t.Fatalf("resolver should not run for encoded traversal in mantle suffix")
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestServeMantleRejectsNonExactRunScopedPathBeforeResolver(t *testing.T) {
	cases := []string{
		"/cbrun/run-123/mantle/v1/responses/extra",
		"/cbrun/run-123/mantle/v1/chat/completions",
	}

	for _, reqPath := range cases {
		t.Run(reqPath, func(t *testing.T) {
			upstreamCalls := 0
			proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls++
			}))
			defer mock.Close()

			counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
			proxy.tokenSub = counterSub
			mockHost := strings.TrimPrefix(mock.URL, "http://")
			proxy.bedrock.client = &http.Client{
				Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
			}

			req := httptest.NewRequest("POST", reqPath, strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer inbound-nonce")
			w := httptest.NewRecorder()

			proxy.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
			}
			if readResolverCalls(t, counter) != 0 {
				t.Fatalf("resolver should not run for non-exact mantle path %q", reqPath)
			}
			if upstreamCalls != 0 {
				t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
			}
		})
	}
}

func TestServeMantleRejectsUnresolvedRunIDBeforeUpstream(t *testing.T) {
	upstreamCalls := 0
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-other"}`)
	proxy.tokenSub = counterSub

	req := httptest.NewRequest("POST", "/cbrun/run-123/mantle/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 1 {
		t.Fatalf("resolver calls = %d, want 1", readResolverCalls(t, counter))
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestServeMantleForwardsRunScopedPath(t *testing.T) {
	var receivedPath, receivedAuth string
	proxy, mock := newTestBedrockProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp_123","object":"response","output":[]}`))
	}))
	defer mock.Close()

	counterSub, counter := newCountingResolver(t, `{"token":"REAL-BEARER","run_id":"run-123"}`)
	proxy.tokenSub = counterSub

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	proxy.bedrock.client = &http.Client{
		Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport},
	}

	req := httptest.NewRequest("POST", "/cbrun/run-123/mantle/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-nonce")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}
	if readResolverCalls(t, counter) != 1 {
		t.Fatalf("resolver calls = %d, want 1", readResolverCalls(t, counter))
	}
	if receivedPath != "/openai/v1/responses" {
		t.Fatalf("upstream path = %q, want /openai/v1/responses", receivedPath)
	}
	if receivedAuth != "Bearer REAL-BEARER" {
		t.Fatalf("upstream auth = %q, want %q", receivedAuth, "Bearer REAL-BEARER")
	}
}

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// staticCredentials provides fixed AWS credentials for signing tests.
type staticCredentials struct{}

func (s staticCredentials) Retrieve(ctx context.Context) (aws.Credentials, error) {
	return aws.Credentials{
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Source:          "test",
	}, nil
}

// newTestPlatformProxy builds a Proxy with platform-on-AWS signing enabled and
// static test credentials (reused from bedrock_test.go, same package).
func newTestPlatformProxy(region, workspaceID string) *Proxy {
	p := NewProxy()
	p.platformAWS = &platformAWSState{
		region:      region,
		workspaceID: workspaceID,
		credProv:    staticCredentials{},
		signer:      v4.NewSigner(),
	}
	return p
}

func TestApplyPlatformAWS_RewritesUpstreamAndSigns(t *testing.T) {
	p := newTestPlatformProxy("us-east-1", "wrkspc_unit")
	body := []byte(`{"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)

	req, err := http.NewRequest("POST", "http://localhost:9999/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Api-Key", "sk-should-be-dropped")
	req.Header.Set("Authorization", "Bearer should-be-dropped")

	if err := p.applyPlatformAWS(req, body); err != nil {
		t.Fatalf("applyPlatformAWS: %v", err)
	}

	if req.URL.Scheme != "https" {
		t.Errorf("scheme = %q, want https", req.URL.Scheme)
	}
	wantHost := "aws-external-anthropic.us-east-1.api.aws"
	if req.URL.Host != wantHost {
		t.Errorf("URL.Host = %q, want %q", req.URL.Host, wantHost)
	}
	if req.Host != wantHost {
		t.Errorf("req.Host = %q, want %q", req.Host, wantHost)
	}
	if req.URL.Path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", req.URL.Path)
	}
	if req.Header.Get("X-Api-Key") != "" {
		t.Error("X-Api-Key should be dropped before signing")
	}
	if req.Header.Get("Anthropic-Workspace-Id") != "wrkspc_unit" {
		t.Errorf("workspace header = %q, want wrkspc_unit", req.Header.Get("Anthropic-Workspace-Id"))
	}
	if req.Header.Get("Anthropic-Version") != anthropicVersionDefault {
		t.Errorf("anthropic-version = %q, want %q (defaulted when client omits it)", req.Header.Get("Anthropic-Version"), anthropicVersionDefault)
	}
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization = %q, want AWS4-HMAC-SHA256 signature", auth)
	}
	if !strings.Contains(auth, "us-east-1/aws-external-anthropic/aws4_request") {
		t.Errorf("Authorization credential scope wrong: %q", auth)
	}
	if req.Header.Get("X-Amz-Date") == "" {
		t.Error("X-Amz-Date should be set by the signer")
	}
}

// signedHeadersFromAuth extracts the SignedHeaders list (semicolon-separated,
// lowercase) from an AWS4-HMAC-SHA256 Authorization header.
func signedHeadersFromAuth(t *testing.T, auth string) []string {
	t.Helper()
	const marker = "SignedHeaders="
	i := strings.Index(auth, marker)
	if i < 0 {
		t.Fatalf("no SignedHeaders in Authorization: %q", auth)
	}
	rest := auth[i+len(marker):]
	if j := strings.Index(rest, ","); j >= 0 {
		rest = rest[:j]
	}
	return strings.Split(strings.TrimSpace(rest), ";")
}

// TestApplyPlatformAWS_StripsHopByHopBeforeSigning is the regression for the live
// 401: the Anthropic SDK sends a Connection header, which Go's transport strips
// or normalizes in transit. If it is signed, AWS computes `connection:` empty and
// the signature never matches. Hop-by-hop headers must be removed BEFORE signing,
// so the signed header set equals what is actually transmitted.
func TestApplyPlatformAWS_StripsHopByHopBeforeSigning(t *testing.T) {
	p := newTestPlatformProxy("us-west-2", "wrkspc_hop")
	body := []byte(`{"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)

	req, err := http.NewRequest("POST", "http://localhost:9999/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	// The header shape the real SDK sends: hop-by-hop Connection plus a header it
	// names, alongside legitimate x-stainless-* headers that DO survive transit.
	req.Header.Set("Connection", "keep-alive, X-Custom-Hop")
	req.Header.Set("X-Custom-Hop", "drop-me")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Transfer-Encoding", "chunked")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Stainless-Runtime", "node")
	req.Header.Set("X-Stainless-Package-Version", "0.0.0")

	if err := p.applyPlatformAWS(req, body); err != nil {
		t.Fatalf("applyPlatformAWS: %v", err)
	}

	// Hop-by-hop headers and the header named by Connection must be gone from the
	// transmitted request.
	for _, h := range []string{"Connection", "X-Custom-Hop", "Keep-Alive", "Transfer-Encoding"} {
		if req.Header.Get(h) != "" {
			t.Errorf("%s should be stripped before signing, still present: %q", h, req.Header.Get(h))
		}
	}
	// Legitimate headers survive and are forwarded.
	if req.Header.Get("X-Stainless-Lang") != "js" {
		t.Error("x-stainless-lang should be preserved")
	}

	signed := signedHeadersFromAuth(t, req.Header.Get("Authorization"))
	signedSet := map[string]bool{}
	for _, s := range signed {
		signedSet[s] = true
	}
	// The bug: connection (and other hop-by-hop) must NOT be in SignedHeaders.
	for _, banned := range []string{"connection", "x-custom-hop", "keep-alive", "transfer-encoding"} {
		if signedSet[banned] {
			t.Errorf("%q must not be in SignedHeaders (hop-by-hop): %v", banned, signed)
		}
	}
	// The stainless headers, being transmitted, are fine to sign.
	if !signedSet["x-stainless-lang"] {
		t.Errorf("expected x-stainless-lang in SignedHeaders: %v", signed)
	}
	// Signed set must equal transmitted set. host and content-length are managed
	// by Go's transport (kept off the header map) but ARE transmitted exactly as
	// signed; every other signed header must be present on the request.
	for _, name := range signed {
		if name == "host" || name == "content-length" {
			continue
		}
		if req.Header.Get(name) == "" {
			t.Errorf("signed header %q is not present on the transmitted request; signed set diverges from transmitted set", name)
		}
	}
}

func TestServeHTTP_PlatformAWS_SignedRoundTrip(t *testing.T) {
	var captured http.Header
	var capturedPath string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer mock.Close()

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	p := newTestPlatformProxy("us-west-2", "wrkspc_round")
	// Deliver the (already-signed) request to the mock instead of the real endpoint.
	p.client = &http.Client{Transport: &rewriteTransport{target: mockHost, inner: http.DefaultTransport}}

	body := `{"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/anthropic/api.anthropic.com/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "sk-should-be-dropped")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "cache-diagnosis-2026-04-07,model-context-window-exceeded-2025-08-26")

	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if capturedPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", capturedPath)
	}
	auth := captured.Get("Authorization")
	if !strings.Contains(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization = %q, want AWS4-HMAC-SHA256", auth)
	}
	if !strings.Contains(auth, "us-west-2/aws-external-anthropic/aws4_request") {
		t.Errorf("Authorization credential scope wrong: %q", auth)
	}
	if captured.Get("X-Api-Key") != "" {
		t.Error("X-Api-Key must not reach the upstream")
	}
	if captured.Get("Anthropic-Workspace-Id") != "wrkspc_round" {
		t.Errorf("workspace header = %q, want wrkspc_round", captured.Get("Anthropic-Workspace-Id"))
	}
	if captured.Get("Anthropic-Beta") != "cache-diagnosis-2026-04-07,model-context-window-exceeded-2025-08-26" {
		t.Errorf("anthropic-beta should pass through, got %q", captured.Get("Anthropic-Beta"))
	}
	if captured.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01 (client value preserved)", captured.Get("Anthropic-Version"))
	}
}

// TestServeHTTP_PlatformAWS_ModeOff verifies that with platform mode disabled the
// anthropic passthrough is unchanged: the client x-api-key reaches the upstream and
// no SigV4 signing occurs.
func TestServeHTTP_PlatformAWS_ModeOff(t *testing.T) {
	var captured http.Header
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer mock.Close()

	mockHost := strings.TrimPrefix(mock.URL, "http://")
	p := NewProxy() // platformAWS is nil

	body := `{"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/anthropic/"+mockHost+"/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "sk-passthrough")

	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if captured.Get("X-Api-Key") != "sk-passthrough" {
		t.Errorf("X-Api-Key = %q, want sk-passthrough forwarded when mode off", captured.Get("X-Api-Key"))
	}
	if strings.Contains(captured.Get("Authorization"), "AWS4-HMAC-SHA256") {
		t.Error("no SigV4 signing should occur when mode is off")
	}
}

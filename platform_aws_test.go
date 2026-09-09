package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/aws/smithy-go"
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

type fakeAssumeRoleResult struct {
	credentials *types.Credentials
	err         error
}

type fakeAssumeRoleClient struct {
	mu      sync.Mutex
	results []fakeAssumeRoleResult
	inputs  []sts.AssumeRoleInput
	started chan struct{}
	release chan struct{}
}

func (f *fakeAssumeRoleClient) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.mu.Lock()
	call := len(f.inputs)
	f.inputs = append(f.inputs, *in)
	if call >= len(f.results) {
		f.mu.Unlock()
		return nil, errors.New("unexpected AssumeRole call")
	}
	result := f.results[call]
	started := f.started
	release := f.release
	f.mu.Unlock()

	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if result.err != nil {
		return nil, result.err
	}
	return &sts.AssumeRoleOutput{Credentials: result.credentials}, nil
}

func (f *fakeAssumeRoleClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inputs)
}

func assumedCredentials(accessKeyID string, expiration time.Time) *types.Credentials {
	return &types.Credentials{
		AccessKeyId:     aws.String(accessKeyID),
		SecretAccessKey: aws.String("assumed-secret"),
		SessionToken:    aws.String("assumed-session-token"),
		Expiration:      aws.Time(expiration),
	}
}

func TestAssumeRoleCredentials_CachesAndUsesBoundedSession(t *testing.T) {
	fake := &fakeAssumeRoleClient{results: []fakeAssumeRoleResult{{
		credentials: assumedCredentials("ASIAFIRST", time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)),
	}}}
	provider := newPlatformAWSAssumeRoleCredentials(fake, "arn:aws:iam::123456789012:role/example-inference")

	first, err := provider.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("first Retrieve: %v", err)
	}
	second, err := provider.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("second Retrieve: %v", err)
	}

	if first.AccessKeyID != "ASIAFIRST" || second.AccessKeyID != "ASIAFIRST" {
		t.Fatalf("access keys = %q, %q; want cached ASIAFIRST", first.AccessKeyID, second.AccessKeyID)
	}
	if fake.callCount() != 1 {
		t.Fatalf("AssumeRole calls = %d, want 1", fake.callCount())
	}
	fake.mu.Lock()
	input := fake.inputs[0]
	fake.mu.Unlock()
	if got := aws.ToString(input.RoleArn); got != "arn:aws:iam::123456789012:role/example-inference" {
		t.Errorf("RoleArn = %q, want configured role", got)
	}
	if got := aws.ToString(input.RoleSessionName); got != "llm-proxy-platform" {
		t.Errorf("RoleSessionName = %q, want llm-proxy-platform", got)
	}
	if got := aws.ToInt32(input.DurationSeconds); got != 3600 {
		t.Errorf("DurationSeconds = %d, want 3600", got)
	}
	if input.SourceIdentity != nil {
		t.Errorf("SourceIdentity = %q, want unset", aws.ToString(input.SourceIdentity))
	}
}

func TestAssumeRoleCredentials_RefreshesExpiredCredentials(t *testing.T) {
	fake := &fakeAssumeRoleClient{results: []fakeAssumeRoleResult{
		{credentials: assumedCredentials("ASIAEXPIRED", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))},
		{credentials: assumedCredentials("ASIAFRESH", time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))},
	}}
	provider := newPlatformAWSAssumeRoleCredentials(fake, "arn:aws:iam::123456789012:role/model-inference")

	first, err := provider.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("first Retrieve: %v", err)
	}
	second, err := provider.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("second Retrieve: %v", err)
	}

	if first.AccessKeyID != "ASIAEXPIRED" || second.AccessKeyID != "ASIAFRESH" {
		t.Fatalf("access keys = %q, %q; want ASIAEXPIRED then ASIAFRESH", first.AccessKeyID, second.AccessKeyID)
	}
	if fake.callCount() != 2 {
		t.Fatalf("AssumeRole calls = %d, want 2", fake.callCount())
	}
}

func TestAssumeRoleCredentials_CoalescesConcurrentFirstRetrieval(t *testing.T) {
	const callers = 12
	fake := &fakeAssumeRoleClient{
		results: []fakeAssumeRoleResult{{credentials: assumedCredentials("ASIASHARED", time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))}},
		started: make(chan struct{}, callers),
		release: make(chan struct{}),
	}
	provider := newPlatformAWSAssumeRoleCredentials(fake, "arn:aws:iam::123456789012:role/model-inference")

	start := make(chan struct{})
	results := make(chan aws.Credentials, callers)
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for range callers {
		go func() {
			ready.Done()
			<-start
			credentials, err := provider.Retrieve(context.Background())
			results <- credentials
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	<-fake.started
	close(fake.release)

	for range callers {
		if err := <-errs; err != nil {
			t.Errorf("Retrieve: %v", err)
		}
		if got := (<-results).AccessKeyID; got != "ASIASHARED" {
			t.Errorf("AccessKeyID = %q, want ASIASHARED", got)
		}
	}
	if fake.callCount() != 1 {
		t.Fatalf("AssumeRole calls = %d, want 1", fake.callCount())
	}
}

func TestServeHTTP_PlatformAWS_AssumeRoleFailureIsOpaqueAndDoesNotSendUpstream(t *testing.T) {
	const secretCanary = "sts-secret-canary"
	fake := &fakeAssumeRoleClient{results: []fakeAssumeRoleResult{
		{credentials: assumedCredentials("ASIAEXPIRED", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))},
		{err: errors.New(secretCanary)},
	}}
	credentials := newPlatformAWSAssumeRoleCredentials(fake, "arn:aws:iam::123456789012:role/model-inference")
	if _, err := credentials.Retrieve(context.Background()); err != nil {
		t.Fatalf("prime expired credentials: %v", err)
	}
	p := NewProxy()
	p.platformAWS = &platformAWSState{
		region:      "us-west-2",
		workspaceID: "wrkspc_configured",
		credProv:    credentials,
		signer:      v4.NewSigner(),
	}
	upstreamCalls := 0
	p.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		upstreamCalls++
		return nil, errors.New("unexpected upstream request")
	})}

	req := httptest.NewRequest("POST", "/anthropic/api.anthropic.com/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("X-Api-Key", "client-key")
	req.Header.Set("Anthropic-Workspace-Id", "wrkspc_client_override")
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if strings.Contains(recorder.Body.String(), secretCanary) {
		t.Errorf("response leaked credential error: %q", recorder.Body.String())
	}
	if got := recorder.Body.String(); got != "platform-aws signing failed\n" {
		t.Errorf("response body = %q, want opaque signing failure", got)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstream calls = %d, want 0", upstreamCalls)
	}
	if fake.callCount() != 2 {
		t.Errorf("AssumeRole calls = %d, want 2", fake.callCount())
	}
}

func TestServeHTTP_PlatformAWS_AssumeRoleFailureClosesCaptureWithoutLeakingSecrets(t *testing.T) {
	const secretCanary = "sts-secret-canary"
	for _, tc := range []struct {
		name           string
		primeExpired   bool
		err            error
		wantDiagnostic string
	}{
		{
			name:           "initial access denied",
			err:            &smithy.GenericAPIError{Code: "AccessDenied", Message: secretCanary, Fault: smithy.FaultClient},
			wantDiagnostic: "AWS AccessDenied",
		},
		{
			name: "refresh expired token", primeExpired: true,
			err:            &smithy.GenericAPIError{Code: "ExpiredToken", Message: secretCanary + " ASIAEXPIRED assumed-secret assumed-session-token", Fault: smithy.FaultClient},
			wantDiagnostic: "AWS ExpiredToken",
		},
		{
			name:           "network failure",
			err:            &net.OpError{Op: "dial", Net: "tcp", Err: errors.New(secretCanary)},
			wantDiagnostic: "network error",
		},
		{
			name:           "deadline exceeded",
			err:            fmt.Errorf("%s: %w", secretCanary, context.DeadlineExceeded),
			wantDiagnostic: "deadline exceeded",
		},
		{
			name:           "request canceled",
			err:            fmt.Errorf("%s: %w", secretCanary, context.Canceled),
			wantDiagnostic: "request canceled",
		},
		{
			name:           "unrecognized AWS error code",
			err:            &smithy.GenericAPIError{Code: secretCanary, Message: secretCanary, Fault: smithy.FaultUnknown},
			wantDiagnostic: "AWS API error",
		},
		{
			name:           "unknown provider failure",
			err:            errors.New(secretCanary),
			wantDiagnostic: "credential or signing error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAssumeRoleClient{}
			if tc.primeExpired {
				fake.results = append(fake.results, fakeAssumeRoleResult{
					credentials: assumedCredentials("ASIAEXPIRED", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)),
				})
			}
			fake.results = append(fake.results, fakeAssumeRoleResult{err: tc.err})
			credentials := newPlatformAWSAssumeRoleCredentials(fake, "arn:aws:iam::123456789012:role/model-inference")
			if tc.primeExpired {
				if _, err := credentials.Retrieve(context.Background()); err != nil {
					t.Fatalf("prime expired credentials: %v", err)
				}
			}

			logger, err := NewLogger(t.TempDir())
			if err != nil {
				t.Fatalf("NewLogger: %v", err)
			}
			defer logger.Close()
			var diagnostics bytes.Buffer
			previousLogWriter := log.Writer()
			log.SetOutput(&diagnostics)
			defer log.SetOutput(previousLogWriter)

			p := newTestPlatformProxy("us-west-2", "wrkspc_configured")
			p.platformAWS.credProv = credentials
			p.logger = logger
			upstreamCalls := 0
			p.client.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
				upstreamCalls++
				return nil, errors.New("unexpected upstream request")
			})
			req := httptest.NewRequest("POST", "/anthropic/api.anthropic.com/v1/messages", strings.NewReader(`{"messages":[]}`))
			recorder := httptest.NewRecorder()
			p.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusInternalServerError || recorder.Body.String() != "platform-aws signing failed\n" {
				t.Errorf("response = %d %q, want opaque 500", recorder.Code, recorder.Body.String())
			}
			if upstreamCalls != 0 {
				t.Errorf("upstream calls = %d, want 0", upstreamCalls)
			}
			if got := fake.callCount(); got != len(fake.results) {
				t.Errorf("AssumeRole calls = %d, want %d", got, len(fake.results))
			}

			entries := readObservationLogEntries(t, logger)
			var requests []map[string]any
			for _, entry := range entries {
				if entry["type"] == "request" {
					requests = append(requests, entry)
				}
			}
			responses := filterResponseEntries(entries)
			if len(requests) != 1 || len(responses) != 1 {
				t.Fatalf("captured %d requests and %d responses, want one matched pair", len(requests), len(responses))
			}
			request, response := requests[0], responses[0]
			requestMeta := request["_meta"].(map[string]any)
			responseMeta := response["_meta"].(map[string]any)
			for _, key := range []string{"session", "request_id"} {
				if value, ok := requestMeta[key].(string); !ok || value == "" || responseMeta[key] != value {
					t.Errorf("capture %s does not match: request=%v response=%v", key, requestMeta[key], responseMeta[key])
				}
			}
			if response["seq"] != request["seq"] || response["path"] != "/v1/messages" {
				t.Errorf("response lost request sequence or path: %v", response)
			}
			if response["status"] != float64(0) || response["size"] != float64(0) || response["body"] != "" || response["chunks"] != nil {
				t.Errorf("response must record no upstream status or bytes: %v", response)
			}
			if response["termination"] != "upstream_unreachable" {
				t.Errorf("termination = %v, want upstream_unreachable", response["termination"])
			}
			if diagnostic, ok := response["termination_error"].(string); !ok || !strings.Contains(diagnostic, tc.wantDiagnostic) {
				t.Errorf("capture diagnostic = %v, want %s", response["termination_error"], tc.wantDiagnostic)
			}
			if !strings.Contains(diagnostics.String(), tc.wantDiagnostic) || !strings.Contains(diagnostics.String(), requestMeta["request_id"].(string)) {
				t.Errorf("operational diagnostic must include failure category and request ID: %q", diagnostics.String())
			}
			_, captureData := readObservationLogFile(t, logger)
			for _, secret := range []string{secretCanary, "ASIAEXPIRED", "assumed-secret", "assumed-session-token"} {
				if strings.Contains(string(captureData), secret) || strings.Contains(diagnostics.String(), secret) || strings.Contains(recorder.Body.String(), secret) {
					t.Errorf("credential failure leaked %q into capture, diagnostics, or client output", secret)
				}
			}
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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
	req.Header.Set("Anthropic-Workspace-Id", "wrkspc_client_override")
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

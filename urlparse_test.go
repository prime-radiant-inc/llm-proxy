// urlparse_test.go
package main

import (
	"strings"
	"testing"
)

func TestParseProxyURL(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantProv string
		wantUp   string
		wantPath string
		wantErr  bool
	}{
		{
			name:     "anthropic basic",
			path:     "/anthropic/api.anthropic.com/v1/messages",
			wantProv: "anthropic",
			wantUp:   "api.anthropic.com",
			wantPath: "/v1/messages",
		},
		{
			name:     "openai basic",
			path:     "/openai/api.openai.com/v1/chat/completions",
			wantProv: "openai",
			wantUp:   "api.openai.com",
			wantPath: "/v1/chat/completions",
		},
		{
			name:     "anthropic token count",
			path:     "/anthropic/api.anthropic.com/v1/messages/count_tokens",
			wantProv: "anthropic",
			wantUp:   "api.anthropic.com",
			wantPath: "/v1/messages/count_tokens",
		},
		{
			name:    "missing provider",
			path:    "/api.anthropic.com/v1/messages",
			wantErr: true,
		},
		{
			name:    "empty path",
			path:    "/",
			wantErr: true,
		},
		{
			name:    "health endpoint passthrough",
			path:    "/health",
			wantErr: true,
		},
		{
			name:    "reject upstream dot dot",
			path:    "/anthropic/../v1/messages",
			wantErr: true,
		},
		{
			name:     "chatgpt backend api",
			path:     "/openai/chatgpt.com/backend-api/codex/v1/responses",
			wantProv: "openai",
			wantUp:   "chatgpt.com",
			wantPath: "/backend-api/codex/v1/responses",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov, upstream, path, err := ParseProxyURL(tt.path)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if prov != tt.wantProv {
				t.Errorf("provider: got %q, want %q", prov, tt.wantProv)
			}
			if upstream != tt.wantUp {
				t.Errorf("upstream: got %q, want %q", upstream, tt.wantUp)
			}
			if path != tt.wantPath {
				t.Errorf("path: got %q, want %q", path, tt.wantPath)
			}
		})
	}
}

func TestParseRunEnvelope(t *testing.T) {
	tests := []struct {
		name             string
		escapedPath      string
		wantOK           bool
		wantRunID        string
		wantInnerEscaped string
		wantInner        string
	}{
		{"bedrock", "/runs/proj-20260628-abc/model/us.anthropic.claude:0/invoke", true, "proj-20260628-abc", "/model/us.anthropic.claude:0/invoke", "/model/us.anthropic.claude:0/invoke"},
		{"mantle", "/runs/run-test/mantle/v1/responses", true, "run-test", "/mantle/v1/responses", "/mantle/v1/responses"},
		{"openai", "/runs/run-test/openai/api.openai.com/v1/responses", true, "run-test", "/openai/api.openai.com/v1/responses", "/openai/api.openai.com/v1/responses"},
		{"not envelope", "/model/foo/invoke", false, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := ParseRunEnvelope(tt.escapedPath)
			if err != nil {
				t.Fatalf("ParseRunEnvelope returned error: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got.RunID != tt.wantRunID || got.InnerEscapedPath != tt.wantInnerEscaped || got.InnerPath != tt.wantInner {
				t.Fatalf("envelope = %#v", got)
			}
		})
	}
}

func TestParseRunEnvelopeRejectsInvalidRunID(t *testing.T) {
	cases := []string{
		"/runs//model/x/invoke",
		"/runs/./model/x/invoke",
		"/runs/../model/x/invoke",
		"/runs/-bad/model/x/invoke",
		"/runs/run%2Fbad/model/x/invoke",
		"/runs/run%5Cbad/model/x/invoke",
		"/runs/run bad/model/x/invoke",
		"/runs/" + strings.Repeat("x", 129) + "/model/x/invoke",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			if _, ok, err := ParseRunEnvelope(path); !ok || err == nil {
				t.Fatalf("ParseRunEnvelope(%q) = ok=%v err=%v, want ok=true err", path, ok, err)
			}
		})
	}
}

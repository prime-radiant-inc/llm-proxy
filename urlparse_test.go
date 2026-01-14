// urlparse_test.go
package main

import (
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

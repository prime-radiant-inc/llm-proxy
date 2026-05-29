// config_test.go
package main

import (
	"os"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Port != 0 {
		t.Errorf("expected default port 0 (dynamic), got %d", cfg.Port)
	}
	if cfg.LogDir != "./logs" {
		t.Errorf("expected default log dir './logs', got %q", cfg.LogDir)
	}
}

func TestLoadConfigFromTOML(t *testing.T) {
	tomlContent := `
port = 9000
log_dir = "/var/log/llm-proxy"
`
	cfg, err := LoadConfigFromTOML([]byte(tomlContent))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9000 {
		t.Errorf("expected port 9000, got %d", cfg.Port)
	}
	if cfg.LogDir != "/var/log/llm-proxy" {
		t.Errorf("expected log dir '/var/log/llm-proxy', got %q", cfg.LogDir)
	}
}

func TestLoadConfigFromTOMLWithDefaults(t *testing.T) {
	tomlContent := `port = 9000`

	cfg, err := LoadConfigFromTOML([]byte(tomlContent))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9000 {
		t.Errorf("expected port 9000, got %d", cfg.Port)
	}
	if cfg.LogDir != "./logs" {
		t.Errorf("expected default log dir './logs', got %q", cfg.LogDir)
	}
}

func TestDefaultConfig_LokiDefaults(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Loki.Enabled != false {
		t.Errorf("expected Loki.Enabled false, got %v", cfg.Loki.Enabled)
	}
	if cfg.Loki.URL != "" {
		t.Errorf("expected Loki.URL empty, got %q", cfg.Loki.URL)
	}
	if cfg.Loki.AuthToken != "" {
		t.Errorf("expected Loki.AuthToken empty, got %q", cfg.Loki.AuthToken)
	}
	if cfg.Loki.BatchSize != 1000 {
		t.Errorf("expected Loki.BatchSize 1000, got %d", cfg.Loki.BatchSize)
	}
	if cfg.Loki.BatchWaitStr != "5s" {
		t.Errorf("expected Loki.BatchWaitStr '5s', got %q", cfg.Loki.BatchWaitStr)
	}
	if cfg.Loki.RetryMax != 5 {
		t.Errorf("expected Loki.RetryMax 5, got %d", cfg.Loki.RetryMax)
	}
	if cfg.Loki.UseGzip != true {
		t.Errorf("expected Loki.UseGzip true, got %v", cfg.Loki.UseGzip)
	}
	if cfg.Loki.Environment != "development" {
		t.Errorf("expected Loki.Environment 'development', got %q", cfg.Loki.Environment)
	}
}

func TestLoadConfigFromEnv_LokiEnabled(t *testing.T) {
	// Setup: set environment variable
	os.Setenv("LLM_PROXY_LOKI_ENABLED", "true")
	defer os.Unsetenv("LLM_PROXY_LOKI_ENABLED")

	cfg := LoadConfigFromEnv(DefaultConfig())

	if cfg.Loki.Enabled != true {
		t.Errorf("expected Loki.Enabled true, got %v", cfg.Loki.Enabled)
	}
}

func TestLoadConfigFromEnv_LokiURL(t *testing.T) {
	testURL := "http://loki.example.com:3100/loki/api/v1/push"
	os.Setenv("LLM_PROXY_LOKI_URL", testURL)
	defer os.Unsetenv("LLM_PROXY_LOKI_URL")

	cfg := LoadConfigFromEnv(DefaultConfig())

	if cfg.Loki.URL != testURL {
		t.Errorf("expected Loki.URL %q, got %q", testURL, cfg.Loki.URL)
	}
}

func TestLoadConfigFromEnv_LokiAuthToken(t *testing.T) {
	testToken := "secret-token-123"
	os.Setenv("LLM_PROXY_LOKI_AUTH_TOKEN", testToken)
	defer os.Unsetenv("LLM_PROXY_LOKI_AUTH_TOKEN")

	cfg := LoadConfigFromEnv(DefaultConfig())

	if cfg.Loki.AuthToken != testToken {
		t.Errorf("expected Loki.AuthToken %q, got %q", testToken, cfg.Loki.AuthToken)
	}
}

func TestLoadConfigFromEnv_LokiBatchSize(t *testing.T) {
	os.Setenv("LLM_PROXY_LOKI_BATCH_SIZE", "500")
	defer os.Unsetenv("LLM_PROXY_LOKI_BATCH_SIZE")

	cfg := LoadConfigFromEnv(DefaultConfig())

	if cfg.Loki.BatchSize != 500 {
		t.Errorf("expected Loki.BatchSize 500, got %d", cfg.Loki.BatchSize)
	}
}

func TestLoadConfigFromEnv_BedrockRegion(t *testing.T) {
	t.Setenv("BEDROCK_REGION", "us-west-2")

	cfg := LoadConfigFromEnv(DefaultConfig())

	if cfg.BedrockRegion != "us-west-2" {
		t.Errorf("expected BedrockRegion 'us-west-2', got %q", cfg.BedrockRegion)
	}
}

func TestValidateBedrockRegion(t *testing.T) {
	tests := []struct {
		region  string
		wantErr bool
	}{
		{"", false},           // empty = disabled, valid
		{"us-west-2", false},  // supported
		{"us-east-1", false},  // supported
		{"us-east-2", false},  // supported
		{"us-west-1", true},   // Bedrock not available
		{"eu-west-1", true},   // not in our approved list
		{"ap-southeast-1", true},
	}

	for _, tt := range tests {
		t.Run(tt.region, func(t *testing.T) {
			err := ValidateBedrockRegion(tt.region)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateBedrockRegion(%q) error = %v, wantErr %v", tt.region, err, tt.wantErr)
			}
		})
	}
}

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

func TestLoadConfigFromTOML_LokiSection(t *testing.T) {
	tomlContent := `
port = 12071

[loki]
enabled = true
url = "http://loki:3100/loki/api/v1/push"
auth_token = "my-token"
batch_size = 2000
batch_wait = "10s"
retry_max = 3
use_gzip = false
environment = "production"
`
	cfg, err := LoadConfigFromTOML([]byte(tomlContent))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Loki.Enabled != true {
		t.Errorf("expected Loki.Enabled true, got %v", cfg.Loki.Enabled)
	}
	if cfg.Loki.URL != "http://loki:3100/loki/api/v1/push" {
		t.Errorf("expected Loki.URL 'http://loki:3100/loki/api/v1/push', got %q", cfg.Loki.URL)
	}
	if cfg.Loki.AuthToken != "my-token" {
		t.Errorf("expected Loki.AuthToken 'my-token', got %q", cfg.Loki.AuthToken)
	}
	if cfg.Loki.BatchSize != 2000 {
		t.Errorf("expected Loki.BatchSize 2000, got %d", cfg.Loki.BatchSize)
	}
	if cfg.Loki.BatchWaitStr != "10s" {
		t.Errorf("expected Loki.BatchWaitStr '10s', got %q", cfg.Loki.BatchWaitStr)
	}
	if cfg.Loki.RetryMax != 3 {
		t.Errorf("expected Loki.RetryMax 3, got %d", cfg.Loki.RetryMax)
	}
	if cfg.Loki.UseGzip != false {
		t.Errorf("expected Loki.UseGzip false, got %v", cfg.Loki.UseGzip)
	}
	if cfg.Loki.Environment != "production" {
		t.Errorf("expected Loki.Environment 'production', got %q", cfg.Loki.Environment)
	}
}

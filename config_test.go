// config_test.go
package main

import (
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Port != 8080 {
		t.Errorf("expected default port 8080, got %d", cfg.Port)
	}
	if cfg.LogDir != "./logs" {
		t.Errorf("expected default log dir './logs', got %q", cfg.LogDir)
	}
}

func TestLoadConfigFromTOML(t *testing.T) {
	tomlContent := `
port = 9000
log_dir = "/var/log/agent-logger"
`
	cfg, err := LoadConfigFromTOML([]byte(tomlContent))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9000 {
		t.Errorf("expected port 9000, got %d", cfg.Port)
	}
	if cfg.LogDir != "/var/log/agent-logger" {
		t.Errorf("expected log dir '/var/log/agent-logger', got %q", cfg.LogDir)
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

// main_test.go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCLIFlags(t *testing.T) {
	args := []string{"--port", "9001", "--log-dir", "/tmp/logs"}

	flags, err := ParseCLIFlags(args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if flags.Port != 9001 {
		t.Errorf("expected port 9001, got %d", flags.Port)
	}
	if flags.LogDir != "/tmp/logs" {
		t.Errorf("expected log dir '/tmp/logs', got %q", flags.LogDir)
	}
}

func TestParseCLIFlagsDefaults(t *testing.T) {
	args := []string{}

	flags, err := ParseCLIFlags(args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if flags.Port != 0 {
		t.Errorf("expected port 0 (unset), got %d", flags.Port)
	}
}

func TestParseCLIFlagsEnv(t *testing.T) {
	args := []string{"--env"}

	flags, err := ParseCLIFlags(args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !flags.Env {
		t.Error("expected Env flag to be true")
	}
}

func TestParseCLIFlagsSetup(t *testing.T) {
	args := []string{"--setup"}

	flags, err := ParseCLIFlags(args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !flags.Setup {
		t.Error("expected Setup flag to be true")
	}
}

func TestApplyRuntimeDefaults(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	wantLogDir := filepath.Join(home, ".llm-provider-logs")

	tests := []struct {
		name       string
		cfg        Config
		flags      CLIFlags
		wantLogDir string
		wantPort   int
	}{
		{
			name:       "non-service mode, no flags: home log dir, default port",
			cfg:        Config{LogDir: "./logs", Port: 0},
			flags:      CLIFlags{},
			wantLogDir: wantLogDir,
			wantPort:   12071,
		},
		{
			name:       "service mode, no flags: home log dir, dynamic port",
			cfg:        Config{LogDir: "./logs", Port: 0, ServiceMode: true},
			flags:      CLIFlags{},
			wantLogDir: wantLogDir,
			wantPort:   0,
		},
		{
			name:       "explicit --log-dir is preserved",
			cfg:        Config{LogDir: "/tmp/logs", Port: 0},
			flags:      CLIFlags{LogDir: "/tmp/logs"},
			wantLogDir: "/tmp/logs",
			wantPort:   12071,
		},
		{
			name:       "configured log dir is preserved",
			cfg:        Config{LogDir: "/var/lib/llm-proxy/provider-logs", LogDirConfigured: true, Port: 0},
			flags:      CLIFlags{},
			wantLogDir: "/var/lib/llm-proxy/provider-logs",
			wantPort:   12071,
		},
		{
			name:       "explicit --port is preserved",
			cfg:        Config{LogDir: "./logs", Port: 9000},
			flags:      CLIFlags{},
			wantLogDir: wantLogDir,
			wantPort:   9000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyRuntimeDefaults(tt.cfg, tt.flags)
			if got.LogDir != tt.wantLogDir {
				t.Errorf("LogDir: got %q, want %q", got.LogDir, tt.wantLogDir)
			}
			if got.Port != tt.wantPort {
				t.Errorf("Port: got %d, want %d", got.Port, tt.wantPort)
			}
		})
	}
}

func TestParseCLIFlagsUninstall(t *testing.T) {
	args := []string{"--uninstall"}

	flags, err := ParseCLIFlags(args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !flags.Uninstall {
		t.Error("expected Uninstall flag to be true")
	}
}

func TestListenAddr(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
	}{
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

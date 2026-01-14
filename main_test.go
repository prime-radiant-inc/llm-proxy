// main_test.go
package main

import (
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

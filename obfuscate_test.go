// obfuscate_test.go
package main

import (
	"net/http"
	"testing"
)

func TestObfuscateAPIKey(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "anthropic key",
			input:    "sk-ant-api03-abcdefghijklmnopqrstuvwxyz",
			expected: "sk-ant-...wxyz",
		},
		{
			name:     "openai key",
			input:    "sk-proj-abcdefghijklmnopqrstuvwxyz1234",
			expected: "sk-proj-...1234",
		},
		{
			name:     "short key",
			input:    "sk-abc",
			expected: "sk-...",
		},
		{
			name:     "empty",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ObfuscateAPIKey(tt.input)
			if result != tt.expected {
				t.Errorf("got %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestObfuscateHeaders(t *testing.T) {
	headers := http.Header{
		"X-Api-Key":         []string{"sk-ant-api03-secretkey12345678"},
		"Authorization":     []string{"******"},
		"Set-Cookie":        []string{"session=abc123"},
		"Content-Type":      []string{"application/json"},
		"Anthropic-Version": []string{"2023-06-01"},
	}

	result := ObfuscateHeaders(headers)

	if result.Get("X-Api-Key") != "sk-ant-...5678" {
		t.Errorf("X-Api-Key not obfuscated correctly: %s", result.Get("X-Api-Key"))
	}
	if got := result.Get("Authorization"); got != "******" && got != redactedSecretValue {
		t.Errorf("Authorization not obfuscated correctly: %s", got)
	}
	if result.Get("Set-Cookie") != redactedSecretValue {
		t.Errorf("Set-Cookie not redacted correctly: %s", result.Get("Set-Cookie"))
	}
	if result.Get("Content-Type") != "application/json" {
		t.Error("Content-Type should not be modified")
	}
}

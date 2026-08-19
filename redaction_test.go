package main

import (
	"strings"
	"testing"
	"time"
)

func TestRedactLoggedStringJSONFieldsAndInlineTokens(t *testing.T) {
	input := `{"access_token":"secret-access-token","messages":[{"role":"user","content":"token sk-ant-api03-abcdefghijklmnopqrstuvwxyz12345678"}],"nested":{"client_secret":"top-secret"},"max_tokens":128}`
	got := RedactLoggedString(input)

	for _, secret := range []string{"secret-access-token", "top-secret", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz12345678"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted body still contains %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, `"access_token":"[REDACTED]"`) {
		t.Fatalf("expected access_token field redaction, got %s", got)
	}
	if !strings.Contains(got, `"client_secret":"[REDACTED]"`) {
		t.Fatalf("expected client_secret field redaction, got %s", got)
	}
	if !strings.Contains(got, `"max_tokens":128`) {
		t.Fatalf("non-secret field should remain intact, got %s", got)
	}
}

func TestRedactLoggedChunks(t *testing.T) {
	chunks := []StreamChunk{
		{Timestamp: time.Now(), Raw: `data: {"access_token":"super-secret","text":"******"}`},
		{Timestamp: time.Now(), Raw: `data: [DONE]`},
	}
	got := RedactLoggedChunks(chunks)

	if strings.Contains(got[0].Raw, "super-secret") || strings.Contains(got[0].Raw, "abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("chunk was not redacted: %s", got[0].Raw)
	}
	if got[1].Raw != `data: [DONE]` {
		t.Fatalf("sentinel chunk changed unexpectedly: %q", got[1].Raw)
	}
}

func TestSanitizeURLForLog(t *testing.T) {
	got := sanitizeURLForLog("https://user" + ":" + "pass@loki.example.com/loki/api/v1/push?token=secret#frag")
	if got != "https://loki.example.com/loki/api/v1/push?token=secret#frag" {
		t.Fatalf("sanitizeURLForLog = %q", got)
	}
}

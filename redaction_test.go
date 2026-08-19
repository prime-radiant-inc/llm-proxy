package main

import (
	"net/url"
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
	got := sanitizeURLForLog("https://user" + ":" + "pass@loki.example.com/loki/api/v1/push?token=secret&stream=prod&note=Bearer+sk-ant-api03-abcdefghijklmnopqrstuvwxyz12345678#frag")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", got, err)
	}
	if parsed.User != nil {
		t.Fatalf("expected credentials to be stripped, got %v", parsed.User)
	}
	if parsed.Query().Get("token") != redactedSecretValue {
		t.Fatalf("expected sensitive query value to be redacted, got %q", parsed.Query().Get("token"))
	}
	if parsed.Query().Get("stream") != "prod" {
		t.Fatalf("expected non-sensitive query value to be preserved, got %q", parsed.Query().Get("stream"))
	}
	note := parsed.Query().Get("note")
	if strings.Contains(note, "sk-ant-api03-abcdefghijklmnopqrstuvwxyz12345678") {
		t.Fatalf("expected secret-like text in query value to be redacted, got %q", note)
	}
	if !strings.HasPrefix(note, "Bearer ") {
		t.Fatalf("expected query value context to be preserved, got %q", note)
	}
	if parsed.Fragment != "frag" {
		t.Fatalf("expected fragment to be preserved, got %q", parsed.Fragment)
	}
}

// fingerprint_test.go
package main

import (
	"testing"
)

func TestFingerprintMessages(t *testing.T) {
	// Same messages should produce same fingerprint
	messages1 := `[{"role":"user","content":"hello"}]`
	messages2 := `[{"role":"user","content":"hello"}]`

	fp1 := FingerprintMessages([]byte(messages1))
	fp2 := FingerprintMessages([]byte(messages2))

	if fp1 != fp2 {
		t.Errorf("Same messages should produce same fingerprint: %s != %s", fp1, fp2)
	}
}

func TestFingerprintDifferentMessages(t *testing.T) {
	messages1 := `[{"role":"user","content":"hello"}]`
	messages2 := `[{"role":"user","content":"goodbye"}]`

	fp1 := FingerprintMessages([]byte(messages1))
	fp2 := FingerprintMessages([]byte(messages2))

	if fp1 == fp2 {
		t.Error("Different messages should produce different fingerprints")
	}
}

func TestFingerprintIgnoresWhitespace(t *testing.T) {
	// These are semantically equivalent JSON
	messages1 := `[{"role":"user","content":"hello"}]`
	messages2 := `[ { "role" : "user" , "content" : "hello" } ]`

	fp1 := FingerprintMessages([]byte(messages1))
	fp2 := FingerprintMessages([]byte(messages2))

	if fp1 != fp2 {
		t.Errorf("Whitespace differences should not affect fingerprint: %s != %s", fp1, fp2)
	}
}

func TestFingerprintKeyOrder(t *testing.T) {
	// Different key order should produce same fingerprint
	messages1 := `[{"role":"user","content":"hello"}]`
	messages2 := `[{"content":"hello","role":"user"}]`

	fp1 := FingerprintMessages([]byte(messages1))
	fp2 := FingerprintMessages([]byte(messages2))

	if fp1 != fp2 {
		t.Errorf("Key order should not affect fingerprint: %s != %s", fp1, fp2)
	}
}

func TestExtractMessagesFromRequest(t *testing.T) {
	// Anthropic request format
	request := `{"model":"claude-3","messages":[{"role":"user","content":"test"}],"max_tokens":100}`

	messages, err := ExtractMessages([]byte(request), "anthropic")
	if err != nil {
		t.Fatalf("Failed to extract messages: %v", err)
	}

	if len(messages) != 1 {
		t.Errorf("Expected 1 message, got %d", len(messages))
	}
}

func TestExtractPriorMessages(t *testing.T) {
	// Should extract all but the last message for fingerprinting
	request := `{"model":"claude-3","messages":[
		{"role":"user","content":"first"},
		{"role":"assistant","content":"response"},
		{"role":"user","content":"second"}
	]}`

	prior, err := ExtractPriorMessages([]byte(request), "anthropic")
	if err != nil {
		t.Fatalf("Failed to extract prior: %v", err)
	}

	// Should only have first 2 messages
	if len(prior) != 2 {
		t.Errorf("Expected 2 prior messages, got %d", len(prior))
	}
}

func TestExtractAssistantMessageAnthropic(t *testing.T) {
	response := `{"content":[{"type":"text","text":"Hello there!"}],"model":"claude-3"}`

	msg, err := ExtractAssistantMessage([]byte(response), "anthropic")
	if err != nil {
		t.Fatalf("Failed to extract assistant message: %v", err)
	}

	if msg["role"] != "assistant" {
		t.Errorf("Expected role 'assistant', got %v", msg["role"])
	}
	if msg["content"] != "Hello there!" {
		t.Errorf("Expected content 'Hello there!', got %v", msg["content"])
	}
}

func TestExtractAssistantMessageOpenAI(t *testing.T) {
	response := `{"choices":[{"message":{"role":"assistant","content":"Hi!"}}]}`

	msg, err := ExtractAssistantMessage([]byte(response), "openai")
	if err != nil {
		t.Fatalf("Failed to extract assistant message: %v", err)
	}

	if msg["role"] != "assistant" {
		t.Errorf("Expected role 'assistant', got %v", msg["role"])
	}
	if msg["content"] != "Hi!" {
		t.Errorf("Expected content 'Hi!', got %v", msg["content"])
	}
}

func TestExtractAssistantMessageMalformed(t *testing.T) {
	// Should return error for malformed JSON
	_, err := ExtractAssistantMessage([]byte("not json"), "anthropic")
	if err == nil {
		t.Error("Expected error for malformed JSON")
	}

	// Should return error for missing content
	_, err = ExtractAssistantMessage([]byte(`{}`), "anthropic")
	if err == nil {
		t.Error("Expected error for missing content")
	}
}

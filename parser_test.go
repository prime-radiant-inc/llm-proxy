package main

import (
	"testing"
)

func TestParseClaudeRequest(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-20250514",
		"max_tokens": 8096,
		"messages": [
			{"role": "user", "content": "What is 2+2?"}
		]
	}`

	parsed := ParseRequestBody(body, "api.anthropic.com")

	if parsed.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Expected claude-sonnet-4-20250514, got %s", parsed.Model)
	}

	if len(parsed.Messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(parsed.Messages))
	}

	if parsed.Messages[0].Role != "user" {
		t.Errorf("Expected role user, got %s", parsed.Messages[0].Role)
	}

	if parsed.Messages[0].TextContent != "What is 2+2?" {
		t.Errorf("Expected 'What is 2+2?', got %s", parsed.Messages[0].TextContent)
	}
}

func TestParseClaudeResponse(t *testing.T) {
	body := `{
		"content": [
			{"type": "text", "text": "2+2 equals 4."}
		],
		"usage": {"input_tokens": 10, "output_tokens": 8}
	}`

	parsed := ParseResponseBody(body, "api.anthropic.com")

	if len(parsed.Content) != 1 {
		t.Fatalf("Expected 1 content block, got %d", len(parsed.Content))
	}

	if parsed.Content[0].Type != "text" {
		t.Errorf("Expected type text, got %s", parsed.Content[0].Type)
	}

	if parsed.Content[0].Text != "2+2 equals 4." {
		t.Errorf("Expected '2+2 equals 4.', got %s", parsed.Content[0].Text)
	}
}

func TestParseClaudeThinkingBlock(t *testing.T) {
	body := `{
		"content": [
			{"type": "thinking", "thinking": "Let me calculate this step by step..."},
			{"type": "text", "text": "The answer is 4."}
		]
	}`

	parsed := ParseResponseBody(body, "api.anthropic.com")

	if len(parsed.Content) != 2 {
		t.Fatalf("Expected 2 content blocks, got %d", len(parsed.Content))
	}

	if parsed.Content[0].Type != "thinking" {
		t.Errorf("Expected thinking block first")
	}

	if parsed.Content[0].Thinking != "Let me calculate this step by step..." {
		t.Error("Thinking content not parsed correctly")
	}
}

func TestParseClaudeToolUse(t *testing.T) {
	body := `{
		"content": [
			{"type": "text", "text": "I'll read that file."},
			{"type": "tool_use", "id": "tool_123", "name": "Read", "input": {"path": "/tmp/test.txt"}}
		]
	}`

	parsed := ParseResponseBody(body, "api.anthropic.com")

	if len(parsed.Content) != 2 {
		t.Fatalf("Expected 2 content blocks, got %d", len(parsed.Content))
	}

	toolBlock := parsed.Content[1]
	if toolBlock.Type != "tool_use" {
		t.Errorf("Expected tool_use, got %s", toolBlock.Type)
	}

	if toolBlock.ToolName != "Read" {
		t.Errorf("Expected tool name Read, got %s", toolBlock.ToolName)
	}
}

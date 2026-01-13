// fingerprint.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// FingerprintMessages computes a SHA256 hash of canonicalized messages
func FingerprintMessages(messagesJSON []byte) string {
	// Parse and re-serialize to canonical form
	var messages []map[string]interface{}
	if err := json.Unmarshal(messagesJSON, &messages); err != nil {
		// If we can't parse, hash the raw bytes
		hash := sha256.Sum256(messagesJSON)
		return hex.EncodeToString(hash[:])
	}

	// Canonicalize each message
	canonical := canonicalizeMessages(messages)

	// Serialize to JSON with sorted keys
	canonicalJSON, _ := json.Marshal(canonical)

	hash := sha256.Sum256(canonicalJSON)
	return hex.EncodeToString(hash[:])
}

func canonicalizeMessages(messages []map[string]interface{}) []map[string]interface{} {
	result := make([]map[string]interface{}, len(messages))
	for i, msg := range messages {
		result[i] = canonicalizeMap(msg)
	}
	return result
}

func canonicalizeMap(m map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	// Get sorted keys
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := m[k]
		switch val := v.(type) {
		case map[string]interface{}:
			result[k] = canonicalizeMap(val)
		case []interface{}:
			result[k] = canonicalizeSlice(val)
		default:
			result[k] = v
		}
	}
	return result
}

func canonicalizeSlice(s []interface{}) []interface{} {
	result := make([]interface{}, len(s))
	for i, v := range s {
		switch val := v.(type) {
		case map[string]interface{}:
			result[i] = canonicalizeMap(val)
		case []interface{}:
			result[i] = canonicalizeSlice(val)
		default:
			result[i] = v
		}
	}
	return result
}

// ExtractMessages extracts the messages array from a request body
func ExtractMessages(body []byte, provider string) ([]map[string]interface{}, error) {
	var request map[string]interface{}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}

	messagesKey := "messages" // Same for both Anthropic and OpenAI

	messagesRaw, ok := request[messagesKey]
	if !ok {
		return nil, nil
	}

	messagesSlice, ok := messagesRaw.([]interface{})
	if !ok {
		return nil, nil
	}

	messages := make([]map[string]interface{}, len(messagesSlice))
	for i, m := range messagesSlice {
		if msg, ok := m.(map[string]interface{}); ok {
			messages[i] = msg
		}
	}

	return messages, nil
}

// ExtractPriorMessages extracts all but the last message (for fingerprinting conversation state)
func ExtractPriorMessages(body []byte, provider string) ([]map[string]interface{}, error) {
	messages, err := ExtractMessages(body, provider)
	if err != nil {
		return nil, err
	}

	if len(messages) <= 1 {
		return nil, nil // No prior messages
	}

	return messages[:len(messages)-1], nil
}

// ComputePriorFingerprint computes fingerprint of conversation state before current message
func ComputePriorFingerprint(body []byte, provider string) (string, error) {
	prior, err := ExtractPriorMessages(body, provider)
	if err != nil {
		return "", err
	}

	if prior == nil {
		return "", nil // First message, no prior state
	}

	priorJSON, err := json.Marshal(prior)
	if err != nil {
		return "", err
	}

	return FingerprintMessages(priorJSON), nil
}

// ExtractAssistantMessage extracts the assistant's response from API response body
func ExtractAssistantMessage(responseBody []byte, provider string) (map[string]interface{}, error) {
	var resp map[string]interface{}
	if err := json.Unmarshal(responseBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if provider == "anthropic" {
		// Anthropic: {"content": [{"type": "text", "text": "..."}], ...}
		content, ok := resp["content"].([]interface{})
		if !ok || len(content) == 0 {
			return nil, fmt.Errorf("missing or empty content in response")
		}
		block, ok := content[0].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid content block format")
		}
		text, _ := block["text"].(string)
		return map[string]interface{}{
			"role":    "assistant",
			"content": text,
		}, nil
	} else if provider == "openai" {
		// OpenAI: {"choices": [{"message": {"role": "assistant", "content": "..."}}]}
		choices, ok := resp["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			return nil, fmt.Errorf("missing or empty choices in response")
		}
		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid choice format")
		}
		message, ok := choice["message"].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("missing message in choice")
		}
		return message, nil
	}

	return nil, fmt.Errorf("unsupported provider: %s", provider)
}

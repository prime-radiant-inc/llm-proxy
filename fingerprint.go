// fingerprint.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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

// Keys to exclude from fingerprinting (metadata that doesn't affect semantic content)
var fingerprintExcludeKeys = map[string]bool{
	"cache_control": true, // Claude Code adds this to follow-up requests
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
		// Skip keys that don't affect semantic content
		if fingerprintExcludeKeys[k] {
			continue
		}
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

// ExtractClientSessionID extracts a client-provided session ID from the request.
// For Anthropic, this is found in metadata.user_id with format:
//
//	user_<hash>_account_<uuid>_session_<session-uuid>
//
// For OpenAI, priority order:
//  1. conversation (Responses API)
//  2. previous_response_id (Responses API chaining)
//  3. metadata.session_id
//  4. X-Session-ID header
//  5. X-Client-Request-Id header
//  6. user field
//
// Returns empty string if no session ID is found.
func ExtractClientSessionID(body []byte, provider string, headers http.Header) string {
	var request map[string]interface{}
	if err := json.Unmarshal(body, &request); err != nil {
		return ""
	}

	if provider == "anthropic" {
		return extractAnthropicSessionID(request)
	}

	if provider == "openai" {
		return extractOpenAISessionID(request, headers)
	}

	return ""
}

// extractAnthropicSessionID extracts session ID from Anthropic's metadata.user_id
func extractAnthropicSessionID(request map[string]interface{}) string {
	metadata, ok := request["metadata"].(map[string]interface{})
	if !ok {
		return ""
	}

	userID, ok := metadata["user_id"].(string)
	if !ok || userID == "" {
		return ""
	}

	// Extract session ID from user_id
	// Format: user_<hash>_account_<uuid>_session_<session-uuid>
	// We want the part after the last "_session_"
	const sessionMarker = "_session_"
	idx := lastIndex(userID, sessionMarker)
	if idx == -1 {
		// No session marker found - can't extract session ID
		return ""
	}

	sessionID := userID[idx+len(sessionMarker):]

	// Validate session ID: must be non-empty and contain only safe characters
	if !isValidSessionID(sessionID) {
		return ""
	}

	return sessionID
}

// extractOpenAISessionID extracts session ID from OpenAI request fields and headers
// Priority: conversation > previous_response_id > metadata.session_id > X-Session-ID > X-Client-Request-Id > user
func extractOpenAISessionID(request map[string]interface{}, headers http.Header) string {
	// 1. conversation (Responses API)
	if conv, ok := request["conversation"].(string); ok && conv != "" {
		if isValidSessionID(conv) {
			return conv
		}
	}

	// 2. previous_response_id (Responses API chaining)
	if prevResp, ok := request["previous_response_id"].(string); ok && prevResp != "" {
		if isValidSessionID(prevResp) {
			return prevResp
		}
	}

	// 3. metadata.session_id
	if metadata, ok := request["metadata"].(map[string]interface{}); ok {
		if sessID, ok := metadata["session_id"].(string); ok && sessID != "" {
			if isValidSessionID(sessID) {
				return sessID
			}
		}
	}

	// 4. X-Session-ID header
	if headers != nil {
		if sessID := headers.Get("X-Session-ID"); sessID != "" {
			if isValidSessionID(sessID) {
				return sessID
			}
		}

		// 5. X-Client-Request-Id header
		if clientReq := headers.Get("X-Client-Request-Id"); clientReq != "" {
			if isValidSessionID(clientReq) {
				return clientReq
			}
		}
	}

	// 6. user field
	if user, ok := request["user"].(string); ok && user != "" {
		if isValidSessionID(user) {
			return user
		}
	}

	return ""
}

// lastIndex returns the index of the last occurrence of substr in s, or -1 if not found
func lastIndex(s, substr string) int {
	for i := len(s) - len(substr); i >= 0; i-- {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// isValidSessionID checks if a session ID is safe to use
// Only allows alphanumeric characters, hyphens, and underscores
func isValidSessionID(id string) bool {
	if id == "" || len(id) > 255 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// ExtractAssistantMessage extracts the assistant's response from API response body
// Preserves the original content structure (array for Anthropic) to match follow-up requests
func ExtractAssistantMessage(responseBody []byte, provider string) (map[string]interface{}, error) {
	var resp map[string]interface{}
	if err := json.Unmarshal(responseBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if provider == "anthropic" {
		// Anthropic response: {"role": "assistant", "content": [{"type": "text", "text": "..."}], ...}
		// Preserve content as array to match how Claude Code sends it in follow-up requests
		content, ok := resp["content"].([]interface{})
		if !ok || len(content) == 0 {
			return nil, fmt.Errorf("missing or empty content in response")
		}
		return map[string]interface{}{
			"role":    "assistant",
			"content": content,
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

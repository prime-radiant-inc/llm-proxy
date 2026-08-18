package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

const redactedSecretValue = "[REDACTED]"

var (
	bearerTokenPattern = regexp.MustCompile(`(?i)\bBearer\s+([A-Za-z0-9._-]{8,})`)
	secretTextPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\bsk-ant(?:-api\d+)?-[A-Za-z0-9_-]{10,}\b`),
		regexp.MustCompile(`\bsk-proj-[A-Za-z0-9_-]{10,}\b`),
		regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	}
)

func RedactLoggedBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	return RedactLoggedString(string(body))
}

func RedactLoggedString(input string) string {
	if input == "" {
		return ""
	}
	if redacted, ok := redactJSONDocument(input); ok {
		return redacted
	}
	return redactSecretLikeText(input)
}

func RedactLoggedChunks(chunks []StreamChunk) []StreamChunk {
	if chunks == nil {
		return nil
	}
	redacted := make([]StreamChunk, len(chunks))
	for i, chunk := range chunks {
		redacted[i] = chunk
		redacted[i].Raw = redactLoggedChunkRaw(chunk.Raw)
	}
	return redacted
}

func redactLoggedChunkRaw(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "data:") {
		payload := strings.TrimPrefix(raw, "data:")
		trimmed := strings.TrimLeft(payload, " \t")
		padding := payload[:len(payload)-len(trimmed)]
		return "data:" + padding + RedactLoggedString(trimmed)
	}
	return RedactLoggedString(raw)
}

func redactJSONDocument(input string) (string, bool) {
	var value interface{}
	if err := json.Unmarshal([]byte(input), &value); err != nil {
		return "", false
	}
	redacted, err := json.Marshal(redactJSONObject(value))
	if err != nil {
		return "", false
	}
	return string(redacted), true
}

func redactJSONObject(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		redacted := make(map[string]interface{}, len(v))
		for key, item := range v {
			if isSensitiveJSONField(key) {
				redacted[key] = redactedSecretValue
				continue
			}
			redacted[key] = redactJSONObject(item)
		}
		return redacted
	case []interface{}:
		redacted := make([]interface{}, len(v))
		for i, item := range v {
			redacted[i] = redactJSONObject(item)
		}
		return redacted
	case string:
		return redactSecretLikeText(v)
	default:
		return value
	}
}

func isSensitiveJSONField(name string) bool {
	normalized := normalizeSecretFieldName(name)
	switch normalized {
	case "authorization", "proxyauthorization", "password", "passwd", "pwd", "privatekey", "credentials", "cookie", "setcookie":
		return true
	}
	if strings.Contains(normalized, "secret") {
		return true
	}
	if strings.HasSuffix(normalized, "token") {
		return true
	}
	if strings.HasSuffix(normalized, "apikey") {
		return true
	}
	return false
}

func normalizeSecretFieldName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func redactSecretLikeText(input string) string {
	redacted := bearerTokenPattern.ReplaceAllStringFunc(input, func(match string) string {
		parts := strings.SplitN(match, " ", 2)
		if len(parts) != 2 {
			return redactedSecretValue
		}
		return parts[0] + " " + redactTokenValue(parts[1])
	})
	for _, pattern := range secretTextPatterns {
		redacted = pattern.ReplaceAllStringFunc(redacted, redactTokenValue)
	}
	return redacted
}

func redactTokenValue(token string) string {
	if token == "" {
		return redactedSecretValue
	}
	if obfuscated := ObfuscateAPIKey(token); obfuscated != "" && obfuscated != "..." {
		return obfuscated
	}
	return redactedSecretValue
}

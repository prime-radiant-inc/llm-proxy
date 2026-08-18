// obfuscate.go
package main

import (
	"net/http"
	"strings"
)

// ObfuscateAPIKey returns an obfuscated version showing prefix and last 4 chars
func ObfuscateAPIKey(key string) string {
	if key == "" {
		return ""
	}

	// Find the prefix (normalized to just the base prefix like sk-ant- or sk-proj-)
	prefix := extractPrefix(key)

	// Only show last 4 characters if the key is long enough
	// that revealing 4 chars doesn't expose too much
	suffix := ""
	if len(key) > len(prefix)+8 {
		suffix = key[len(key)-4:]
	}

	return prefix + "..." + suffix
}

func extractPrefix(key string) string {
	// Map full prefixes to their normalized (shorter) display forms
	// sk-ant-api03- and sk-ant- both normalize to sk-ant-
	prefixMappings := []struct {
		match  string
		output string
	}{
		{"sk-ant-api03-", "sk-ant-"},
		{"sk-ant-", "sk-ant-"},
		{"sk-proj-", "sk-proj-"},
		{"sk-", "sk-"},
	}

	for _, pm := range prefixMappings {
		if strings.HasPrefix(key, pm.match) {
			return pm.output
		}
	}

	// Fallback: use first segment
	if idx := strings.Index(key, "-"); idx > 0 {
		return key[:idx+1]
	}

	return ""
}

// ObfuscateHeaders returns a copy of headers with API keys obfuscated
func ObfuscateHeaders(headers http.Header) http.Header {
	result := make(http.Header)

	for key, values := range headers {
		newValues := make([]string, len(values))
		for i, v := range values {
			if isAPIKeyHeader(key) {
				newValues[i] = obfuscateHeaderValue(v)
			} else {
				newValues[i] = v
			}
		}
		result[key] = newValues
	}

	return result
}

func isAPIKeyHeader(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "authorization", "proxy-authorization", "x-api-key", "x-auth-token", "x-access-token", "x-amz-security-token":
		return true
	case "cookie", "set-cookie":
		return true
	default:
		return false
	}
}

func obfuscateHeaderValue(value string) string {
	if value == "******" || value == redactedSecretValue {
		return value
	}
	if strings.Contains(value, "=") && !strings.HasPrefix(value, "Bearer ") {
		return redactedSecretValue
	}
	if strings.HasPrefix(value, "Bearer ") {
		token := strings.TrimPrefix(value, "Bearer ")
		if token == "******" || token == redactedSecretValue {
			return "Bearer " + token
		}
		return "Bearer " + redactTokenValue(token)
	}
	return redactTokenValue(value)
}

// e2e_test.go
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// loadAPIKey loads the Anthropic API key from the keys file
func loadAPIKey(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile("/home/jesse/.amplifier/keys.env")
	if err != nil {
		t.Skipf("Skipping live test: cannot read keys file: %v", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "ANTHROPIC_API_KEY=") {
			key := strings.TrimPrefix(line, "ANTHROPIC_API_KEY=")
			// Remove surrounding quotes if present
			key = strings.Trim(key, "\"")
			return key
		}
	}

	t.Skip("Skipping live test: ANTHROPIC_API_KEY not found in keys file")
	return ""
}

func TestLiveAnthropicProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping live test in short mode")
	}

	apiKey := loadAPIKey(t)

	// Start our proxy server
	tmpDir := t.TempDir()
	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()
	proxy := httptest.NewServer(srv)
	defer proxy.Close()

	// Build request through our proxy
	proxyURL := proxy.URL + "/anthropic/api.anthropic.com/v1/messages"

	requestBody := map[string]interface{}{
		"model":      "claude-3-haiku-20240307",
		"max_tokens": 10,
		"messages": []map[string]string{
			{"role": "user", "content": "Say 'test' and nothing else."},
		},
	}

	bodyBytes, _ := json.Marshal(requestBody)
	req, err := http.NewRequest("POST", proxyURL, bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	// Make request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	// Verify response structure
	var response map[string]interface{}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if _, ok := response["id"]; !ok {
		t.Error("Response missing 'id' field")
	}
	if _, ok := response["content"]; !ok {
		t.Error("Response missing 'content' field")
	}

	t.Logf("Live proxy test successful! Response ID: %v", response["id"])
}

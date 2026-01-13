// e2e_test.go
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestLiveAnthropicProxyWithLogging(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping live test in short mode")
	}

	apiKey := loadAPIKey(t)
	tmpDir := t.TempDir()

	// Start our proxy server with logging
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
			{"role": "user", "content": "Say 'logged' and nothing else."},
		},
	}

	bodyBytes, _ := json.Marshal(requestBody)
	req, _ := http.NewRequest("POST", proxyURL, bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

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

	// Give logger a moment to flush
	time.Sleep(100 * time.Millisecond)

	// Verify logs were created
	logFiles, _ := filepath.Glob(filepath.Join(tmpDir, "anthropic", "*.jsonl"))
	if len(logFiles) == 0 {
		t.Fatal("No log files created")
	}

	logData, _ := os.ReadFile(logFiles[0])
	logContent := string(logData)

	// Verify log contents
	if !strings.Contains(logContent, `"type":"session_start"`) {
		t.Error("Missing session_start in log")
	}
	if !strings.Contains(logContent, `"type":"request"`) {
		t.Error("Missing request in log")
	}
	if !strings.Contains(logContent, `"type":"response"`) {
		t.Error("Missing response in log")
	}

	// Verify API key was obfuscated
	if strings.Contains(logContent, apiKey) {
		t.Error("API key was not obfuscated in log!")
	}
	if !strings.Contains(logContent, "sk-ant-...") {
		t.Error("Obfuscated API key format not found")
	}

	// Verify timing was captured
	if !strings.Contains(logContent, `"ttfb_ms"`) {
		t.Error("TTFB timing not captured")
	}

	t.Logf("Live proxy with logging test successful!")
	t.Logf("Log file: %s", logFiles[0])
}

func TestLiveAnthropicStreamingProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping live test in short mode")
	}

	apiKey := loadAPIKey(t)
	tmpDir := t.TempDir()

	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	proxy := httptest.NewServer(srv)
	defer proxy.Close()

	proxyURL := proxy.URL + "/anthropic/api.anthropic.com/v1/messages"

	requestBody := map[string]interface{}{
		"model":      "claude-3-haiku-20240307",
		"max_tokens": 50,
		"stream":     true,
		"messages": []map[string]string{
			{"role": "user", "content": "Count from 1 to 5, one number per line."},
		},
	}

	bodyBytes, _ := json.Marshal(requestBody)
	req, _ := http.NewRequest("POST", proxyURL, bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	// Verify it's streaming
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/event-stream") {
		t.Errorf("Expected text/event-stream, got %s", contentType)
	}

	// Read streaming response
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "event:") && !strings.Contains(bodyStr, "data:") {
		t.Error("Response should contain SSE events")
	}

	// Give logger time to flush
	time.Sleep(100 * time.Millisecond)

	// Verify logs capture chunks
	logFiles, _ := filepath.Glob(filepath.Join(tmpDir, "anthropic", "*.jsonl"))
	if len(logFiles) == 0 {
		t.Fatal("No log files created")
	}

	logData, _ := os.ReadFile(logFiles[0])
	logContent := string(logData)

	if !strings.Contains(logContent, `"chunks"`) {
		t.Error("Log should contain chunks array for streaming response")
	}
	if !strings.Contains(logContent, `"delta_ms"`) {
		t.Error("Log should contain delta_ms timing for chunks")
	}

	t.Logf("Live streaming proxy test successful!")
}

func TestLiveMultiTurnConversation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping live test in short mode")
	}

	apiKey := loadAPIKey(t)
	tmpDir := t.TempDir()

	srv, err := NewServer(Config{Port: 8080, LogDir: tmpDir})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Close()

	proxy := httptest.NewServer(srv)
	defer proxy.Close()

	proxyURL := proxy.URL + "/anthropic/api.anthropic.com/v1/messages"
	client := &http.Client{}

	// Turn 1
	turn1 := map[string]interface{}{
		"model":      "claude-3-haiku-20240307",
		"max_tokens": 50,
		"messages": []map[string]string{
			{"role": "user", "content": "Remember the number 42. Just say 'OK'."},
		},
	}

	resp1 := makeRequest(t, client, proxyURL, apiKey, turn1)

	// Extract assistant response
	var result1 map[string]interface{}
	json.Unmarshal(resp1, &result1)
	content1 := extractTextContent(result1)

	t.Logf("Turn 1 response: %s", content1)

	// Turn 2 - continuation
	turn2 := map[string]interface{}{
		"model":      "claude-3-haiku-20240307",
		"max_tokens": 50,
		"messages": []map[string]string{
			{"role": "user", "content": "Remember the number 42. Just say 'OK'."},
			{"role": "assistant", "content": content1},
			{"role": "user", "content": "What number did I ask you to remember?"},
		},
	}

	resp2 := makeRequest(t, client, proxyURL, apiKey, turn2)

	var result2 map[string]interface{}
	json.Unmarshal(resp2, &result2)
	content2 := extractTextContent(result2)

	t.Logf("Turn 2 response: %s", content2)

	if !strings.Contains(content2, "42") {
		t.Errorf("Expected response to contain '42', got: %s", content2)
	}

	// Give logger time
	time.Sleep(200 * time.Millisecond)

	// Verify session tracking worked
	logFiles, _ := filepath.Glob(filepath.Join(tmpDir, "anthropic", "*.jsonl"))
	t.Logf("Created %d log files", len(logFiles))

	// Read and display log contents for debugging
	for _, f := range logFiles {
		data, _ := os.ReadFile(f)
		t.Logf("Log file %s:\n%s", filepath.Base(f), string(data))
	}

	// Verify we have session entries
	if len(logFiles) == 0 {
		t.Fatal("No log files created")
	}

	// Verify DB has fingerprints
	db, _ := NewSessionDB(filepath.Join(tmpDir, "sessions.db"))
	defer db.Close()

	// Count sessions
	var count int
	row := db.db.QueryRow("SELECT COUNT(*) FROM sessions")
	row.Scan(&count)
	t.Logf("Total sessions in DB: %d", count)
}

func makeRequest(t *testing.T, client *http.Client, url, apiKey string, body interface{}) []byte {
	t.Helper()

	bodyBytes, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody
}

func extractTextContent(response map[string]interface{}) string {
	content, ok := response["content"].([]interface{})
	if !ok || len(content) == 0 {
		return ""
	}

	block, ok := content[0].(map[string]interface{})
	if !ok {
		return ""
	}

	text, _ := block["text"].(string)
	return text
}

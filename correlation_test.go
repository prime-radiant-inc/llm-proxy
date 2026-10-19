package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var correlationTestHeaders = []string{"X-Lace-Session-Id", "X-Lace-Turn-Id", "X-Lace-Model-Call-Id"}

func setCorrelationTestHeaders(r *http.Request, suffix string) {
	r.Header.Set(correlationTestHeaders[0], "session-"+suffix)
	r.Header.Set(correlationTestHeaders[1], "turn-"+suffix)
	r.Header.Set(correlationTestHeaders[2], "call-"+suffix)
}

func readCorrelationRecords(t *testing.T, directory string) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(directory, "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(file)
		for {
			var record map[string]any
			if err := decoder.Decode(&record); err != nil {
				if !errors.Is(err, io.EOF) {
					file.Close()
					t.Fatal(err)
				}
				break
			}
			if record["type"] == "request" || record["type"] == "response" {
				records = append(records, record)
			}
		}
		file.Close()
	}
	return records
}

func TestCorrelationRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, response string
		status                      int
	}{
		{"buffered", "application/json", `{"content":[{"type":"text","text":"ok"}]}`, 200},
		{"streaming", "text/event-stream", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", 200},
		{"upstream error", "application/json", `{"error":{"message":"fixture"}}`, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const body = `{"model":"fixture-model","max_tokens":5,"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"fixture","input_schema":{"type":"object"}}]}`
			seen := make(chan []byte, 3)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, header := range correlationTestHeaders {
					if r.Header.Get(header) != "" {
						t.Errorf("internal metadata escaped upstream: %s", header)
					}
				}
				got, _ := io.ReadAll(r.Body)
				seen <- got
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.response)
			}))
			defer upstream.Close()
			directory := t.TempDir()
			server, err := NewServer(Config{LogDir: directory})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			for _, enabled := range []bool{false, true, true} {
				request := httptest.NewRequest("POST", "/anthropic/"+strings.TrimPrefix(upstream.URL, "http://")+"/v1/messages", strings.NewReader(body))
				if enabled {
					setCorrelationTestHeaders(request, "a")
				}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				if response.Code != tc.status || response.Body.String() != tc.response {
					t.Fatalf("forwarding changed: %d %q", response.Code, response.Body.String())
				}
				if got := string(<-seen); got != body {
					t.Fatalf("request body/model/tools changed: %s", got)
				}
			}
			records := readCorrelationRecords(t, directory)
			if len(records) != 6 {
				t.Fatalf("got %d records, want three request/response pairs", len(records))
			}
			requests := map[string]bool{}
			correlated := 0
			for _, record := range records {
				meta := record["_meta"].(map[string]any)
				if meta["session"] == "" || meta["request_id"] == "" {
					t.Fatalf("existing identity lost: %#v", meta)
				}
				if record["type"] == "request" {
					requests[meta["request_id"].(string)] = true
				}
				if meta["model_call_id"] != nil {
					correlated++
					if meta["lace_session_id"] != "session-a" || meta["lace_turn_id"] != "turn-a" || meta["model_call_id"] != "call-a" {
						t.Fatalf("wrong correlation: %#v", meta)
					}
				}
			}
			if correlated != 4 || len(requests) != 3 {
				t.Fatalf("correlated records=%d, unique attempt IDs=%d; want 4 and 3", correlated, len(requests))
			}
		})
	}
}

func TestCorrelationInvalidValuesAreOmittedAndStripped(t *testing.T) {
	for _, value := range []string{"", "two words", " leading", "trailing ", "tab\tvalue", "line\nvalue", "control\x00", "delete\x7f", "unicode-π", strings.Repeat("x", 129)} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, header := range correlationTestHeaders {
					if r.Header.Get(header) != "" {
						t.Errorf("header escaped: %s", header)
					}
				}
				io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			logger, err := NewLogger(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer logger.Close()
			proxy := NewProxyWithLogger(logger)
			request := httptest.NewRequest("POST", "/anthropic/"+strings.TrimPrefix(upstream.URL, "http://")+"/v1/messages", strings.NewReader(`{"messages":[]}`))
			request.Header.Set("X-Lace-Session-Id", value)
			request.Header.Set("X-Lace-Turn-Id", strings.Repeat("t", 128))
			request.Header.Set("X-Lace-Model-Call-Id", "call-valid")
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatalf("invalid optional metadata changed HTTP outcome: %d", response.Code)
			}
			records := readCorrelationRecords(t, logger.baseDir)
			if len(records) != 2 {
				t.Fatalf("capture count=%d", len(records))
			}
			for _, record := range records {
				meta := record["_meta"].(map[string]any)
				if meta["lace_session_id"] != nil || meta["lace_turn_id"] != strings.Repeat("t", 128) || meta["model_call_id"] != "call-valid" {
					t.Fatalf("invalid field affected valid siblings: %#v", meta)
				}
			}
		})
	}
}

func TestCorrelationConcurrentRequestsDoNotMix(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		io.WriteString(w, `{}`)
	}))
	defer func() { unblock(); upstream.Close() }()
	server, err := NewServer(Config{LogDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var requests sync.WaitGroup
	for _, suffix := range []string{"a", "b"} {
		requests.Add(1)
		go func(suffix string) {
			defer requests.Done()
			body := fmt.Sprintf(`{"messages":[{"role":"user","content":%q}]}`, suffix)
			request := httptest.NewRequest("POST", "/anthropic/"+strings.TrimPrefix(upstream.URL, "http://")+"/v1/messages", strings.NewReader(body))
			setCorrelationTestHeaders(request, suffix)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != 200 {
				t.Errorf("status=%d", response.Code)
			}
		}(suffix)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			unblock()
			requests.Wait()
			t.Fatal("requests did not both reach upstream")
		}
	}
	unblock()
	requests.Wait()
	counts := map[string]int{}
	for _, record := range readCorrelationRecords(t, server.fileLogger.baseDir) {
		meta := record["_meta"].(map[string]any)
		call, ok := meta["model_call_id"].(string)
		if !ok {
			t.Fatal("missing concurrent call ID")
		}
		suffix := strings.TrimPrefix(call, "call-")
		if meta["lace_session_id"] != "session-"+suffix || meta["lace_turn_id"] != "turn-"+suffix {
			t.Fatalf("mixed contexts: %#v", meta)
		}
		if record["type"] == "request" {
			var body struct{ Messages []struct{ Content string } }
			if err := json.Unmarshal([]byte(record["body"].(string)), &body); err != nil || len(body.Messages) != 1 || body.Messages[0].Content != suffix {
				t.Fatalf("call did not match actual request: %#v", record)
			}
		}
		counts[call]++
	}
	if counts["call-a"] != 2 || counts["call-b"] != 2 || len(counts) != 2 {
		t.Fatalf("unpaired calls: %#v", counts)
	}
	server.fileLogger.contexts.Range(func(key, value any) bool {
		t.Error("completed request context retained")
		return false
	})
}

func TestCorrelationPreservesRunContextAndAuthorization(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Lace-Model-Call-Id") != "" {
			t.Error("internal header escaped")
		}
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")
	logger, err := NewLogger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	proxy := NewProxyWithSessionManagerAndLogger(NewMultiWriter(logger, nil), nil)
	proxy.allowedUpstreams = map[string][]string{"anthropic": {host}}
	proxy.tokenSub = cachedSub("anthropic", host, "nonce-key", ResolveResult{Token: "REAL-KEY", Project: "proj", RunID: "resolved-run", ClientFPHash: "client-fp"})
	for _, run := range []string{"proj-run-1", "other-run"} {
		request := httptest.NewRequest("POST", "/runs/"+run+"/anthropic/"+host+"/v1/messages", strings.NewReader(`{"messages":[]}`))
		request.Header.Set("X-Api-Key", "nonce-key")
		setCorrelationTestHeaders(request, "a")
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if (run == "proj-run-1" && response.Code != 200) || (run == "other-run" && response.Code != 403) {
			t.Fatalf("authorization changed: run=%s status=%d", run, response.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d, want 1", calls)
	}
	entries := readCorrelationRecords(t, logger.baseDir)
	if len(entries) != 2 {
		t.Fatalf("capture count=%d", len(entries))
	}
	for _, entry := range entries {
		meta := entry["_meta"].(map[string]any)
		if meta["run_id"] != "proj-run-1" || meta["resolved_run_id"] != "resolved-run" || meta["project"] != "proj" || meta["client_fp_hash"] != "client-fp" || meta["model_call_id"] != "call-a" {
			t.Fatalf("run context changed or correlation lost: %#v", meta)
		}
	}
}

func TestCorrelationLoggingFailureDoesNotChangeForwarding(t *testing.T) {
	logger, err := NewLogger(filepath.Join(t.TempDir(), "capture"))
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	if err := os.Remove(logger.baseDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logger.baseDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(202)
		io.WriteString(w, "unchanged")
	}))
	defer upstream.Close()
	proxy := NewProxyWithLogger(logger)
	request := httptest.NewRequest("POST", "/anthropic/"+strings.TrimPrefix(upstream.URL, "http://")+"/v1/messages", strings.NewReader(`{"messages":[]}`))
	setCorrelationTestHeaders(request, "a")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != 202 || response.Body.String() != "unchanged" || calls != 1 {
		t.Fatalf("logging failure changed forwarding: %d %q calls=%d", response.Code, response.Body.String(), calls)
	}
	logger.contexts.Range(func(key, value any) bool {
		t.Error("failed logging retained context")
		return false
	})
}

func TestPlatformAWSCorrelationSigningFailure(t *testing.T) {
	logger, err := NewLogger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	proxy := newTestPlatformProxy("us-west-2", "wrkspc_fixture")
	proxy.logger = logger
	fake := &fakeAssumeRoleClient{results: []fakeAssumeRoleResult{{err: errors.New("fixture signing failure")}}}
	proxy.platformAWS.credProv = newPlatformAWSAssumeRoleCredentials(fake, "arn:aws:iam::123456789012:role/example-inference")
	calls := 0
	proxy.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected upstream call")
	})}
	request := httptest.NewRequest("POST", "/anthropic/api.anthropic.com/v1/messages", strings.NewReader(`{"messages":[]}`))
	setCorrelationTestHeaders(request, "failure")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != 500 || response.Body.String() != "platform-aws signing failed\n" || calls != 0 {
		t.Fatalf("signing failure behavior changed: %d %q calls=%d", response.Code, response.Body.String(), calls)
	}
	records := readCorrelationRecords(t, logger.baseDir)
	if len(records) != 2 {
		t.Fatalf("unpaired signing-failure capture: %d records", len(records))
	}
	for _, record := range records {
		meta := record["_meta"].(map[string]any)
		if meta["model_call_id"] != "call-failure" || meta["lace_session_id"] != "session-failure" || meta["lace_turn_id"] != "turn-failure" {
			t.Fatalf("signing-failure correlation missing: %#v", meta)
		}
	}
}

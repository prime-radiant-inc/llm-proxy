package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var cloudBuildRunIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

const (
	mantleProvider      = "openai"
	mantleProviderRoute = "bedrock-mantle"
	mantleWireAPI       = "openai-responses"
)

// serveMantle handles the legacy Bedrock Mantle Responses API pass-through path.
// It resolves the client's opaque nonce to a real Bedrock Bearer key and forwards
// to the fixed bedrock-mantle.<region>.api.aws upstream, mapping
// /mantle/v1/responses to /openai/v1/responses.
//
// Mantle observability (session tracking, event emission, response logging) is a
// deliberate follow-up: this v1 path is intentionally lean pass-through only.
func (p *Proxy) serveMantle(w http.ResponseWriter, r *http.Request) {
	p.serveMantleForPath(w, r, "", r.URL.Path)
}

func (p *Proxy) serveMantleForPath(w http.ResponseWriter, r *http.Request, requiredRunID, mantlePath string) {
	if p.bedrock == nil {
		http.Error(w, "Bedrock not configured", http.StatusServiceUnavailable)
		return
	}
	if p.tokenSub == nil {
		http.Error(w, "token substitution not configured", http.StatusServiceUnavailable)
		return
	}

	// Reject non-canonical paths before any minting or upstream call. A literal
	// `..` or double-slash would otherwise forward to /openai/../... and escape the
	// upstream's namespace on the fixed host. Require the legacy /v1/ prefix here;
	// run-scoped /cbrun paths are tightened separately in parseCloudBuildMantlePath.
	rest := strings.TrimPrefix(mantlePath, "/mantle")
	if rest != path.Clean(rest) || !strings.HasPrefix(rest, "/v1/") {
		http.Error(w, "invalid mantle path", http.StatusBadRequest)
		return
	}

	sessionID := p.generateSessionID()
	requestID := uuid.New().String()

	// Acquire concurrency semaphore for backpressure.
	select {
	case p.bedrock.semaphore <- struct{}{}:
		defer func() { <-p.bedrock.semaphore }()
	case <-r.Context().Done():
		http.Error(w, "request cancelled", http.StatusServiceUnavailable)
		return
	}

	upstream := fmt.Sprintf("bedrock-mantle.%s.api.aws", p.bedrock.region)

	// Map /mantle/v1/responses → /openai/v1/responses, preserving any query string.
	targetPath := "/openai" + rest
	upstreamURL := fmt.Sprintf("https://%s%s", upstream, targetPath)
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	// Read request body (capped).
	var reqBody []byte
	if r.Body != nil {
		var err error
		reqBody, err = io.ReadAll(io.LimitReader(r.Body, bedrockMaxRequestBody))
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusInternalServerError)
			return
		}
		r.Body.Close()
	}

	// Resolve the client's opaque nonce to a real short-lived Bearer key.
	// Fail closed: no upstream call on resolution failure.
	nonce, _ := readClientToken(r.Header)
	resolved, status, _ := p.tokenSub.Resolve(r.Context(), ResolveContext{
		APIToken:    nonce,
		ClientHost:  r.RemoteAddr,
		Provider:    mantleProvider,
		ProviderURL: upstream,
	})
	if status != 0 {
		p.logMantlePreUpstreamError(sessionID, requestID, requiredRunID, "", mantleRequestModel(reqBody), "token_substitution_failed", "api token substitution failed", status)
		http.Error(w, "api token substitution failed", status)
		return
	}
	if requiredRunID != "" && resolved.RunID != requiredRunID {
		p.logMantlePreUpstreamError(sessionID, requestID, requiredRunID, "", mantleRequestModel(reqBody), "unresolved_run_id", "unresolved cloud build run id", http.StatusForbidden)
		http.Error(w, "unresolved cloud build run id", http.StatusForbidden)
		return
	}

	p.logMantleRequestObservation(sessionID, requestID, requiredRunID, upstream, r.Method, r.URL.Path, mantlePath, targetPath, reqBody)

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	// Whitelist headers — only copy Content-Type and Accept; the client's opaque
	// nonce never rides upstream alongside the resolved Bearer key.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		proxyReq.Header.Set("Content-Type", ct)
	}
	if accept := r.Header.Get("Accept"); accept != "" {
		proxyReq.Header.Set("Accept", accept)
	}
	proxyReq.Header.Set("Authorization", "Bearer "+resolved.Token)

	startTime := time.Now()
	resp, err := p.bedrock.client.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if isStreamingResponse(resp) {
		sw := NewStreamingResponseWriter(w, mantleProvider)

		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)

		reader := bufio.NewReader(resp.Body)
		for {
			line, readErr := reader.ReadBytes('\n')
			if len(line) > 0 {
				if _, writeErr := sw.Write(line); writeErr != nil {
					return
				}
				sw.Flush()
			}
			if readErr != nil {
				if readErr == io.EOF {
					break
				}
				return
			}
		}

		ttfb := int64(0)
		if len(sw.Chunks()) > 0 {
			ttfb = sw.Chunks()[0].DeltaMs
		}
		p.logMantleResponseObservation(
			sessionID,
			requestID,
			requiredRunID,
			resp.StatusCode,
			ResponseTiming{
				TTFBMs:  ttfb,
				TotalMs: time.Since(startTime).Milliseconds(),
			},
			nil,
			sw.Chunks(),
			reqBody,
		)
		return
	}

	ttfb := time.Since(startTime)

	// Non-streaming: forward headers, status, and body (also forwards non-200 bodies).
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, bedrockMaxRequestBody))
	if err != nil {
		http.Error(w, "failed to read response body: "+err.Error(), http.StatusBadGateway)
		return
	}

	p.logMantleResponseObservation(
		sessionID,
		requestID,
		requiredRunID,
		resp.StatusCode,
		ResponseTiming{
			TTFBMs:  ttfb.Milliseconds(),
			TotalMs: time.Since(startTime).Milliseconds(),
		},
		decodeBodyForLogging(respBody, resp.Header),
		nil,
		reqBody,
	)

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func parseCloudBuildMantlePath(path string) (runID string, mantlePath string, ok bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "cbrun" || parts[3] != "mantle" || parts[4] != "v1" || parts[5] != "responses" {
		return "", "", false
	}

	runID, err := url.PathUnescape(parts[2])
	if err != nil {
		return "", "", false
	}
	if !validCloudBuildRunID(runID) {
		return "", "", false
	}

	return runID, "/mantle/v1/responses", true
}

func validCloudBuildRunID(runID string) bool {
	if runID == "" || runID == "." || runID == ".." || len(runID) > 128 {
		return false
	}
	if runID[0] == '-' || strings.ContainsAny(runID, `/\`) {
		return false
	}
	return cloudBuildRunIDRe.MatchString(runID)
}

func mantleUpstreamHost(region string) string {
	if region == "" {
		return "bedrock-mantle.api.aws"
	}
	return fmt.Sprintf("bedrock-mantle.%s.api.aws", region)
}

func extractRejectedCloudBuildRunID(escapedPath string) string {
	parts := strings.Split(escapedPath, "/")
	if len(parts) > 2 {
		runID, err := url.PathUnescape(parts[2])
		if err == nil {
			return runID
		}
		return parts[2]
	}
	return ""
}

func mantleRequestModel(body []byte) string {
	parsed, _ := decodeObservationBody(body)
	obj, ok := parsed.(map[string]any)
	if !ok {
		return ""
	}
	model, _ := obj["model"].(string)
	return model
}

func decodeObservationBody(body []byte) (any, bool) {
	if len(body) == 0 {
		return nil, false
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err == nil {
		return parsed, true
	}
	return string(body), false
}

func mantleResponseBodyFromStream(chunks []StreamChunk) any {
	for _, chunk := range chunks {
		raw := strings.TrimSpace(chunk.Raw)
		if !strings.HasPrefix(raw, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(raw, "data: "))), &event); err != nil {
			continue
		}
		if eventType, _ := event["type"].(string); eventType == "response.completed" {
			if response, ok := event["response"]; ok {
				return response
			}
		}
	}
	return nil
}

func (p *Proxy) newMantleMeta(sessionID, requestID, runID, model string) map[string]any {
	meta := map[string]any{
		"schema_version":     "telemetry-contract-v0",
		"ts":                 time.Now().UTC().Format(time.RFC3339Nano),
		"request_id":         requestID,
		"cloud_build_run_id": runID,
		"provider":           mantleProvider,
		"provider_route":     mantleProviderRoute,
		"wire_api":           mantleWireAPI,
	}
	if sessionID != "" {
		meta["session"] = sessionID
	}
	if model != "" {
		meta["model_raw"] = model
		meta["model_normalized"] = model
	}
	return meta
}

func (p *Proxy) writeMantleObservation(sessionID string, entry map[string]any) {
	if p.logger == nil {
		return
	}
	upstream := mantleUpstreamHost("")
	if p.bedrock != nil {
		upstream = mantleUpstreamHost(p.bedrock.region)
	}
	p.logger.RegisterUpstream(sessionID, upstream)
	_ = p.logger.LogObservation(sessionID, mantleProvider, entry)
}

func (p *Proxy) logMantlePreUpstreamError(sessionID, requestID, runID, rejectedRunID, model, class, message string, status int) {
	entry := map[string]any{
		"type":   "error",
		"status": status,
		"_meta":  p.newMantleMeta(sessionID, requestID, runID, model),
		"error": map[string]any{
			"class":   class,
			"message": message,
		},
	}
	if rejectedRunID != "" {
		entry["_meta"].(map[string]any)["rejected_run_id"] = rejectedRunID
	}
	p.writeMantleObservation(sessionID, entry)
}

func (p *Proxy) logMantleRequestObservation(sessionID, requestID, runID, upstream, method, ingressPath, proxyRoute, upstreamPath string, reqBody []byte) {
	body, _ := decodeObservationBody(reqBody)
	model := mantleRequestModel(reqBody)
	entry := map[string]any{
		"type":  "request",
		"_meta": p.newMantleMeta(sessionID, requestID, runID, model),
		"request": map[string]any{
			"method":        method,
			"ingress_path":  ingressPath,
			"proxy_route":   proxyRoute,
			"upstream_path": upstreamPath,
			"body":          body,
		},
	}
	_ = upstream
	p.writeMantleObservation(sessionID, entry)
}

func (p *Proxy) logMantleResponseObservation(sessionID, requestID, runID string, status int, timing ResponseTiming, respBody []byte, chunks []StreamChunk, reqBody []byte) {
	model := mantleRequestModel(reqBody)
	response := map[string]any{}
	if len(chunks) > 0 {
		response["chunks"] = chunks
		if body := mantleResponseBodyFromStream(chunks); body != nil {
			response["body"] = body
		}
	} else if body, _ := decodeObservationBody(respBody); body != nil {
		response["body"] = body
	}

	entry := map[string]any{
		"type":     "response",
		"status":   status,
		"timing":   timing,
		"_meta":    p.newMantleMeta(sessionID, requestID, runID, model),
		"response": response,
	}
	p.writeMantleObservation(sessionID, entry)
}

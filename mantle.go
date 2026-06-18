package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

var cloudBuildRunIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

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
		Provider:    "openai",
		ProviderURL: upstream,
	})
	if status != 0 {
		http.Error(w, "api token substitution failed", status)
		return
	}
	if requiredRunID != "" && resolved.RunID != requiredRunID {
		http.Error(w, "unresolved cloud build run id", http.StatusForbidden)
		return
	}

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

	resp, err := p.bedrock.client.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if isStreamingResponse(resp) {
		// Pass-through stream with no logging (nil logger/sm/emitter) — mantle
		// observability is a deliberate follow-up.
		streamResponse(w, resp, nil, nil, "", "openai", 0, time.Now(), reqBody, "", nil, "", nil)
		return
	}

	// Non-streaming: forward headers, status, and body (also forwards non-200 bodies).
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, bedrockMaxRequestBody))
}

func parseCloudBuildMantlePath(path string) (runID string, mantlePath string, ok bool) {
	const prefix = "/cbrun/"
	const requiredSuffix = "mantle/v1/responses"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}

	rest := strings.TrimPrefix(path, prefix)
	runID, suffix, found := strings.Cut(rest, "/")
	if !found || strings.Contains(runID, "%") || strings.Contains(suffix, "%") {
		return "", "", false
	}
	if !validCloudBuildRunID(runID) || suffix != requiredSuffix {
		return "", "", false
	}

	return runID, "/" + suffix, true
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

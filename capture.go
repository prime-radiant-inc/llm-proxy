// capture.go
//
// Capture-contract vocabulary: facts the proxy stamps on every logged
// request/response line so downstream consumers read them instead of
// inferring them from file layout or path patterns.
package main

import "strings"

// CaptureVersion marks lines that carry stamped capture facts.
const CaptureVersion = 1

// Termination describes how the relay of a response ended.
const (
	TerminationEOF                 = "eof"
	TerminationUpstreamError       = "upstream_error"
	TerminationClientCancel        = "client_cancel"
	TerminationUpstreamUnreachable = "upstream_unreachable"
)

// ResponseCapture carries per-response capture facts into LogResponse.
type ResponseCapture struct {
	Path             string
	Termination      string
	TerminationError string
}

// meteringProviderFromUpstream maps an upstream host to the provider whose
// pricing applies. This is deliberately host-based: the proxy route segment
// is a transport detail (OpenRouter rides the "openai" route) and must never
// be treated as the billing provider. The vocabulary mirrors the consumer
// side's host mapping; chatgpt.com is the JWT/codex reroute target.
func meteringProviderFromUpstream(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	switch {
	case strings.Contains(host, "openrouter"):
		return "openrouter"
	case strings.Contains(host, "bedrock-runtime"):
		return "anthropic"
	case strings.Contains(host, "anthropic"):
		return "anthropic"
	case host == "chatgpt.com":
		return "openai"
	case strings.Contains(host, "openai"):
		return "openai"
	default:
		return ""
	}
}

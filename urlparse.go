// urlparse.go
package main

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var (
	ErrInvalidProxyPath = errors.New("invalid proxy path: expected /{provider}/{upstream}/{path}")
	ErrUnknownProvider  = errors.New("unknown provider: must be 'anthropic' or 'openai'")
)

var validProviders = map[string]bool{
	"anthropic": true,
	"openai":    true,
}

var runAttributionIDRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]{0,127}$`)

type RunEnvelope struct {
	RunID            string
	InnerEscapedPath string
	InnerPath        string
}

// ParseProxyURL extracts provider, upstream host, and remaining path from a proxy URL.
// Expected format: /{provider}/{upstream}/{remaining_path}
func ParseProxyURL(urlPath string) (provider, upstream, path string, err error) {
	// Remove leading slash and split
	trimmed := strings.TrimPrefix(urlPath, "/")
	parts := strings.SplitN(trimmed, "/", 3)

	if len(parts) < 3 {
		return "", "", "", ErrInvalidProxyPath
	}

	provider = parts[0]
	upstream = parts[1]
	path = "/" + parts[2]

	if !validProviders[provider] {
		return "", "", "", ErrUnknownProvider
	}

	if upstream == "" {
		return "", "", "", ErrInvalidProxyPath
	}

	return provider, upstream, path, nil
}

func ParseRunEnvelope(escapedPath string) (RunEnvelope, bool, error) {
	if !strings.HasPrefix(escapedPath, "/runs/") {
		return RunEnvelope{}, false, nil
	}

	rest := strings.TrimPrefix(escapedPath, "/runs/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return RunEnvelope{}, true, ErrInvalidProxyPath
	}

	escapedRunID := rest[:slash]
	if escapedRunID == "" {
		return RunEnvelope{}, true, ErrInvalidProxyPath
	}

	lowerEscapedRunID := strings.ToLower(escapedRunID)
	if strings.Contains(lowerEscapedRunID, "%2f") || strings.Contains(lowerEscapedRunID, "%5c") {
		return RunEnvelope{}, true, ErrInvalidProxyPath
	}

	runID, err := url.PathUnescape(escapedRunID)
	if err != nil {
		return RunEnvelope{}, true, err
	}
	if !ValidRunAttributionID(runID) {
		return RunEnvelope{}, true, ErrInvalidProxyPath
	}

	innerEscaped := rest[slash:]
	inner, err := url.PathUnescape(innerEscaped)
	if err != nil {
		return RunEnvelope{}, true, err
	}
	if inner == "" || inner[0] != '/' {
		return RunEnvelope{}, true, ErrInvalidProxyPath
	}

	return RunEnvelope{
		RunID:            runID,
		InnerEscapedPath: innerEscaped,
		InnerPath:        inner,
	}, true, nil
}

func ValidRunAttributionID(runID string) bool {
	if runID == "" || runID == "." || runID == ".." {
		return false
	}
	if strings.ContainsAny(runID, `/\`) {
		return false
	}
	return runAttributionIDRe.MatchString(runID)
}

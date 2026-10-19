package main

import "net/http"

// ModelCallCorrelation joins runtime diagnostics to captures without influencing execution.
type ModelCallCorrelation struct {
	SessionID   string
	TurnID      string
	ModelCallID string
}

func extractModelCallCorrelation(headers http.Header) ModelCallCorrelation {
	identifier := func(name string) string {
		value := headers.Get(name)
		if len(value) == 0 || len(value) > 128 {
			return ""
		}
		for i := range value {
			if value[i] < '!' || value[i] > '~' {
				return ""
			}
		}
		return value
	}
	return ModelCallCorrelation{
		SessionID:   identifier("X-Lace-Session-Id"),
		TurnID:      identifier("X-Lace-Turn-Id"),
		ModelCallID: identifier("X-Lace-Model-Call-Id"),
	}
}

// Remove even invalid values before upstream validation or signing sees them.
func stripModelCallCorrelation(headers http.Header) {
	headers.Del("X-Lace-Session-Id")
	headers.Del("X-Lace-Turn-Id")
	headers.Del("X-Lace-Model-Call-Id")
}

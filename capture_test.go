package main

import "testing"

func TestMeteringProviderFromUpstream(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"openrouter.ai", "openrouter"},
		{"api.openai.com", "openai"},
		{"api.anthropic.com", "anthropic"},
		{"bedrock-runtime.us-west-2.amazonaws.com", "anthropic"},
		{"chatgpt.com", "openai"},
		{"OPENROUTER.AI", "openrouter"},
		{" api.openai.com ", "openai"},
		{"bedrock-mantle.us-east-1.api.aws", ""},
		{"example.com", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := meteringProviderFromUpstream(tc.host); got != tc.want {
			t.Errorf("meteringProviderFromUpstream(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

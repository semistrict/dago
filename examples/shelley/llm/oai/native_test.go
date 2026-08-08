package oai

import "testing"

func TestResponsesAPIBaseURL(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"":                                     "",
		"https://api.openai.com":               "https://api.openai.com/v1",
		"https://api.openai.com/":              "https://api.openai.com/v1",
		"https://api.openai.com/v1":            "https://api.openai.com/v1",
		"https://api.openai.com/v1/":           "https://api.openai.com/v1",
		"https://api.openai.com/v1/responses":  "https://api.openai.com/v1",
		"https://example.test/provider/prefix": "https://example.test/provider/prefix/v1",
		" https://example.test/v1/responses/ ": "https://example.test/v1",
	}
	for input, want := range tests {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if got := responsesAPIBaseURL(input); got != want {
				t.Fatalf("responsesAPIBaseURL(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

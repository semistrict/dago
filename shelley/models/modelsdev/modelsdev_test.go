package modelsdev

import "testing"

func TestLookupImageSupport(t *testing.T) {
	cases := []struct {
		name       string
		endpoint   string
		model      string
		wantFound  bool
		wantImages bool
	}{
		// First-party hosts (models.dev omits their "api" field; seeded via
		// knownHosts).
		{"anthropic", "https://api.anthropic.com", "claude-opus-4-1-20250805", true, true},
		{"openai", "https://api.openai.com/v1", "gpt-5.4", true, true},
		{"gemini", "https://generativelanguage.googleapis.com", "gemini-3.1-pro-preview", true, true},

		// Hosts that carry an explicit "api" field in models.dev.
		{"fireworks text-only", "https://api.fireworks.ai/inference/v1", "accounts/fireworks/models/glm-5p2", true, false},
		{"fireworks vision", "https://api.fireworks.ai/inference/v1", "accounts/fireworks/models/kimi-k3", true, true},

		// The original bug: a custom model pointed at opencode.ai/zen. The
		// host matches even though the configured path needn't be exact, and
		// deepseek-v4-flash is text-only.
		// /zen/go/v1 is the opencode-go provider (the exact URL from the
		// original 400). deepseek-v4-flash lives there and is text-only.
		{"opencode-go zen deepseek", "https://opencode.ai/zen/go/v1/chat/completions", "deepseek-v4-flash", true, false},
		// /zen/v1 is the opencode provider, which carries deepseek-v4-flash-free.
		{"opencode zen deepseek-free", "https://opencode.ai/zen/v1", "deepseek-v4-flash-free", true, false},
		// The path disambiguates which provider's catalog applies: the -free
		// id only exists under opencode (/zen/v1), not opencode-go.
		{"opencode bare host resolves go model", "opencode.ai", "deepseek-v4-flash", true, false},

		// Unknown / empty endpoints yield no information.
		{"unknown host", "https://made-up.example.com", "x", false, false},
		{"empty endpoint", "", "claude-opus-4-1-20250805", false, false},
		{"known host unknown model", "https://api.fireworks.ai/inference/v1", "made-up-model", false, false},

		// Last-segment fallback within a host-matched provider.
		{"openai slug", "https://api.openai.com", "openai/gpt-4o", true, true},
		{"openai slug text", "https://api.openai.com", "openai/gpt-oss-20b", true, false},

		// Slugs whose host we don't know fall through to OpenRouter's catalog.
		{"openrouter llama", "", "meta-llama/llama-3.3-70b-instruct", true, false},
		{"openrouter deepseek", "", "deepseek/deepseek-chat", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotImages, gotFound := LookupImageSupport(c.endpoint, c.model)
			if gotFound != c.wantFound || gotImages != c.wantImages {
				t.Errorf("LookupImageSupport(%q,%q) = (images=%v,found=%v); want (images=%v,found=%v)",
					c.endpoint, c.model, gotImages, gotFound, c.wantImages, c.wantFound)
			}
		})
	}
}

// imageEntry builds a modelEntry with the given image-input support.
func imageEntry(image bool) modelEntry {
	var m modelEntry
	if image {
		m.Modalities.Input = []string{"text", "image"}
	} else {
		m.Modalities.Input = []string{"text"}
	}
	return m
}

// prov builds a providerEntry with an "api" URL carrying a single model id.
func prov(api, modelID string, image bool) providerEntry {
	return providerEntry{API: api, Models: map[string]modelEntry{modelID: imageEntry(image)}}
}

func TestBestProviderForPath(t *testing.T) {
	// Mirror the real opencode collision: two providers on one host with
	// different paths and different image support for the same model id.
	zen := prov("https://opencode.ai/zen/v1", "m", true)
	zenGo := prov("https://opencode.ai/zen/go/v1", "m", false)
	providers := []providerEntry{zen, zenGo}

	cases := []struct {
		name     string
		endpoint string
		wantAPI  string // "" means expect ok=false
	}{
		{"go path picks opencode-go", "https://opencode.ai/zen/go/v1/chat/completions", zenGo.API},
		{"plain zen path picks opencode", "https://opencode.ai/zen/v1/chat/completions", zen.API},
		{"shorter/looser path still resolves", "https://opencode.ai/zen", zen.API},
		{"model absent everywhere", "https://opencode.ai/zen/go/v1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model := "m"
			if c.wantAPI == "" {
				model = "absent"
			}
			p, ok := bestProviderForPath(providers, pathSegments(c.endpoint), model)
			if (c.wantAPI != "") != ok {
				t.Fatalf("ok = %v; want %v", ok, c.wantAPI != "")
			}
			if ok && p.API != c.wantAPI {
				t.Errorf("chose %q; want %q", p.API, c.wantAPI)
			}
		})
	}
}

func TestLookupReasoningSupport(t *testing.T) {
	cases := []struct {
		endpoint, model string
		want, found     bool
	}{
		{"https://api.openai.com/v1", "gpt-5.4", true, true},
		{"https://api.openai.com/v1", "gpt-4o", false, true},
		{"https://api.fireworks.ai/inference/v1", "accounts/fireworks/models/gpt-oss-20b", true, true},
		{"https://generativelanguage.googleapis.com", "gemini-3-flash-preview", true, true},
		{"https://made-up.example.com", "x", false, false},
	}
	for _, tc := range cases {
		got, found := LookupReasoningSupport(tc.endpoint, tc.model)
		if got != tc.want || found != tc.found {
			t.Errorf("LookupReasoningSupport(%q, %q) = (%v, %v), want (%v, %v)", tc.endpoint, tc.model, got, found, tc.want, tc.found)
		}
	}
}

func TestLookupCost(t *testing.T) {
	cases := []struct {
		name      string
		endpoint  string
		model     string
		wantFound bool
		wantIn    float64
		wantOut   float64
	}{
		// First-party models resolve by name alone even when the endpoint is
		// an unknown gateway host.
		{"anthropic via gateway", "https://llm.int.exe.xyz/v1/messages", "claude-opus-4-6", true, 5, 25},
		{"anthropic dated", "", "claude-sonnet-4-5-20250929", true, 3, 15},
		// OpenAI snapshot names carry a date suffix that models.dev omits.
		{"openai dated", "https://llm.int.exe.xyz/v1/responses", "gpt-5.5-2026-04-23", true, 5, 30},
		{"openai undated", "", "gpt-5.3-codex", true, 1.75, 14},
		{"fireworks full path", "", "accounts/fireworks/models/kimi-k2p6", true, 0.95, 4},
		{"unknown model", "", "predictable-v1", false, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, found := LookupCost(tc.endpoint, tc.model)
			if found != tc.wantFound {
				t.Fatalf("LookupCost(%q, %q) found = %v, want %v", tc.endpoint, tc.model, found, tc.wantFound)
			}
			if c.Input != tc.wantIn || c.Output != tc.wantOut {
				t.Errorf("LookupCost(%q, %q) = %+v, want input=%v output=%v", tc.endpoint, tc.model, c, tc.wantIn, tc.wantOut)
			}
		})
	}
}

package dacode

import (
	"slices"
	"testing"
)

func TestSlashCommandRegistryClassifiesPinnedPublicSurface(t *testing.T) {
	definitions := publicSlashCommandDefinitions()
	if len(definitions) != 42 {
		t.Fatalf("public commands = %d", len(definitions))
	}
	want := []string{
		"/agents", "/auto", "/manual", "/yolo", "/auth", "/clear", "/copy", "/context", "/cost", "/force-clear",
		"/goal", "/workflow", "/workflows", "/editor", "/effort", "/mcp", "/plugins", "/model", "/notifications", "/offload", "/remember", "/reload",
		"/skill-creator", "/threads", "/trace", "/tokens", "/tools", "/rubric", "/restart", "/theme", "/scrollbar", "/timestamps",
		"/line-numbers", "/update", "/install", "/auto-update", "/changelog", "/version", "/feedback", "/docs", "/help", "/quit",
	}
	for index, definition := range definitions {
		if definition.Name != want[index] {
			t.Fatalf("command %d = %q, want %q", index, definition.Name, want[index])
		}
	}
	classified := classifiedSlashCommandNames()
	for _, name := range []string{"/about", "/compact", "/connect", "/criteria", "/q", "/debug", "/debug-error"} {
		if !slices.Contains(classified, name) {
			t.Fatalf("classified commands omitted %q", name)
		}
	}
}

func TestSlashCommandRegistryResolvesAliasesWithoutMutation(t *testing.T) {
	definition, exists := slashCommandDefinitionFor("  /ABOUT  ")
	if !exists || definition.Name != "/version" || definition.Tier != commandConnecting {
		t.Fatalf("alias = %#v, %v", definition, exists)
	}
	definitions := publicSlashCommandDefinitions()
	definitions[len(definitions)-1].Aliases[0] = "/mutated"
	definition, exists = slashCommandDefinitionFor("/q")
	if !exists || definition.Name != "/quit" {
		t.Fatalf("registry mutated = %#v, %v", definition, exists)
	}
}

func TestSlashCommandQueueBypassTiers(t *testing.T) {
	busy := commandQueueState{AgentRunning: true}
	tests := []struct {
		value string
		state commandQueueState
		want  bool
	}{
		{"/quit", commandQueueState{Switching: true, AgentRunning: true}, true},
		{"/quit now", commandQueueState{Switching: true, AgentRunning: true}, false},
		{"/q now", commandQueueState{Switching: true, AgentRunning: true}, false},
		{"/restart", commandQueueState{Switching: true}, true},
		{"/restart forced", commandQueueState{Switching: true}, false},
		{"/force-clear now", busy, false},
		{"/version", commandQueueState{Connecting: true}, true},
		{"/about", commandQueueState{Connecting: true, AgentRunning: true}, false},
		{"/model", busy, true},
		{"/workflows", busy, true},
		{"/workflow release-sweep", busy, false},
		{"/model openai:example", busy, false},
		{"/auto model", busy, true},
		{"/auto\t model", busy, true},
		{"/auto model clear", busy, false},
		{"/docs", busy, true},
		{"/mcp login server", busy, true},
		{"/help", busy, false},
		{"/update", commandQueueState{StartupFailed: true}, true},
		{"/install provider", commandQueueState{StartupFailed: true}, true},
		{"/install --unknown", commandQueueState{StartupFailed: true}, false},
		{"/install one two", commandQueueState{StartupFailed: true}, false},
		{"/reload", commandQueueState{StartupFailed: true, AgentRunning: true}, false},
		{"/clear", commandQueueState{StartupFailed: true}, false},
		{"/docs", commandQueueState{Switching: true}, false},
		{"", busy, false},
		{"/unknown", busy, false},
	}
	for _, test := range tests {
		if got := canBypassCommandQueue(test.value, test.state); got != test.want {
			t.Errorf("canBypassCommandQueue(%q, %#v) = %v, want %v", test.value, test.state, got, test.want)
		}
	}
}

func TestSlashCommandRegistryRejectsInvalidStaticDefinitions(t *testing.T) {
	tests := [][]slashCommandDefinition{
		{{Name: "bad", Description: "bad", Tier: commandQueued}},
		{{Name: "/one", Description: "one", Tier: commandQueued}, {Name: "/two", Description: "two", Tier: commandQueued, Aliases: []string{"/one"}}},
		{{Name: "/one", Description: "one", Tier: "unknown"}},
	}
	for _, definitions := range tests {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("definitions did not panic: %#v", definitions)
				}
			}()
			buildSlashCommandIndex(definitions)
		}()
	}
}

func TestSlashAutocompleteIsDerivedFromPublicRegistry(t *testing.T) {
	for _, definition := range publicSlashCommandDefinitions() {
		if !slices.Contains(slashCommandNames, definition.Name) {
			t.Fatalf("autocomplete omitted %q", definition.Name)
		}
		if label := slashCompletionLabel(definition.Name); label == definition.Name || label == "" {
			t.Fatalf("autocomplete label for %q = %q", definition.Name, label)
		}
	}
	for hidden := range hiddenSlashCommands {
		if slices.Contains(slashCommandNames, hidden) {
			t.Fatalf("autocomplete exposed hidden command %q", hidden)
		}
	}
	for _, legacy := range []string{"/new", "/skills", "/resume", "/exit"} {
		if slices.Contains(slashCommandNames, legacy) {
			t.Fatalf("autocomplete exposed unregistered legacy command %q", legacy)
		}
	}
	if !slices.Contains(slashCommandNames, "/skill:") {
		t.Fatal("autocomplete omitted dynamic skill invocation")
	}
}

func TestSlashAliasesCanonicalizeBeforeDispatch(t *testing.T) {
	tests := map[string]string{
		"/q": "/quit", "/connect": "/auth", "/compact now": "/offload now",
		"/criteria show": "/rubric show", "/about": "/version",
	}
	for input, want := range tests {
		if got, ok := canonicalSlashInput(input); !ok || got != want {
			t.Errorf("canonicalSlashInput(%q) = %q, %v; want %q", input, got, ok, want)
		}
	}
}

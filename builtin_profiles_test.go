package dago

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	providerprofile "github.com/semistrict/dago/daproviders/profile"
)

func TestBuiltinAnthropicHarnessProfiles(t *testing.T) {
	tests := []struct {
		model        string
		opusGuidance bool
	}{
		{model: "claude-haiku-4-5"},
		{model: "claude-sonnet-4-6"},
		{model: "claude-opus-4-7", opusGuidance: true},
	}
	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			profile, exists := builtinHarnessProfile("anthropic", test.model)
			if !exists || profile.SystemPromptSuffix == nil {
				t.Fatalf("profile = %#v, exists = %v", profile, exists)
			}
			suffix := *profile.SystemPromptSuffix
			for _, marker := range []string{"<use_parallel_tool_calls>", "<investigate_before_answering>", "<tool_result_reflection>"} {
				if !strings.Contains(suffix, marker) {
					t.Errorf("suffix omits %s", marker)
				}
			}
			if strings.Contains(suffix, "<tool_usage>") != test.opusGuidance || strings.Contains(suffix, "<subagent_usage>") != test.opusGuidance {
				t.Errorf("model-specific guidance mismatch: %q", suffix)
			}
		})
	}
}

func TestBuiltinHarnessProfileResolvesFromModel(t *testing.T) {
	script := modeltest.New(damodel.Profile{Provider: "anthropic", Model: "claude-opus-4-7"}, modeltest.Step{
		Check: func(request damodel.Request) error {
			prompt := request.Messages[0].TextContent()
			if !strings.Contains(prompt, "user instructions") || !strings.Contains(prompt, "<tool_usage>") {
				return &profileTestError{value: prompt}
			}
			return nil
		},
		Response: damodel.Response{Message: damessage.Assistant("done")},
	})
	compiled := New(script, WithSystemMessage(damessage.System("user instructions")), WithFilesystem(Filesystem{}))

	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}
}

func TestBuiltinEngineeringProfileAddsPlanningAndBehavior(t *testing.T) {
	modelID := "gpt-5.2-" + "co" + "dex"
	script := modeltest.New(damodel.Profile{Provider: "openai", Model: modelID}, modeltest.Step{
		Check: func(request damodel.Request) error {
			if request.Messages[0].Role != damessage.RoleSystem || !strings.Contains(request.Messages[0].TextContent(), "Engineering-Agent Behavior") {
				return &profileTestError{value: request.Messages[0].TextContent()}
			}
			for _, definition := range request.Tools {
				if definition.Name == "write_todos" {
					return nil
				}
			}
			return &profileTestError{value: "write_todos missing"}
		},
		Response: damodel.Response{Message: damessage.Assistant("done")},
	})
	compiled := New(script, WithFilesystem(Filesystem{}))

	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}
}

func TestBuiltinProviderProfiles(t *testing.T) {
	profiles := providerprofile.Builtin()
	openAI, err := profiles.ApplyWithPreInit("openai:any-model", nil)
	if err != nil || openAI["use_responses_api"] != true {
		t.Fatalf("OpenAI options = %#v, error = %v", openAI, err)
	}
	nvidia, err := profiles.ApplyWithPreInit("nvidia:any-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantHeaders := map[string]string{"X-BILLING-INVOKE-ORIGIN": "DeepAgents"}
	if !reflect.DeepEqual(nvidia["default_headers"], wantHeaders) {
		t.Fatalf("NVIDIA headers = %#v", nvidia["default_headers"])
	}
}

func TestBuiltinOpenRouterDefaultsRespectEnvironment(t *testing.T) {
	t.Setenv("OPENROUTER_APP_URL", "")
	t.Setenv("OPENROUTER_APP_TITLE", "custom")
	t.Setenv("DEEPAGENTS_OPENROUTER_ALLOW_AZURE", " yes ")
	options, err := providerprofile.Builtin().ApplyWithPreInit("openrouter:any-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := options["app_url"]; exists {
		t.Fatalf("app_url overrode environment: %#v", options)
	}
	if _, exists := options["app_title"]; exists {
		t.Fatalf("app_title overrode environment: %#v", options)
	}
	if _, exists := options["openrouter_provider"]; exists {
		t.Fatalf("provider exclusion ignored environment: %#v", options)
	}
}

func TestBuiltinOpenRouterDefaultsWithoutEnvironment(t *testing.T) {
	for _, name := range []string{"OPENROUTER_APP_URL", "OPENROUTER_APP_TITLE", "DEEPAGENTS_OPENROUTER_ALLOW_AZURE"} {
		t.Setenv(name, "temporary")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	options, err := providerprofile.Builtin().ApplyWithPreInit("openrouter:any-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"app_url":             "https://github.com/langchain-ai/deepagents",
		"app_title":           "Deep Agents",
		"openrouter_provider": map[string]any{"ignore": []string{"azure"}},
	}
	if !reflect.DeepEqual(options, want) {
		t.Fatalf("options = %#v, want %#v", options, want)
	}
}

func TestBuiltinOpenRouterDefaultsAreIsolated(t *testing.T) {
	t.Setenv("OPENROUTER_APP_URL", "https://github.com/langchain-ai/deepagents")
	t.Setenv("OPENROUTER_APP_TITLE", "Deep Agents")
	t.Setenv("DEEPAGENTS_OPENROUTER_ALLOW_AZURE", "false")
	first, err := providerprofile.Builtin().ApplyWithPreInit("openrouter:model", nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := first["openrouter_provider"].(map[string]any)
	provider["ignore"] = []string{"changed"}
	second, err := providerprofile.Builtin().ApplyWithPreInit("openrouter:model", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"ignore": []string{"azure"}}
	if !reflect.DeepEqual(second["openrouter_provider"], want) {
		t.Fatalf("options aliased across calls: %#v", second)
	}
}

type profileTestError struct{ value string }

func (err *profileTestError) Error() string { return "built-in profile prompt missing: " + err.value }

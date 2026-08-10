package dago

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
	"github.com/semistrict/dago/tool"
)

func TestProfileRegistrationMergeToolOverrideAndExclusion(t *testing.T) {
	name := "test-profile-registration"
	if err := RegisterProfile(Profile{
		Name: name, Kind: ProfileHarness, SystemPrompt: "profile prompt",
		ToolDescriptions: map[string]string{"kept": "overridden description"},
		ExcludeTools:     []string{"removed"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterProfile(Profile{Name: name, Kind: ProfileHarness, SystemPromptSuffix: stringPointer("later suffix")}); err != nil {
		t.Fatal(err)
	}
	kept := tool.Func{Spec: tool.Definition{Name: "kept", Description: "old", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
		return tool.TextResult("ok"), nil
	}}
	removed := tool.Func{Spec: tool.Definition{Name: "removed", Description: "old", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
		return tool.TextResult("ok"), nil
	}}
	script := modeltest.New(model.Profile{}, modeltest.Step{
		Check: func(request model.Request) error {
			if request.Messages[0].Role != message.RoleSystem || !strings.Contains(request.Messages[0].TextContent(), "profile prompt") || !strings.Contains(request.Messages[0].TextContent(), "later suffix") {
				return errors.New("profile prompt missing")
			}
			if len(request.Tools) != 8 {
				return errors.New("excluded tool remained or built-in tools missing")
			}
			for _, definition := range request.Tools {
				if definition.Name == "removed" {
					return errors.New("removed tool remained")
				}
				if definition.Name == "kept" && definition.Description != "overridden description" {
					return errors.New("tool description was not overridden")
				}
			}
			return nil
		},
		Response: model.Response{Message: message.Assistant("done")},
	})
	compiled, err := New(Options{Model: script, ProfileNames: []string{name}, Tools: []tool.Tool{kept, removed}, DisableSubagents: true, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("go")}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := LookupProfile(name); !ok || len(RegisteredProfiles()) == 0 {
		t.Fatal("registered profile was not discoverable")
	}
}

func TestProfileToolExclusionOnlyFiltersTheModelBoundary(t *testing.T) {
	name := "test-profile-late-tool-exclusion"
	if err := RegisterProfile(Profile{Name: name, Kind: ProfileHarness, ExcludeTools: []string{"hidden"}}); err != nil {
		t.Fatal(err)
	}
	executions := 0
	hidden := tool.Func{Spec: tool.Definition{Name: "hidden", Description: "hidden", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}, Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
		executions++
		return tool.TextResult("executed"), nil
	}}
	script := modeltest.New(model.Profile{},
		modeltest.Step{Check: func(request model.Request) error {
			for _, definition := range request.Tools {
				if definition.Name == "hidden" {
					return errors.New("excluded tool reached the model")
				}
			}
			return nil
		}, Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "historical-call", Name: "hidden", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if len(request.Messages) == 0 || request.Messages[len(request.Messages)-1].Role != message.RoleTool || request.Messages[len(request.Messages)-1].TextContent() != "executed" {
				return errors.New("registered excluded tool did not execute")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: script, Tools: []tool.Tool{hidden}, ProfileNames: []string{name},
		DisableSubagents: true, DisableSummary: true, DisableTodo: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("go")}}); err != nil {
		t.Fatal(err)
	}
	if executions != 1 {
		t.Fatalf("executions = %d, want 1", executions)
	}
}

func TestProfileCannotExcludeRequiredFilesystem(t *testing.T) {
	_, err := New(Options{Model: modeltest.New(model.Profile{}), Profiles: []Profile{{Name: "bad", Kind: ProfileHarness, ExcludeMiddleware: []string{"filesystem"}}}})
	if err == nil || !strings.Contains(err.Error(), "required middleware") {
		t.Fatalf("error = %v", err)
	}
}

func TestProfileRegistrationRejectsMalformedKeys(t *testing.T) {
	for _, name := range []string{"", " provider", "provider ", "provider:", ":model", "provider: model", "a:b:c"} {
		if err := RegisterProfile(Profile{Name: name, Kind: ProfileHarness}); err == nil {
			t.Fatalf("profile name %q should fail", name)
		}
	}
}

func TestProfileCannotExcludeRequiredSubagentsOrUnknownMiddleware(t *testing.T) {
	for _, excluded := range []string{"subagents", "TypoMiddleware"} {
		_, err := New(Options{
			Model:    modeltest.New(model.Profile{}),
			Profiles: []Profile{{Name: "bad-" + excluded, Kind: ProfileHarness, ExcludeMiddleware: []string{excluded}}},
		})
		if err == nil {
			t.Fatalf("exclusion %q should fail", excluded)
		}
	}
	if err := RegisterProfile(Profile{Name: "private-exclusion", Kind: ProfileHarness, ExcludeMiddleware: []string{"_private"}}); err == nil {
		t.Fatal("private middleware exclusion should fail")
	}
}

func TestProfilesResolveFromModelAndMergeProviderWithExactModel(t *testing.T) {
	provider := "profile-auto-provider"
	modelID := "profile-auto-model"
	if err := RegisterProfile(Profile{
		Name: provider, Kind: ProfileHarness,
		BaseSystemPrompt: stringPointer("provider base"), SystemPromptSuffix: stringPointer("provider suffix"),
		ExcludeTools: []string{"glob"},
	}); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if err := RegisterProfile(Profile{
		Name: provider + ":" + modelID, Kind: ProfileHarness,
		SystemPromptSuffix: stringPointer("model suffix"), ExcludeTools: []string{"grep"},
		GeneralPurpose: &GeneralPurposeSubagentProfile{Enabled: &disabled},
	}); err != nil {
		t.Fatal(err)
	}
	script := modeltest.New(model.Profile{Provider: provider, Model: modelID}, modeltest.Step{Check: func(request model.Request) error {
		prompt := request.Messages[0].TextContent()
		userAt, baseAt, suffixAt := strings.Index(prompt, "user prompt"), strings.Index(prompt, "provider base"), strings.Index(prompt, "model suffix")
		if userAt < 0 || baseAt <= userAt || suffixAt <= baseAt || strings.Contains(prompt, "provider suffix") {
			return errors.New("profile prompt precedence is incorrect")
		}
		for _, definition := range request.Tools {
			if definition.Name == "glob" || definition.Name == "grep" || definition.Name == "task" {
				return errors.New("profile exclusion or general-purpose disable was ignored")
			}
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	compiled, err := New(Options{Model: script, SystemPrompt: "user prompt", DisableSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("go")}}); err != nil {
		t.Fatal(err)
	}
}

func stringPointer(value string) *string { return &value }

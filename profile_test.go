package dago

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
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
	kept := datool.Func{Spec: datool.Definition{Name: "kept", Description: "old", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		return datool.TextResult("ok"), nil
	}}
	removed := datool.Func{Spec: datool.Definition{Name: "removed", Description: "old", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		return datool.TextResult("ok"), nil
	}}
	script := modeltest.New(damodel.Profile{}, modeltest.Step{
		Check: func(request damodel.Request) error {
			if request.Messages[0].Role != damessage.RoleSystem || !strings.Contains(request.Messages[0].TextContent(), "profile prompt") || !strings.Contains(request.Messages[0].TextContent(), "later suffix") {
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
		Response: damodel.Response{Message: damessage.Assistant("done")},
	})
	compiled, err := New(Options{Model: script, ProfileNames: []string{name}, Tools: []datool.Tool{kept, removed}, DisableSubagents: true, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("go")}}); err != nil {
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
	hidden := datool.Func{Spec: datool.Definition{Name: "hidden", Description: "hidden", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}, Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		executions++
		return datool.TextResult("executed"), nil
	}}
	script := modeltest.New(damodel.Profile{},
		modeltest.Step{Check: func(request damodel.Request) error {
			for _, definition := range request.Tools {
				if definition.Name == "hidden" {
					return errors.New("excluded tool reached the model")
				}
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "historical-call", Name: "hidden", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if len(request.Messages) == 0 || request.Messages[len(request.Messages)-1].Role != damessage.RoleTool || request.Messages[len(request.Messages)-1].TextContent() != "executed" {
				return errors.New("registered excluded tool did not execute")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: script, Tools: []datool.Tool{hidden}, ProfileNames: []string{name},
		DisableSubagents: true, DisableSummary: true, DisableTodo: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("go")}}); err != nil {
		t.Fatal(err)
	}
	if executions != 1 {
		t.Fatalf("executions = %d, want 1", executions)
	}
}

func TestProfileCannotExcludeRequiredFilesystem(t *testing.T) {
	_, err := New(Options{Model: modeltest.New(damodel.Profile{}), Profiles: []Profile{{Name: "bad", Kind: ProfileHarness, ExcludeMiddleware: []string{"filesystem"}}}})
	if err == nil || !strings.Contains(err.Error(), "required middleware") {
		t.Fatalf("error = %v", err)
	}
}

func TestProfileUsesCanonicalSerializedMiddlewareNames(t *testing.T) {
	called := false
	replacement := dagent.Middleware{Name: "summarization", BeforeModel: func(context.Context, dastate.Values, dagent.Runtime) (dastate.Values, error) {
		called = true
		return nil, nil
	}}
	compiled, err := New(Options{
		Model: modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}}),
		Profiles: []Profile{{
			Name: "canonical-middleware-name", Kind: ProfileHarness,
			ExcludeMiddleware: []string{"SummarizationMiddleware"},
		}},
		Middleware: []dagent.Middleware{replacement}, DisableSubagents: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("go")}}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("canonical exclusion did not remove the replaced summarization slot")
	}
}

func TestProfileRejectsCanonicalRequiredMiddlewareNames(t *testing.T) {
	for _, name := range []string{"FilesystemMiddleware", "SubAgentMiddleware"} {
		_, err := New(Options{Model: modeltest.New(damodel.Profile{}), Profiles: []Profile{{
			Name: "required-" + name, Kind: ProfileHarness, ExcludeMiddleware: []string{name},
		}}})
		if err == nil || !strings.Contains(err.Error(), "required middleware") {
			t.Fatalf("exclusion %q error = %v", name, err)
		}
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
			Model:    modeltest.New(damodel.Profile{}),
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
	script := modeltest.New(damodel.Profile{Provider: provider, Model: modelID}, modeltest.Step{Check: func(request damodel.Request) error {
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
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled, err := New(Options{Model: script, SystemPrompt: "user prompt", DisableSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("go")}}); err != nil {
		t.Fatal(err)
	}
}

func TestProfilePromptPreservesExplicitEmptySlots(t *testing.T) {
	empty := ""
	base := "base"
	if got := applyProfilePrompt(Profile{BaseSystemPrompt: &empty}, "", "ignored"); got != "" {
		t.Fatalf("empty base = %q", got)
	}
	if got := applyProfilePrompt(Profile{BaseSystemPrompt: &base, SystemPromptSuffix: &empty}, "", "ignored"); got != "base\n\n" {
		t.Fatalf("empty suffix = %q", got)
	}
	if got := applyProfilePrompt(Profile{SystemPromptSuffix: stringPointer("suffix")}, "user", ""); got != "user\n\nsuffix" {
		t.Fatalf("user and suffix = %q", got)
	}
}

func stringPointer(value string) *string { return &value }

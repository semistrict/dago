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

func TestExplicitProfilesMergeToolOverrideAndExclusion(t *testing.T) {
	profile := MergeProfiles(Profile{
		Name: "test-explicit-profile", SystemPrompt: "profile prompt",
		ToolDescriptions: map[string]string{"kept": "overridden description"},
		ExcludeTools:     []string{"removed"},
	}, Profile{SystemPromptSuffix: new("later suffix")})
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
	compiled := New(script, WithProfiles(profile), WithTools(kept, removed), WithFilesystem(Filesystem{}))

	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}
}

func TestProfileToolExclusionOnlyFiltersTheModelBoundary(t *testing.T) {
	profile := Profile{Name: "test-profile-late-tool-exclusion", ExcludeTools: []string{"hidden"}}
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
	compiled := New(
		script, WithProfiles(profile), WithTools(hidden), WithFilesystem(Filesystem{}),
	)

	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}
	if executions != 1 {
		t.Fatalf("executions = %d, want 1", executions)
	}
}

func TestProfileCanExcludeOptionalFilesystem(t *testing.T) {
	New(modeltest.New(damodel.Profile{}), WithProfiles(Profile{Name: "without-filesystem", ExcludeMiddleware: []string{"filesystem"}}), WithFilesystem(Filesystem{}))
}

func TestProfileUsesCanonicalSerializedMiddlewareNames(t *testing.T) {
	called := false
	replacement := dagent.Middleware{Name: "summarization", BeforeModel: func(context.Context, dastate.Values, dagent.Runtime) (dastate.Values, error) {
		called = true
		return nil, nil
	}}
	compiled := New(
		modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}}), WithProfiles(Profile{
			Name:              "canonical-middleware-name",
			ExcludeMiddleware: []string{"SummarizationMiddleware"},
		}), WithFilesystem(Filesystem{}), WithSummarization(Summarization{}), WithMiddleware(replacement),
	)

	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("canonical exclusion did not remove the replaced summarization slot")
	}
}

func TestProfileRejectsRequiredMiddlewareNames(t *testing.T) {
	for _, name := range []string{"patch_tool_calls", "PatchToolCallsMiddleware"} {
		requirePanicContaining(t, "required middleware", func() {
			New(modeltest.New(damodel.Profile{}), WithProfiles(Profile{
				Name: "required-" + name, ExcludeMiddleware: []string{name},
			}), WithFilesystem(Filesystem{}), WithSummarization(Summarization{}), WithSubagents())
		})
	}
}

func TestExplicitProfileRejectsMalformedNames(t *testing.T) {
	for _, name := range []string{"", " provider", "provider ", "provider:", ":model", "provider: model", "a:b:c"} {
		if name == "" {
			continue
		}
		requirePanicContaining(t, "profile", func() {
			New(modeltest.New(damodel.Profile{}), WithProfiles(Profile{Name: name}), WithFilesystem(Filesystem{}), WithSummarization(Summarization{}), WithSubagents())
		})
	}
}

func TestProfileCanExcludeOptionalSubagentsButRejectsUnknownMiddleware(t *testing.T) {
	New(modeltest.New(damodel.Profile{}), WithProfiles(Profile{Name: "without-subagents", ExcludeMiddleware: []string{"subagents"}}), WithSubagents())
	requirePanicContaining(t, "middleware", func() {
		New(modeltest.New(damodel.Profile{}), WithProfiles(Profile{Name: "bad-unknown", ExcludeMiddleware: []string{"TypoMiddleware"}}))
	})
	requirePanicContaining(t, "middleware", func() {
		New(modeltest.New(damodel.Profile{}), WithProfiles(Profile{Name: "private-exclusion", ExcludeMiddleware: []string{"_private"}}), WithFilesystem(Filesystem{}), WithSummarization(Summarization{}), WithSubagents())
	})
}

func TestExplicitProfilesMergeProviderWithExactModel(t *testing.T) {
	provider := "profile-auto-provider"
	modelID := "profile-auto-model"
	providerProfile := Profile{
		Name:             provider,
		BaseSystemPrompt: new("provider base"), SystemPromptSuffix: new("provider suffix"),
		ExcludeTools: []string{"glob"},
	}
	modelProfile := Profile{
		Name:               provider + ":" + modelID,
		SystemPromptSuffix: new("model suffix"), ExcludeTools: []string{"grep"},
		GeneralPurpose: &GeneralPurposeSubagentProfile{Mode: GeneralPurposeSubagentDisabled},
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
	compiled := New(script, WithProfiles(providerProfile, modelProfile), WithSystemMessage(damessage.System("user prompt")), WithFilesystem(Filesystem{}), WithSubagents())

	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("go")); err != nil {
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
	if got := applyProfilePrompt(Profile{SystemPromptSuffix: new("suffix")}, "user", ""); got != "user\n\nsuffix" {
		t.Fatalf("user and suffix = %q", got)
	}
}

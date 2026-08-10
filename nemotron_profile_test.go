package dago

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
)

func TestNemotronProfilesRegisteredForSupportedModels(t *testing.T) {
	wantMiddleware := []string{
		"NemotronProgressBudgetMiddleware",
		"NemotronPolicyNudgeMiddleware",
		"NemotronToolCallShim",
		"ReadFileContinuationNoticeMiddleware",
		"ToolRetryMiddleware",
		"ModelRateLimitRetryMiddleware",
		"ChatNVIDIAMessageCompatibilityMiddleware",
		"NemotronReasoningTagCleanupMiddleware",
		"NemotronTextToolCallParser",
		"FollowupDisciplineMiddleware",
		"EntityResolutionGuardMiddleware",
		"FinalAnswerGuardMiddleware",
	}
	for _, spec := range nemotronModelSpecs {
		profile, exists := LookupProfile(spec)
		if !exists {
			t.Errorf("profile %q is not registered", spec)
			continue
		}
		if profile.SystemPromptSuffix == nil || !strings.Contains(*profile.SystemPromptSuffix, "<state_changes>") {
			t.Errorf("profile %q prompt = %#v", spec, profile.SystemPromptSuffix)
		}
		if !strings.Contains(profile.ToolDescriptions["read_file"], "whole/full file") {
			t.Errorf("profile %q read description = %q", spec, profile.ToolDescriptions["read_file"])
		}
		var names []string
		for _, middleware := range profile.Middleware {
			names = append(names, middleware.Name)
		}
		if !reflect.DeepEqual(names, wantMiddleware) {
			t.Errorf("profile %q middleware = %#v", spec, names)
		}
	}
}

func TestNemotronProfileAutoResolvesPromptAndToolOverride(t *testing.T) {
	script := modeltest.New(model.Profile{Provider: "nvidia", Model: "nvidia/nemotron-3-ultra-550b-a55b"}, modeltest.Step{
		Check: func(request model.Request) error {
			if !strings.Contains(request.Messages[0].TextContent(), "<state_changes>") {
				return errors.New("profile prompt missing")
			}
			for _, definition := range request.Tools {
				if definition.Name == "read_file" && !strings.Contains(definition.Description, "whole/full file") {
					return errors.New("read_file description override missing")
				}
			}
			return nil
		},
		Response: model.Response{Message: message.Assistant("done")},
	})
	compiled, err := New(Options{Model: script, DisableSubagents: true, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("hello")}}); err != nil {
		t.Fatal(err)
	}
}

package nemotron

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

func TestProfilesCoverSupportedModels(t *testing.T) {
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
	profiles := Profiles()
	if len(profiles) != len(nemotronModelSpecs) {
		t.Fatalf("profiles = %d, want %d", len(profiles), len(nemotronModelSpecs))
	}
	for index, spec := range nemotronModelSpecs {
		profile := profiles[index]
		if profile.Name != spec {
			t.Errorf("profile name = %q, want %q", profile.Name, spec)
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

func TestProfileAppliesPromptAndToolOverride(t *testing.T) {
	profile, exists := Profile("nvidia:nvidia/nemotron-3-ultra-550b-a55b")
	if !exists {
		t.Fatal("profile missing")
	}
	script := modeltest.New(damodel.Profile{Provider: "nvidia", Model: "nvidia/nemotron-3-ultra-550b-a55b"}, modeltest.Step{
		Check: func(request damodel.Request) error {
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
		Response: damodel.Response{Message: damessage.Assistant("done")},
	})
	compiled := dago.New(script, dago.WithProfiles(profile), dago.WithFilesystem(dago.Filesystem{}))

	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("hello")); err != nil {
		t.Fatal(err)
	}
}

package dago

import (
	"context"
	"strings"
	"testing"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
)

func TestParseSkillYAMLMetadata(t *testing.T) {
	content := "---\nname: research\ndescription: >-\n  Research carefully across\n  several primary sources\nlicense: MIT\ncompatibility: Go 1.26+\nmetadata:\n  owner: platform\n  revision: 3\nallowed-tools:\n  - read_file\n  - grep\n  - 7\n---\n# Instructions\n"
	skill, warnings, err := parseSkill(content, "/skills/research/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if skill.Description != "Research carefully across several primary sources" || skill.Metadata["revision"] != "3" {
		t.Fatalf("skill = %#v", skill)
	}
	if strings.Join(skill.AllowedTools, ",") != "read_file,grep" {
		t.Fatalf("allowed tools = %v", skill.AllowedTools)
	}
}

func TestSkillsLaterSourceWinsAndWarningsAreUntrusted(t *testing.T) {
	memory, err := backend.NewMemory(map[string]backend.FileData{
		"/base/research/SKILL.md":    {Content: "---\nname: research\ndescription: base\n---\nbody", Encoding: backend.EncodingUTF8},
		"/project/research/SKILL.md": {Content: "---\nname: research\ndescription: project\n---\nbody", Encoding: backend.EncodingUTF8},
		"/project/broken/SKILL.md":   {Content: "not yaml", Encoding: backend.EncodingUTF8},
	})
	if err != nil {
		t.Fatal(err)
	}
	var observedWarnings []string
	middleware, err := SkillsMiddleware(SkillsOptions{Backend: memory, Sources: []string{"/base", "/project"}, Warn: func(value string) {
		observedWarnings = append(observedWarnings, value)
	}})
	if err != nil {
		t.Fatal(err)
	}
	script := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		prompt := request.Messages[0].TextContent()
		if !strings.Contains(prompt, "**research**: project") || strings.Contains(prompt, "**research**: base") {
			return &skillTestError{"priority merge missing"}
		}
		if !strings.Contains(prompt, "<skill_load_warnings>") || !strings.Contains(prompt, "untrusted diagnostics") {
			return &skillTestError{"safe warnings missing"}
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	compiled, err := agent.New(agent.Options{Model: script, Middleware: []agent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("go")}}); err != nil {
		t.Fatal(err)
	}
	if len(observedWarnings) == 0 {
		t.Fatal("warning callback was not called")
	}
}

func TestSkillsSkipDiscoveryWhenCheckpointedMetadataExists(t *testing.T) {
	memory, err := backend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := SkillsMiddleware(SkillsOptions{Backend: memory, Sources: []string{"/skills"}})
	if err != nil {
		t.Fatal(err)
	}
	update, err := middleware.BeforeAgent(context.Background(), map[string]any{"skills": []Skill{}}, agent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if update != nil {
		t.Fatalf("update = %#v, want nil", update)
	}
}

func TestSkillsPromptSupportsLabelsEmptyLibrariesAndDisabling(t *testing.T) {
	memory, err := backend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := SkillsMiddleware(SkillsOptions{
		Backend:        memory,
		Sources:        []string{"/home/me/.agents/skills"},
		LabeledSources: []SkillSource{{Path: "/project/skills", Label: "Project"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	script := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		prompt := request.Messages[0].TextContent()
		for _, expected := range []string{
			"**Agents Skills**: `/home/me/.agents/skills`",
			"**Project Skills**: `/project/skills` (higher priority)",
			"No skills available yet",
			"/home/me/.agents/skills or /project/skills",
		} {
			if !strings.Contains(prompt, expected) {
				return &skillTestError{"missing " + expected}
			}
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	compiled, err := agent.New(agent.Options{Model: script, Middleware: []agent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("go")}}); err != nil {
		t.Fatal(err)
	}

	disabled := ""
	middleware, err = SkillsMiddleware(SkillsOptions{Backend: memory, SystemPrompt: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	script = modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if request.Messages[0].TextContent() != "go" {
			return &skillTestError{"disabled skills prompt changed the request"}
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	compiled, err = agent.New(agent.Options{Model: script, Middleware: []agent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("go")}}); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsCustomPromptRequiresProgressiveDisclosureSlots(t *testing.T) {
	memory, err := backend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	invalid := "{skills_list}"
	if _, err := SkillsMiddleware(SkillsOptions{Backend: memory, SystemPrompt: &invalid}); err == nil {
		t.Fatal("expected missing prompt slots to fail")
	}
	valid := "Locations:\n{skills_locations}{skills_load_warnings}\nSkills:\n{skills_list}"
	if _, err := SkillsMiddleware(SkillsOptions{Backend: memory, SystemPrompt: &valid}); err != nil {
		t.Fatal(err)
	}
}

type skillTestError struct{ text string }

func (value *skillTestError) Error() string { return value.text }

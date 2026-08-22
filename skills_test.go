package dago

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint/serde"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

func mustSkills(backend dabackend.Backend, options Skills) dagent.Middleware {
	middleware, err := newSkills(backend, options)
	if err != nil {
		panic(err)
	}
	return middleware
}

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
	memory := dabackend.NewMemory(map[string]dabackend.FileData{
		"/base/research/SKILL.md":    {Content: "---\nname: research\ndescription: base\n---\nbody", Encoding: dabackend.EncodingUTF8},
		"/project/research/SKILL.md": {Content: "---\nname: research\ndescription: project\n---\nbody", Encoding: dabackend.EncodingUTF8},
		"/project/broken/SKILL.md":   {Content: "not yaml", Encoding: dabackend.EncodingUTF8},
	})
	var observedWarnings []string
	middleware := mustSkills(memory, Skills{Sources: []string{"/base", "/project"}, Warn: func(value string) {
		observedWarnings = append(observedWarnings, value)
	}})

	script := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		prompt := request.Messages[0].TextContent()
		if !strings.Contains(prompt, "**research**: project") || strings.Contains(prompt, "**research**: base") {
			return &skillTestError{"priority merge missing"}
		}
		if !strings.Contains(prompt, "<skill_load_warnings>") || !strings.Contains(prompt, "untrusted diagnostics") {
			return &skillTestError{"safe warnings missing"}
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled := dagent.New(script, dagent.Options{Middleware: []dagent.Middleware{middleware}})

	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}
	if len(observedWarnings) == 0 {
		t.Fatal("warning callback was not called")
	}
}

type partialSkillsBackend struct {
	dabackend.Backend
	listing dabackend.ListResult
	err     error
}

func (partial partialSkillsBackend) List(context.Context, string) (dabackend.ListResult, error) {
	return partial.listing, partial.err
}

func TestSkillsRetainPartialListingResults(t *testing.T) {
	memory := dabackend.NewMemory(map[string]dabackend.FileData{
		"/skills/research/SKILL.md": {Content: "---\nname: research\ndescription: found despite warning\n---\nbody", Encoding: dabackend.EncodingUTF8},
	})
	partial := partialSkillsBackend{
		Backend: memory,
		listing: dabackend.ListResult{Entries: []dabackend.FileInfo{{Path: "/skills/research/", IsDir: true}}},
		err:     errors.New("one directory could not be inspected"),
	}
	middleware := mustSkills(partial, Skills{Sources: []string{"/skills"}})

	update, err := middleware.BeforeAgent(context.Background(), map[string]any{}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	sk := skillsFromState(update["skills"])
	warnings := stringsFromState(update["skills_load_errors"])
	if len(sk) != 1 || sk[0].Name != "research" || len(warnings) != 1 || !strings.Contains(warnings[0], "could not be inspected") {
		t.Fatalf("skills = %#v, warnings = %#v", sk, warnings)
	}
}

func TestSkillsSkipDiscoveryWhenCheckpointedMetadataExists(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	middleware := mustSkills(memory, Skills{Sources: []string{"/skills"}})

	update, err := middleware.BeforeAgent(context.Background(), map[string]any{"skills": []Skill{}}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if update != nil {
		t.Fatalf("update = %#v, want nil", update)
	}
}

func TestSkillsPromptSupportsLabelsEmptyLibrariesAndDisabling(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	middleware := mustSkills(
		memory, Skills{

			Sources:        []string{"/home/me/.agents/skills"},
			LabeledSources: []SkillSource{{Path: "/project/skills", Label: "Project"}},
		})

	script := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
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
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled := dagent.New(script, dagent.Options{Middleware: []dagent.Middleware{middleware}})

	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}

	middleware = mustSkills(memory, Skills{SystemPrompt: PromptTemplate{Mode: PromptDisabled}})

	script = modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		messages := messagesWithoutSystem(request)
		if len(messages) != 1 || messages[0].TextContent() != "go" {
			return &skillTestError{"disabled skills prompt changed the request"}
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled = dagent.New(script, dagent.Options{Middleware: []dagent.Middleware{middleware}})

	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsCustomPromptRequiresProgressiveDisclosureSlots(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	invalid := "{skills_list}"
	requirePanicContaining(t, "missing required slot", func() {
		mustSkills(memory, Skills{SystemPrompt: PromptTemplate{Mode: PromptCustom, Text: invalid}})
	})
	valid := "Locations:\n{skills_locations}{skills_load_warnings}\nSkills:\n{skills_list}"
	mustSkills(memory, Skills{SystemPrompt: PromptTemplate{Mode: PromptCustom, Text: valid}})
}

func TestSkillsCatalogUsesApplicationActivationAndFilesystemOverrides(t *testing.T) {
	memory := dabackend.NewMemory(map[string]dabackend.FileData{
		"/project/research/SKILL.md": {Content: "---\nname: research\ndescription: project\n---\nbody", Encoding: dabackend.EncodingUTF8},
	})
	middleware := mustSkills(
		memory, Skills{

			Sources: []string{"/project"},
			Catalog: []Skill{
				{Name: "research", Description: "catalog"},
				{Name: "builtin", Description: "embedded"},
			},
			Activate: func(item Skill) string { return "Run skill show " + item.Name },
		})

	update, err := middleware.BeforeAgent(context.Background(), map[string]any{}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serde.New(serde.Limits{}).Encode(update["skills"]); err != nil {
		t.Fatalf("skills checkpoint state is not language-neutral: %v", err)
	}
	script := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		prompt := request.Messages[0].TextContent()
		for _, expected := range []string{"**research**: project", "**builtin**: embedded", "Run skill show research", "Run skill show builtin"} {
			if !strings.Contains(prompt, expected) {
				return &skillTestError{"missing " + expected}
			}
		}
		if strings.Contains(prompt, "**research**: catalog") {
			return &skillTestError{"filesystem source did not override catalog"}
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled := dagent.New(script, dagent.Options{Middleware: []dagent.Middleware{middleware}})

	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsCatalogBodySuppliesActivationAndFilesystemOverridesIt(t *testing.T) {
	memory := dabackend.NewMemory(map[string]dabackend.FileData{
		"/project/herdr/SKILL.md": {Content: "---\nname: herdr\ndescription: project\n---\nproject body", Encoding: dabackend.EncodingUTF8},
	})
	middleware := mustSkills(memory, Skills{
		Sources: []string{"/project"},
		Catalog: []Skill{
			{Name: "herdr", Description: "catalog", Body: "Run the catalog command."},
			{Name: "builtin", Description: "embedded", Body: "Follow the embedded instructions."},
		},
	})

	update, err := middleware.BeforeAgent(context.Background(), map[string]any{}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serde.New(serde.Limits{}).Encode(update["skills"]); err != nil {
		t.Fatalf("skills checkpoint state is not language-neutral: %v", err)
	}
	stateSkills := skillsFromState(update["skills"])
	if len(stateSkills) != 2 || stateSkills[0].Name != "builtin" || stateSkills[0].Body != "Follow the embedded instructions." || stateSkills[1].Name != "herdr" || stateSkills[1].Body != "" {
		t.Fatalf("skills = %#v", stateSkills)
	}

	script := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		prompt := request.Messages[0].TextContent()
		for _, expected := range []string{"**builtin**: embedded", "Follow the embedded instructions.", "**herdr**: project", "Read `/project/herdr/SKILL.md` for full instructions"} {
			if !strings.Contains(prompt, expected) {
				return &skillTestError{"missing " + expected}
			}
		}
		if strings.Contains(prompt, "Run the catalog command.") {
			return &skillTestError{"overridden catalog body remained"}
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled := dagent.New(script, dagent.Options{Middleware: []dagent.Middleware{middleware}})

	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsUseNativeModelArtifactsWithoutPromptInjection(t *testing.T) {
	memory := dabackend.NewMemory(map[string]dabackend.FileData{
		"/project/research/SKILL.md": {Content: "---\nname: research\ndescription: Research carefully\n---\nUse primary sources.", Encoding: dabackend.EncodingUTF8},
	})
	middleware := mustSkills(memory, Skills{
		Sources: []string{"/project"},
		Catalog: []Skill{{Name: "review", Description: "Review changes", Body: "Inspect the diff."}},
	})
	script := modeltest.New(damodel.Profile{NativeSkills: true}, modeltest.Step{Check: func(request damodel.Request) error {
		if len(request.Skills) != 2 {
			return &skillTestError{fmt.Sprintf("native skills = %#v", request.Skills)}
		}
		if request.Skills[0].Name != "research" || request.Skills[0].Instructions != "Use primary sources." ||
			request.Skills[1].Name != "review" || request.Skills[1].Instructions != "Inspect the diff." {
			return &skillTestError{fmt.Sprintf("native skills = %#v", request.Skills)}
		}
		if strings.Contains(request.Messages[0].TextContent(), "Skills System") || strings.Contains(request.Messages[0].TextContent(), "Use primary sources.") {
			return &skillTestError{"native skill instructions were injected into the prompt"}
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled := dagent.New(script, dagent.Options{Middleware: []dagent.Middleware{middleware}})
	if _, err := compiled.Invoke(context.Background(), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}
}

type skillTestError struct{ text string }

func (value *skillTestError) Error() string { return value.text }

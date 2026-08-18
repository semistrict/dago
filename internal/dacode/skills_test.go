package dacode

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
	checkpointsqlite "github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/daskill"
)

type skillFakeRunner struct {
	*fakeRunner
	entries     []daskill.Entry
	load        daskill.Entry
	loadErr     error
	trusted     string
	loadCalls   int
	trustResult daskill.Entry
}

func (runner *skillFakeRunner) ListSkills(context.Context) ([]daskill.Entry, error) {
	return append([]daskill.Entry(nil), runner.entries...), nil
}

func (runner *skillFakeRunner) LoadSkill(context.Context, string) (daskill.Entry, error) {
	runner.loadCalls++
	if runner.trusted != "" && runner.trustResult.Skill.Name != "" {
		return runner.trustResult, nil
	}
	return runner.load, runner.loadErr
}

func (runner *skillFakeRunner) TrustSkill(_ context.Context, target string) error {
	runner.trusted = target
	return nil
}

func TestSkillsCLIPrecedenceCreateInfoDeleteAndTrust(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(root, description string) {
		t.Helper()
		dir := filepath.Join(root, "review")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := "---\nname: review\ndescription: " + description + "\n---\n\nbody"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".agents", "skills"), "user")
	write(filepath.Join(workspace, ".claude", "skills"), "project claude")
	var output bytes.Buffer
	if err := runSkillsCommand(t.Context(), []string{"list", "--working-dir", workspace, "--state-dir", state}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "review\tproject-claude\tproject claude") || !strings.Contains(output.String(), "remember\tbuilt-in") {
		t.Fatalf("list output = %q", output.String())
	}
	output.Reset()
	if err := runSkillsCommand(t.Context(), []string{"info", "--working-dir", workspace, "--state-dir", state, "review"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Source: project-claude") || !strings.Contains(output.String(), "body") {
		t.Fatalf("info output = %q", output.String())
	}
	output.Reset()
	if err := runSkillsCommand(t.Context(), []string{"create", "--project", "--working-dir", workspace, "new-skill"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(workspace, ".deepagents", "skills", "new-skill", "SKILL.md")
	if _, err := os.Stat(created); err != nil {
		t.Fatal(err)
	}
	if err := runSkillsCommand(t.Context(), []string{"delete", "--project", "--working-dir", workspace, "new-skill"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted skill still exists: %v", err)
	}
	if err := runSkillsCommand(t.Context(), []string{"create", "--state-dir", state, "--agent", "reviewer", "personal-skill"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	personal := filepath.Join(state, "reviewer", agentSkillsDirectory, "personal-skill", "SKILL.md")
	if _, err := os.Stat(personal); err != nil {
		t.Fatalf("named-agent skill was not created: %v", err)
	}
	if err := runSkillsCommand(t.Context(), []string{"trust", "--state-dir", state, "add", workspace}, &output, &output); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runSkillsCommand(t.Context(), []string{"trust", "--state-dir", state, "list"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(workspace)
	if !strings.Contains(output.String(), canonical) {
		t.Fatalf("trust output = %q", output.String())
	}
}

func TestSkillsCLIRefusesDeleteSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated Windows privileges")
	}
	workspace := t.TempDir()
	external := t.TempDir()
	if _, err := daskill.CreateTemplate(external, "linked"); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(workspace, ".deepagents", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, "linked"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	err := runSkillsCommand(t.Context(), []string{"delete", "--project", "--working-dir", workspace, "linked"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("delete error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(external, "linked", "SKILL.md")); err != nil {
		t.Fatalf("external target was changed: %v", err)
	}
}

func TestRuntimeSkillSourcesUsePinnedPrecedenceAndReadOnlyMounts(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := daskill.CreateTemplate(filepath.Join(home, ".agents", "skills"), "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := daskill.CreateTemplate(filepath.Join(home, ".claude", "skills"), "claude-shared"); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{".deepagents/skills", ".agents/skills", ".claude/skills"} {
		if err := os.MkdirAll(filepath.Join(workspace, relative), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	routes, userAgents, userClaude, err := runtimeUserSkillRoutes(home)
	if err != nil || !userAgents || !userClaude {
		t.Fatalf("routes = %#v, %v, %v, %v", routes, userAgents, userClaude, err)
	}
	memory := dabackend.NewMemory(nil)
	backend := dabackend.NewComposite(memory, routes)
	downloads := backend.Download(t.Context(), []string{userAgentSkillsMount + "/shared/SKILL.md"})
	if len(downloads) != 1 || downloads[0].Error != "" || !strings.Contains(string(downloads[0].Content), "name: shared") {
		t.Fatalf("download = %#v", downloads)
	}
	if _, err := backend.Write(t.Context(), userAgentSkillsMount+"/shared/SKILL.md", "changed"); !errors.Is(err, errReadOnlySkillSource) {
		t.Fatalf("write error = %v", err)
	}
	want := strings.Join([]string{
		agentMemoryMount + "/" + agentSkillsDirectory,
		userAgentSkillsMount,
		"/.deepagents/skills",
		"/.agents/skills",
		userClaudeSkillsMount,
		"/.claude/skills",
	}, "\n")
	if got := strings.Join(orderedRuntimeSkillSources(workspace, userAgents, userClaude), "\n"); got != want {
		t.Fatalf("sources =\n%s\nwant\n%s", got, want)
	}
}

func TestTUISkillsListAliasesAndTrustRetry(t *testing.T) {
	base := &fakeRunner{streams: []eventStream{&fakeEventStream{}}}
	runner := &skillFakeRunner{fakeRunner: base, entries: []daskill.Entry{{Skill: daskill.Skill{Name: "remember", Description: "remember things"}, Source: "built-in"}}}
	model := newTUIModel(t.Context(), runner, t.TempDir(), "model", "thread", false, false, "")
	runner.load = daskill.Entry{Skill: daskill.Skill{Name: "remember", Body: "remember body"}, Source: "built-in"}
	command, handled := model.slashCommand("/remember keep this")
	if !handled || command == nil {
		t.Fatal("/remember was not handled")
	}
	updated, _ := model.Update(command())
	model = updated.(*tuiModel)
	if len(runner.inputs) != 1 || !strings.Contains(runner.inputs[0].Messages[0].TextContent(), "remember body") || !strings.Contains(runner.inputs[0].Messages[0].TextContent(), "keep this") {
		t.Fatalf("skill input = %#v", runner.inputs)
	}

	model.running = false
	model.stream = nil
	target := t.TempDir()
	runner.load = daskill.Entry{Skill: daskill.Skill{Name: "linked"}, TrustRequired: true, TargetDir: target}
	runner.loadErr = &daskill.TrustRequiredError{Skill: "linked", TargetDir: target}
	runner.trustResult = daskill.Entry{Skill: daskill.Skill{Name: "linked", Body: "linked body"}, Source: "project-agents"}
	command, handled = model.slashCommand("/skill:linked do it")
	if !handled || command == nil {
		t.Fatal("dynamic skill was not handled")
	}
	updated, _ = model.Update(command())
	model = updated.(*tuiModel)
	if model.skillTrust == nil || !strings.Contains(model.renderSkillTrust(), "Trust external skill?") {
		t.Fatalf("trust state = %#v", model.skillTrust)
	}
	trustCommand, handled := model.handleSkillTrustKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || trustCommand == nil {
		t.Fatal("trust confirmation was not handled")
	}
	updated, retry := model.Update(trustCommand())
	model = updated.(*tuiModel)
	if retry == nil || runner.trusted != target {
		t.Fatalf("trust retry = %v target=%q", retry, runner.trusted)
	}
	updated, _ = model.Update(retry())
	model = updated.(*tuiModel)
	if runner.loadCalls < 2 || len(runner.inputs) != 2 || !strings.Contains(runner.inputs[1].Messages[0].TextContent(), "linked body") {
		t.Fatalf("retried inputs = %#v calls=%d", runner.inputs, runner.loadCalls)
	}
}

func TestThreadInspectorCLIReadsDatabaseWithoutMutation(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "threads.db")
	saver, err := checkpointsqlite.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := dacheckpoint.Empty(0)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.ChannelValues[dagent.MessagesKey] = dacheckpoint.DeltaSnapshot{Value: []damessage.Message{damessage.Human("question"), damessage.Assistant("answer")}}
	checkpoint.ChannelVersions[dagent.MessagesKey] = "v1"
	if _, err := saver.Put(t.Context(), dacheckpoint.Config{ThreadID: "cli-thread"}, checkpoint, dacheckpoint.Metadata{}, map[string]string{dagent.MessagesKey: "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := saver.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runThreadInspectorCommand(t.Context(), skillCLIOptions{}, []string{"--db", databasePath, "--mode", "transcript", "cli-thread"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"role":"human"`) || !strings.Contains(output.String(), `"role":"assistant"`) {
		t.Fatalf("transcript = %q", output.String())
	}
	after, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("read-only inspection changed database size from %d to %d", before.Size(), after.Size())
	}
}

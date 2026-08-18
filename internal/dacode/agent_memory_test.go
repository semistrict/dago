package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

func TestAgentMemoryCreatesUsefulDefaultAndIsolatesSelectedAgents(t *testing.T) {
	stateDir := t.TempDir()
	identity := &agentIdentity{name: defaultAgentName}
	backend, err := openAgentMemory(stateDir, identity)
	if err != nil {
		t.Fatal(err)
	}
	defaultPath := filepath.Join(stateDir, defaultAgentName, agentInstructionsFilename)
	info, err := os.Stat(defaultPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("default memory info = %#v, err = %v", info, err)
	}
	if contents, err := os.ReadFile(defaultPath); err != nil || len(contents) != 0 {
		t.Fatalf("default memory = %q, err = %v", contents, err)
	}
	if _, err := backend.Write(t.Context(), "/AGENTS.md", "default memory"); err != nil {
		t.Fatal(err)
	}

	reviewerDir := filepath.Join(stateDir, "reviewer")
	if err := os.Mkdir(reviewerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reviewerDir, agentInstructionsFilename), []byte("reviewer memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity.set("reviewer")
	if got := downloadAgentMemory(t, t.Context(), backend); got != "reviewer memory" {
		t.Fatalf("selected memory = %q", got)
	}
	if _, err := backend.Write(t.Context(), "/AGENTS.md", "updated reviewer"); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, defaultPath); got != "default memory" {
		t.Fatalf("default memory changed to %q", got)
	}
	if got := readFileString(t, filepath.Join(reviewerDir, agentInstructionsFilename)); got != "updated reviewer" {
		t.Fatalf("reviewer memory = %q", got)
	}
	for _, alias := range []string{"/reviewer/AGENTS.md", "/../dacode/AGENTS.md", "/other.md", "/"} {
		if _, err := backend.Write(t.Context(), alias, "cross-agent write"); err == nil {
			t.Errorf("write through alias %q succeeded", alias)
		}
	}
}

func TestAgentMemoryRuntimeBindingPinsResumedSession(t *testing.T) {
	stateDir := t.TempDir()
	identity := &agentIdentity{name: defaultAgentName}
	backend, err := openAgentMemory(stateDir, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureAgentMemoryFile(stateDir, "reviewer"); err != nil {
		t.Fatal(err)
	}
	identity.set("reviewer")
	bound, err := backend.BindRuntime(t.Context(), dastate.Values{sessionAgentNameKey: defaultAgentName})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Write(bound, "/AGENTS.md", "resumed default memory"); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, filepath.Join(stateDir, defaultAgentName, agentInstructionsFilename)); got != "resumed default memory" {
		t.Fatalf("bound default memory = %q", got)
	}
	if got := readFileString(t, filepath.Join(stateDir, "reviewer", agentInstructionsFilename)); got != "" {
		t.Fatalf("selected reviewer memory changed to %q", got)
	}
	if _, err := backend.BindRuntime(t.Context(), dastate.Values{sessionAgentNameKey: 42}); err == nil {
		t.Fatal("non-string persisted agent identity was accepted")
	}
}

func TestAgentMemorySwitchLoadsNewThreadAndKeepsResumedThreadPinned(t *testing.T) {
	stateDir := t.TempDir()
	identity := &agentIdentity{name: defaultAgentName}
	memoryBackend, err := openAgentMemory(stateDir, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memoryBackend.Write(t.Context(), "/AGENTS.md", "default durable memory"); err != nil {
		t.Fatal(err)
	}
	if err := ensureAgentMemoryFile(stateDir, "reviewer"); err != nil {
		t.Fatal(err)
	}
	identity.set("reviewer")
	if _, err := memoryBackend.Write(t.Context(), "/AGENTS.md", "reviewer durable memory"); err != nil {
		t.Fatal(err)
	}
	identity.set(defaultAgentName)
	workspace := dabackend.NewMemory(nil)
	backend := dabackend.NewComposite(workspace, map[string]dabackend.Backend{agentMemoryMount: memoryBackend})
	checkMemory := func(expected, excluded string) func(damodel.Request) error {
		return func(request damodel.Request) error {
			prompt := modelSystemText(request)
			if prompt == "" {
				return errors.New("memory prompt is missing")
			}
			if !strings.Contains(prompt, expected) || strings.Contains(prompt, excluded) {
				return errors.New("wrong agent memory was loaded")
			}
			return nil
		}
	}
	model := modeltest.New(damodel.Profile{SupportsSeparateSystemMessage: true},
		modeltest.Step{Check: checkMemory("default durable memory", "reviewer durable memory"), Response: damodel.Response{Message: damessage.Assistant("first")}},
		modeltest.Step{Check: checkMemory("default durable memory", "reviewer durable memory"), Response: damodel.Response{Message: damessage.Assistant("resumed")}},
		modeltest.Step{Check: checkMemory("reviewer durable memory", "default durable memory"), Response: damodel.Response{Message: damessage.Assistant("switched")}},
	)
	saver := dacheckpoint.NewMemorySaver()
	agent := dago.NewAgent(
		model,
		dago.WithBackend(backend),
		dago.WithMemory(configureAgentMemory(dago.Memory{}, false)),
		dago.WithMiddleware(agentIdentityMiddleware(identity, stateDir)),
		dago.WithStateFields(sessionStateFields()),
		dago.WithSaver(saver),
		dago.WithRetainedThreadState(),
		dago.WithoutSubagents(),
		dago.WithoutSummary(),
	)
	defaultThread := dacheckpoint.Config{ThreadID: "default-thread"}
	if _, err := agent.Invoke(t.Context(), dagent.Input{Config: defaultThread, Messages: []damessage.Message{damessage.Human("first")}}); err != nil {
		t.Fatal(err)
	}
	identity.set("reviewer")
	if _, err := agent.Invoke(t.Context(), dagent.Input{Config: defaultThread, Messages: []damessage.Message{damessage.Human("resume")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Invoke(t.Context(), dagent.Input{Config: dacheckpoint.Config{ThreadID: "reviewer-thread"}, Messages: []damessage.Message{damessage.Human("new")}}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentMemoryConfinesSearchAndTransfers(t *testing.T) {
	stateDir := t.TempDir()
	identity := &agentIdentity{name: defaultAgentName}
	backend, err := openAgentMemory(stateDir, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Write(t.Context(), "/AGENTS.md", "needle in memory\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "secret.txt"), []byte("needle secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	globbed, err := backend.Glob(t.Context(), "*", "/")
	if err != nil || len(globbed.Matches) != 1 || globbed.Matches[0].Path != "/AGENTS.md" {
		t.Fatalf("glob = %#v, err = %v", globbed, err)
	}
	if _, err := backend.Glob(t.Context(), "*", "/../"); err == nil {
		t.Fatal("traversing glob base was accepted")
	}
	grep, err := backend.Grep(t.Context(), "needle", dabackend.GrepOptions{Path: "/"})
	if err != nil || len(grep.Matches) != 1 || grep.Matches[0].Path != "/AGENTS.md" || strings.Contains(grep.Matches[0].Text, "secret") {
		t.Fatalf("grep = %#v, err = %v", grep, err)
	}
	if _, err := backend.Grep(t.Context(), "needle", dabackend.GrepOptions{Path: "/../"}); err == nil {
		t.Fatal("traversing grep path was accepted")
	}

	uploads := backend.Upload(t.Context(), []dabackend.Upload{
		{Path: "/AGENTS.md", Content: []byte("uploaded")},
		{Path: "/../secret.txt", Content: []byte("stolen")},
	})
	if len(uploads) != 2 || uploads[0].Error != "" || uploads[1].Error == "" {
		t.Fatalf("uploads = %#v", uploads)
	}
	downloads := backend.Download(t.Context(), []string{"/AGENTS.md", "/../secret.txt"})
	if len(downloads) != 2 || string(downloads[0].Content) != "uploaded" || downloads[0].Error != "" || downloads[1].Error == "" || len(downloads[1].Content) != 0 {
		t.Fatalf("downloads = %#v", downloads)
	}
	if got := readFileString(t, filepath.Join(stateDir, "secret.txt")); got != "needle secret" {
		t.Fatalf("sibling file changed to %q", got)
	}
}

func TestAgentMemoryRejectsFileAndDirectorySymlinks(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		stateDir := t.TempDir()
		identity := &agentIdentity{name: defaultAgentName}
		backend, err := openAgentMemory(stateDir, identity)
		if err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.md")
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		memoryPath := filepath.Join(stateDir, defaultAgentName, agentInstructionsFilename)
		if err := os.Remove(memoryPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, memoryPath); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Write(t.Context(), "/AGENTS.md", "escape"); err == nil {
			t.Fatal("write followed a memory-file symlink")
		}
		if results := backend.Upload(t.Context(), []dabackend.Upload{{Path: "/AGENTS.md", Content: []byte("escape")}}); len(results) != 1 || results[0].Error == "" {
			t.Fatalf("symlink upload = %#v", results)
		}
		if got := readFileString(t, outside); got != "outside" {
			t.Fatalf("outside file changed to %q", got)
		}
	})

	t.Run("directory swap", func(t *testing.T) {
		stateDir := t.TempDir()
		identity := &agentIdentity{name: defaultAgentName}
		backend, err := openAgentMemory(stateDir, identity)
		if err != nil {
			t.Fatal(err)
		}
		outsideDir := t.TempDir()
		outside := filepath.Join(outsideDir, agentInstructionsFilename)
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		agentDir := filepath.Join(stateDir, defaultAgentName)
		if err := os.Remove(filepath.Join(agentDir, agentInstructionsFilename)); err != nil {
			t.Fatal(err)
		}
		for _, child := range []string{agentSkillsDirectory, agentSessionsDirectory} {
			if err := os.Remove(filepath.Join(agentDir, child)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(agentDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideDir, agentDir); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Write(t.Context(), "/AGENTS.md", "escape"); err == nil {
			t.Fatal("write followed a swapped agent-directory symlink")
		}
		if got := readFileString(t, outside); got != "outside" {
			t.Fatalf("outside file changed to %q", got)
		}
	})
}

func TestAgentMemoryPromptAndManagedGuardReachSubagentWithoutDuplication(t *testing.T) {
	stateDir := t.TempDir()
	reviewerDir := filepath.Join(stateDir, "reviewer")
	if err := os.MkdirAll(reviewerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	managed := "Personal preference.\n" + dago.ManagedMemoryBlockStart + "\n- name: Ada\n" + dago.ManagedMemoryBlockEnd + "\nOld note.\n"
	if err := os.WriteFile(filepath.Join(reviewerDir, agentInstructionsFilename), []byte(managed), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := &agentIdentity{name: "reviewer"}
	memoryBackend, err := openAgentMemory(stateDir, identity)
	if err != nil {
		t.Fatal(err)
	}
	workspace := dabackend.NewMemory(nil)
	backend := dabackend.NewComposite(workspace, map[string]dabackend.Backend{agentMemoryMount: memoryBackend})
	memory := configureAgentMemory(dago.Memory{
		Sources:  []string{"/AGENTS.md"},
		Contents: map[string]string{"/AGENTS.md": "Project root instruction."},
	}, false)

	checkPrompt := func(requireIdentity bool) func(damodel.Request) error {
		return func(request damodel.Request) error {
			prompt := modelSystemText(request)
			if prompt == "" {
				return errors.New("memory prompt is missing")
			}
			if strings.Count(prompt, "Personal preference.") != 1 || strings.Count(prompt, "Project root instruction.") != 1 {
				return fmt.Errorf("agent or project guidance was duplicated: %q", prompt)
			}
			if strings.Index(prompt, "Personal preference.") > strings.Index(prompt, "Project root instruction.") {
				return errors.New("agent memory was not loaded before project guidance")
			}
			if !strings.Contains(prompt, "More deeply scoped workspace files take precedence") {
				return errors.New("workspace precedence is missing")
			}
			if requireIdentity && !strings.Contains(prompt, "reviewer") {
				return errors.New("selected identity is missing")
			}
			return nil
		}
	}
	childModel := modeltest.New(damodel.Profile{ToolCalling: true, SupportsSeparateSystemMessage: true},
		modeltest.Step{Check: checkPrompt(false), Response: damodel.Response{Message: damessage.Message{
			Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
				ID: "edit-memory", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/agent-memory/AGENTS.md","content":"Personal preference.\n<!-- deepagents:onboarding-name:start -->\n- name: Mallory\n<!-- deepagents:onboarding-name:end -->\nNew note.\n"}`),
			}},
		}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Role != damessage.RoleTool || last.ToolStatus != damessage.ToolStatusError {
				return errors.New("managed memory edit was not rejected for subagent")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("child done")}},
	)
	parentModel := modeltest.New(damodel.Profile{ToolCalling: true, SupportsSeparateSystemMessage: true},
		modeltest.Step{Check: checkPrompt(true), Response: damodel.Response{Message: damessage.Message{
			Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
				ID: "delegate", Name: "task", Arguments: json.RawMessage(`{"description":"update durable memory","subagent_type":"writer"}`),
			}},
		}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled := dago.NewAgent(
		parentModel,
		dago.WithBackend(backend),
		dago.WithFilesystem(dago.Filesystem{}),
		dago.WithMemory(memory),
		dago.WithSystemPrompt("More deeply scoped workspace files take precedence over broader workspace files."),
		dago.WithMiddleware(agentIdentityMiddleware(identity, stateDir)),
		dago.WithStateFields(sessionStateFields()),
		dago.WithSubagents(dago.NewSubagent(
			"writer", "Updates memory", childModel,
			dago.WithSystemPrompt("More deeply scoped workspace files take precedence over broader workspace files."),
		)),
		dago.WithoutSummary(),
	)
	result, err := compiled.Invoke(t.Context(), dagent.Input{Messages: []damessage.Message{damessage.Human("delegate")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "done" {
		t.Fatalf("result = %#v", result.Messages)
	}
	after := readFileString(t, filepath.Join(reviewerDir, agentInstructionsFilename))
	if !strings.Contains(after, "name: Ada") || strings.Contains(after, "Mallory") || !strings.Contains(after, "New note.") {
		t.Fatalf("guarded memory = %q", after)
	}

	generalModel := modeltest.New(damodel.Profile{ToolCalling: true, SupportsSeparateSystemMessage: true},
		modeltest.Step{Check: checkPrompt(true), Response: damodel.Response{Message: damessage.Message{
			Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
				ID: "delegate-general", Name: "task", Arguments: json.RawMessage(`{"description":"inspect durable memory","subagent_type":"general-purpose"}`),
			}},
		}}},
		modeltest.Step{Check: checkPrompt(false), Response: damodel.Response{Message: damessage.Assistant("general child done")}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("general done")}},
	)
	general := dago.NewAgent(
		generalModel,
		dago.WithBackend(backend),
		dago.WithFilesystem(dago.Filesystem{}),
		dago.WithMemory(memory),
		dago.WithSystemPrompt("More deeply scoped workspace files take precedence over broader workspace files."),
		dago.WithMiddleware(agentIdentityMiddleware(identity, stateDir)),
		dago.WithStateFields(sessionStateFields()),
		dago.WithSubagents(dacodeGeneralSubagent(damessage.System("More deeply scoped workspace files take precedence over broader workspace files."))),
		dago.WithoutSummary(),
	)
	generalResult, err := general.Invoke(t.Context(), dagent.Input{Messages: []damessage.Message{damessage.Human("delegate generally")}})
	if err != nil {
		t.Fatal(err)
	}
	if generalResult.Messages[len(generalResult.Messages)-1].TextContent() != "general done" {
		t.Fatalf("general result = %#v", generalResult.Messages)
	}
}

func TestAgentMemoryGuardThroughCompositeBackend(t *testing.T) {
	stateDir := t.TempDir()
	identity := &agentIdentity{name: defaultAgentName}
	memoryBackend, err := openAgentMemory(stateDir, identity)
	if err != nil {
		t.Fatal(err)
	}
	workspace := dabackend.NewMemory(nil)
	backend := dabackend.NewComposite(workspace, map[string]dabackend.Backend{agentMemoryMount: memoryBackend})
	before := dago.ManagedMemoryBlockStart + "\nAda\n" + dago.ManagedMemoryBlockEnd + "\nold\n"
	if _, err := backend.Write(t.Context(), agentMemorySourcePath, before); err != nil {
		t.Fatal(err)
	}
	guard := dago.ManagedMemoryGuard(backend, agentMemorySourcePath)
	arguments, _ := json.Marshal(map[string]string{"file_path": agentMemorySourcePath})
	response, err := guard.WrapToolCall(t.Context(), dagent.ToolCallRequest{
		Call: damessage.ToolCall{ID: "write", Name: "write_file", Arguments: arguments},
	}, func(ctx context.Context, _ dagent.ToolCallRequest) (dagent.ToolCallResponse, error) {
		_, writeErr := backend.Write(ctx, agentMemorySourcePath, strings.ReplaceAll(strings.ReplaceAll(before, "Ada", "Mallory"), "old", "new"))
		return dagent.ToolCallResponse{Result: datool.TextResult("ok")}, writeErr
	})
	if err != nil || response.Result.Status != damessage.ToolStatusError {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	after := downloadCompositeMemory(t, backend)
	if strings.Contains(after, "Mallory") || !strings.Contains(after, "Ada") || !strings.Contains(after, "new") {
		t.Fatalf("guarded composite memory = %q", after)
	}
}

func downloadAgentMemory(t *testing.T, ctx context.Context, backend dabackend.Backend) string {
	t.Helper()
	results := backend.Download(ctx, []string{"/AGENTS.md"})
	if len(results) != 1 || results[0].Error != "" {
		t.Fatalf("download = %#v", results)
	}
	return string(results[0].Content)
}

func downloadCompositeMemory(t *testing.T, backend dabackend.Backend) string {
	t.Helper()
	results := backend.Download(t.Context(), []string{agentMemorySourcePath})
	if len(results) != 1 || results[0].Error != "" {
		t.Fatalf("download = %#v", results)
	}
	return string(results[0].Content)
}

func readFileString(t *testing.T, name string) string {
	t.Helper()
	contents, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func modelSystemText(request damodel.Request) string {
	var sections []string
	if request.SystemMessage != nil {
		sections = append(sections, request.SystemMessage.TextContent())
	}
	for _, message := range request.Messages {
		if message.Role == damessage.RoleSystem {
			sections = append(sections, message.TextContent())
		}
	}
	return strings.Join(sections, "\n")
}

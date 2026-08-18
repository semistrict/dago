package dacode

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
)

func TestParseCLIStartupAutomationFlags(t *testing.T) {
	options, err := parseCLI([]string{
		"-m", "review this patch", "-s", "code-review", "--startup-cmd", "git status",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.message != "review this patch" || options.initialSkill != "code-review" || options.startupCommand != "git status" {
		t.Fatalf("options = %#v", options)
	}
	defaults, err := parseCLI(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.initialSkill != "" || defaults.startupCommand != "" {
		t.Fatalf("defaults = %#v", defaults)
	}
	if defaults.recursionLimit != defaultRecursionLimit {
		t.Fatalf("default recursion limit = %d, want %d", defaults.recursionLimit, defaultRecursionLimit)
	}
	overridden, err := parseCLI([]string{"--recursion-limit", "3000"}, io.Discard)
	if err != nil || overridden.recursionLimit != 3000 {
		t.Fatalf("recursion override = %d, %v", overridden.recursionLimit, err)
	}
	for _, value := range []string{"0", "-1", "invalid"} {
		if _, err := parseCLI([]string{"--recursion-limit", value}, io.Discard); err == nil {
			t.Errorf("recursion limit %q was accepted", value)
		}
	}
	for _, arguments := range [][]string{{"--acp", "--skill", "review"}, {"acp", "--startup-cmd", "true"}} {
		if _, err := parseCLI(arguments, io.Discard); err == nil || !strings.Contains(err.Error(), "--acp cannot be used") {
			t.Errorf("parseCLI(%#v) error = %v", arguments, err)
		}
	}
	var usage bytes.Buffer
	printUsage(&usage)
	for _, option := range []string{"-m, --message", "-s, --skill", "--startup-cmd", "--recursion-limit"} {
		if !strings.Contains(usage.String(), option) {
			t.Errorf("usage missing %q:\n%s", option, usage.String())
		}
	}
}

func TestMemoryAutoSaveUsesUsefulDefaultAndUpstreamEnvironment(t *testing.T) {
	t.Setenv("DEEPAGENTS_CODE_MEMORY_AUTO_SAVE", "false")
	readOnly, err := parseCLI(nil, io.Discard)
	if err != nil || readOnly.memoryAutoSave {
		t.Fatalf("disabled environment = %#v, err = %v", readOnly, err)
	}
	enabled, err := parseCLI([]string{"--memory-auto-save=true"}, io.Discard)
	if err != nil || !enabled.memoryAutoSave {
		t.Fatalf("explicit enable = %#v, err = %v", enabled, err)
	}
	t.Setenv("DEEPAGENTS_CODE_MEMORY_AUTO_SAVE", "unexpected")
	useful, err := parseCLI(nil, io.Discard)
	if err != nil || !useful.memoryAutoSave {
		t.Fatalf("unknown environment default = %#v, err = %v", useful, err)
	}
	var usage bytes.Buffer
	printUsage(&usage)
	if !strings.Contains(usage.String(), "--memory-auto-save") {
		t.Fatalf("usage does not document memory auto-save:\n%s", usage.String())
	}
}

func TestComposeInitialSkillPromptUsesHighestPriorityAndPreservesRequest(t *testing.T) {
	root := t.TempDir()
	writeTestSkill(t, root, ".agents/skills", "review", "lower-priority instructions")
	writeTestSkill(t, root, ".deepagents/skills", "review", "highest-priority instructions")

	prompt, err := composeInitialSkillPrompt(root, "  REVIEW  ", "  keep leading whitespace")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"I'm invoking the skill `review`.",
		"name: review",
		"highest-priority instructions",
		"**User request:**   keep leading whitespace",
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt missing %q:\n%s", expected, prompt)
		}
	}
	if strings.Contains(prompt, "lower-priority instructions") {
		t.Fatalf("lower-priority skill remained in prompt:\n%s", prompt)
	}
	if _, err := composeInitialSkillPrompt(root, "missing", "task"); err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("missing skill error = %v", err)
	}
}

func TestComposeInitialSkillPromptConfinesSymlinksToWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated permission on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	writeTestSkill(t, outside, "", "review", "outside instructions")
	source := filepath.Join(root, ".deepagents", "skills")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "review"), filepath.Join(source, "review")); err != nil {
		t.Fatal(err)
	}
	if prompt, err := composeInitialSkillPrompt(root, "review", "task"); err == nil || strings.Contains(prompt, "outside instructions") {
		t.Fatalf("symlink escape prompt = %q, error = %v", prompt, err)
	}
}

func TestRunStartupCommandShowsLiteralOutputAndContinuesAfterFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test commands use POSIX shell syntax")
	}
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	command := "printf 'safe\\n\\033]52;c;hostile\\a\\n'; printf 'warning-output\\n' >&2; exit 7"
	if err := runStartupCommand(t.Context(), command, root, false, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	output := stdout.String()
	for _, expected := range []string{
		"Running startup command:",
		"safe",
		"<U+001B CONTROL>]52;c;hostile<U+0007 CONTROL>",
		"warning-output",
		"Warning: startup command exited with code 7; continuing anyway",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("output missing %q:\n%s", expected, output)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunStartupCommandCancellationStopsExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test command uses POSIX shell syntax")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	started := time.Now()
	err := runStartupCommand(ctx, "while :; do :; done", t.TempDir(), true, io.Discard, io.Discard)
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("error = %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("cancellation took %s", time.Since(started))
	}
}

func TestStartupTranscriptIsMountedOnceAndAfterResumedHistory(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.startupTranscript = "Running startup command: prepare\nprepared"
	model.Init()
	model.Init()
	if len(model.items) != 1 || model.items[0].kind != itemNotice || model.items[0].text != "Running startup command: prepare\nprepared" {
		t.Fatalf("new-session items = %#v", model.items)
	}

	resumed := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	resumed.startupTranscript = "Running startup command: resume-prepare\nresume-ready"
	resumed.sessionPicker = &sessionPickerState{resuming: true}
	resumed.finishSessionLoad(sessionLoadedMsg{
		session: sessionInfo{ThreadID: "resumed-thread"},
		messages: []damessage.Message{
			damessage.Human("saved task"), damessage.Assistant("saved response"),
		},
	})
	if len(resumed.items) != 3 || resumed.items[2].kind != itemNotice || resumed.items[2].text != "Running startup command: resume-prepare\nresume-ready" {
		t.Fatalf("resumed items = %#v", resumed.items)
	}
}

func TestRunHeadlessStartupCommandPrecedesSkillAndKeepsJSONClean(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test command uses POSIX shell syntax")
	}
	var requestBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var err error
		requestBody, err = io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"startup-1\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer server.Close()

	root := t.TempDir()
	stateDirectory := t.TempDir()
	command := "mkdir -p .deepagents/skills/review; " +
		"printf '%s\\n' '---' 'name: review' 'description: Review generated work' '---' 'First-turn generated instructions' > .deepagents/skills/review/SKILL.md; " +
		"printf 'startup-local\\n'"
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	var stdout, stderr bytes.Buffer
	err := Run(t.Context(), []string{
		"--startup-cmd", command,
		"--skill", "review",
		"--non-interactive", "inspect it",
		"--quiet", "--json",
		"--model", defaultModel,
		"--cwd", root,
		"--state-dir", stateDirectory,
	}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run: %v; stderr: %s", err, stderr.String())
	}
	if !bytes.Contains(requestBody, []byte("First-turn generated instructions")) || !bytes.Contains(requestBody, []byte("**User request:** inspect it")) {
		t.Fatalf("request did not contain generated skill envelope: %s", requestBody)
	}
	if bytes.Contains(requestBody, []byte("startup-local")) {
		t.Fatalf("startup command output leaked into model context: %s", requestBody)
	}
	if !strings.Contains(stderr.String(), "startup-local") {
		t.Fatalf("startup output was not shown on clean stderr: %q", stderr.String())
	}
	var result struct {
		Version  int    `json:"version"`
		Response string `json:"response"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode stdout %q: %v", stdout.String(), err)
	}
	if result.Version != 1 || result.Response != "done" {
		t.Fatalf("result = %#v", result)
	}
}

func writeTestSkill(t *testing.T, root, source, name, body string) {
	t.Helper()
	directory := filepath.Join(root, filepath.FromSlash(source), name)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: Test skill\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

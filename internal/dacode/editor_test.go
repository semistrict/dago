package dacode

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestResolveEditorCommandUsesVisualThenEditorAndFallback(t *testing.T) {
	t.Setenv("VISUAL", `"/Applications/My Editor/bin/code" --profile "work profile"`)
	t.Setenv("EDITOR", "ignored --flag")
	arguments, err := resolveEditorCommand()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/Applications/My Editor/bin/code", "--profile", "work profile"}
	if !slices.Equal(arguments, want) {
		t.Fatalf("arguments = %#v, want %#v", arguments, want)
	}

	t.Setenv("VISUAL", "")
	arguments, err = resolveEditorCommand()
	if err != nil || !slices.Equal(arguments, []string{"ignored", "--flag"}) {
		t.Fatalf("EDITOR arguments = %#v, error = %v", arguments, err)
	}

	t.Setenv("EDITOR", "")
	arguments, err = resolveEditorCommand()
	fallback := "vi"
	if runtime.GOOS == "windows" {
		fallback = "notepad"
	}
	if err != nil || !slices.Equal(arguments, []string{fallback}) {
		t.Fatalf("fallback arguments = %#v, error = %v", arguments, err)
	}
}

func TestResolveEditorCommandRejectsInvalidConfiguration(t *testing.T) {
	t.Setenv("EDITOR", "")
	for _, value := range []string{"   \t", `'unterminated`, `editor \`} {
		t.Setenv("VISUAL", value)
		if _, err := resolveEditorCommand(); err == nil {
			t.Errorf("resolveEditorCommand accepted %q", value)
		}
	}
}

func TestPrepareEditorCommandAddsCompatibilityFlagsWithoutMutatingInput(t *testing.T) {
	tests := []struct {
		input []string
		want  []string
	}{
		{[]string{"/usr/local/bin/code", "--reuse-window"}, []string{"/usr/local/bin/code", "--wait", "--reuse-window", "draft.md"}},
		{[]string{"cursor", "--wait"}, []string{"cursor", "--wait", "draft.md"}},
		{[]string{"subl"}, []string{"subl", "-w", "draft.md"}},
		{[]string{"nvim", "--clean"}, []string{"nvim", "--clean", "-i", "NONE", "draft.md"}},
		{[]string{"vim", "-i", "custom.viminfo"}, []string{"vim", "-i", "custom.viminfo", "draft.md"}},
		{[]string{"emacsclient", "-c"}, []string{"emacsclient", "-c", "draft.md"}},
	}
	for _, test := range tests {
		original := slices.Clone(test.input)
		got := prepareEditorCommand(test.input, "draft.md")
		if !slices.Equal(got, test.want) {
			t.Errorf("prepareEditorCommand(%#v) = %#v, want %#v", test.input, got, test.want)
		}
		if !slices.Equal(test.input, original) {
			t.Errorf("prepareEditorCommand mutated input: %#v", test.input)
		}
	}
}

func TestEditorDisplayNameOnlyReturnsSafeShortExecutableName(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{"/usr/local/bin/code --wait", "code"},
		{`"/Applications/My Editor/cursor" --wait`, "cursor"},
		{"..", ""},
		{"editor;rm", ""},
		{"🔧", ""},
		{"an-editor-name-that-is-too-long", ""},
		{"'unterminated", ""},
	}
	for _, test := range tests {
		t.Setenv("VISUAL", test.value)
		if got := editorDisplayName(); got != test.want {
			t.Errorf("editorDisplayName(%q) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestEditorInvocationUsesPrivateMarkdownFileAndDirectArguments(t *testing.T) {
	t.Setenv("VISUAL", "code --reuse-window")
	invocation, err := prepareEditorInvocation(t.Context(), "draft text")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(invocation.directory) })
	if filepath.Ext(invocation.file) != ".md" || filepath.Dir(invocation.file) != invocation.directory {
		t.Fatalf("file = %q, directory = %q", invocation.file, invocation.directory)
	}
	info, err := os.Stat(invocation.directory)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o", info.Mode().Perm())
	}
	content, err := os.ReadFile(invocation.file)
	if err != nil || string(content) != "draft text" {
		t.Fatalf("content = %q, error = %v", content, err)
	}
	want := []string{"code", "--wait", "--reuse-window", invocation.file}
	if !slices.Equal(invocation.command.Args, want) {
		t.Fatalf("command arguments = %#v, want %#v", invocation.command.Args, want)
	}
}

func TestEditorFinishNormalizesNewlinesAndRemovesExactlyOneTrailingNewline(t *testing.T) {
	invocation := newTestEditorInvocation(t, "before")
	if err := os.WriteFile(invocation.file, []byte("first\r\nsecond\rthird\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	message := invocation.finish(nil)
	if message.err != nil || message.cancelled || message.text != "first\nsecond\nthird\n" {
		t.Fatalf("message = %#v", message)
	}
	if _, err := os.Stat(invocation.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary directory remains: %v", err)
	}
}

func TestEditorFinishCancelsBlankAndNonzeroExit(t *testing.T) {
	blank := newTestEditorInvocation(t, "before")
	if err := os.WriteFile(blank.file, []byte(" \r\n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if message := blank.finish(nil); !message.cancelled || message.err != nil {
		t.Fatalf("blank message = %#v", message)
	}

	exited := newTestEditorInvocation(t, "before")
	command := exec.Command("definitely-not-run")
	exitError := &exec.ExitError{ProcessState: command.ProcessState}
	if message := exited.finish(exitError); !message.cancelled || message.err != nil {
		t.Fatalf("exit message = %#v", message)
	}

	failed := newTestEditorInvocation(t, "before")
	launchError := errors.New("launch failed")
	if message := failed.finish(launchError); message.err != launchError || message.cancelled {
		t.Fatalf("launch message = %#v", message)
	}
}

func TestEditorFinishRejectsSymlinkReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated permission on Windows")
	}
	invocation := newTestEditorInvocation(t, "before")
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("escaped"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(invocation.file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, invocation.file); err != nil {
		t.Fatal(err)
	}
	if message := invocation.finish(nil); message.err == nil {
		t.Fatalf("symlink replacement was accepted: %#v", message)
	}
}

func TestExternalEditorRoutesSlashAndControlXToCurrentComposer(t *testing.T) {
	t.Setenv("VISUAL", "fixture-editor")
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	var drafts []string
	model.editDraft = func(draft string) (tea.Cmd, error) {
		drafts = append(drafts, draft)
		return func() tea.Msg { return editorFinishedMsg{text: "edited\ntext"} }, nil
	}
	model.composer.SetValue("current draft")
	command, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlX})
	if !handled || command == nil || !slices.Equal(drafts, []string{"current draft"}) {
		t.Fatalf("Ctrl+X handled = %v, command nil = %v, drafts = %#v", handled, command == nil, drafts)
	}
	model.Update(command())
	if model.composer.Value() != "edited\ntext" {
		t.Fatalf("composer = %q", model.composer.Value())
	}

	model.composer.SetValue("slash draft")
	command, handled = model.slashCommand("/editor")
	if !handled || command == nil || !slices.Equal(drafts, []string{"current draft", "slash draft"}) {
		t.Fatalf("/editor handled = %v, command nil = %v, drafts = %#v", handled, command == nil, drafts)
	}
	model.Update(editorFinishedMsg{cancelled: true})
	if model.composer.Value() != "slash draft" {
		t.Fatalf("cancel changed composer to %q", model.composer.Value())
	}
}

func TestExternalEditorHelpAndFooterNameConfiguredEditor(t *testing.T) {
	t.Setenv("VISUAL", "/usr/local/bin/cursor --wait")
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.width = 120
	if _, handled := model.slashCommand("/help"); !handled {
		t.Fatal("/help was not handled")
	}
	if got := model.items[len(model.items)-1].text; !strings.Contains(got, "/editor") || !strings.Contains(got, "Ctrl+X open draft in cursor") {
		t.Fatalf("help = %q", got)
	}
	if got := model.renderStatus(); !strings.Contains(got, "ctrl+x cursor") {
		t.Fatalf("status = %q", got)
	}
}

func newTestEditorInvocation(t *testing.T, content string) *editorInvocation {
	t.Helper()
	directory := t.TempDir()
	file := filepath.Join(directory, "prompt.md")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return &editorInvocation{directory: directory, file: file}
}

package dacode

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
)

func TestInputModePrefixesAndCompletion(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, t.TempDir(), "model", "thread", false, false, "")
	if _, handled := model.handleComposerKey(testTextKey(string([]rune("!")))); !handled || model.inputMode != inputShell || model.composer.Prompt != "$ " {
		t.Fatalf("shell prefix: handled=%v mode=%v prompt=%q", handled, model.inputMode, model.composer.Prompt)
	}
	if _, handled := model.handleComposerKey(testTextKey(string([]rune("!")))); !handled || model.inputMode != inputIncognitoShell {
		t.Fatalf("incognito prefix: handled=%v mode=%v", handled, model.inputMode)
	}
	if _, handled := model.handleComposerKey(tea.KeyPressMsg{Code: tea.KeyBackspace}); !handled || model.inputMode != inputNormal {
		t.Fatalf("prefix backspace: handled=%v mode=%v", handled, model.inputMode)
	}
	model.setInputMode(inputCommand)
	model.composer.SetValue("hep")
	model.updateInputCompletion()
	if len(model.inputCompletion.items) == 0 || model.inputCompletion.items[0] != "/help" {
		t.Fatalf("slash completions = %#v", model.inputCompletion.items)
	}
}

func TestFileCompletionAndDroppedMediaPaste(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello world.go"), []byte("package hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "screen.png"), []byte("\x89PNG\r\n\x1a\nfixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	model := newTUIModel(t.Context(), &fakeRunner{}, root, "model", "thread", false, false, "")
	model.inputFiles = workspaceFiles(root, 100)
	model.composer.SetValue("inspect @hello")
	model.updateInputCompletion()
	if len(model.inputCompletion.items) != 1 || model.inputCompletion.items[0] != "hello world.go" {
		t.Fatalf("file completions = %#v", model.inputCompletion.items)
	}
	model.applyInputCompletion()
	if got := model.composer.Value(); got != "inspect @hello world.go " {
		t.Fatalf("completed draft = %q", got)
	}
	model.composer.Reset()
	model.insertPaste("'" + filepath.Join(root, "screen.png") + "'")
	if got := model.composer.Value(); got != "[image 1] " {
		t.Fatalf("media paste = %q", got)
	}
	if block := model.inputMedia["[image 1]"]; block.Type != damessage.BlockImage || block.MIMEType != "image/png" || len(block.Data) == 0 {
		t.Fatalf("media block = %#v", block)
	}
}

func TestLargeAndLegacyPasteCollapseAndExpansion(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, t.TempDir(), "model", "thread", false, false, "")
	pasted := strings.Repeat("large paste ", 100)
	model.insertPaste(pasted)
	if !strings.Contains(model.composer.Value(), "[Pasted text #1]") {
		t.Fatalf("collapsed draft = %q", model.composer.Value())
	}
	if got := model.expandedComposerValue(); got != pasted {
		t.Fatalf("expanded paste differs: %d != %d", len(got), len(pasted))
	}

	legacy := newTUIModel(t.Context(), &fakeRunner{}, t.TempDir(), "model", "thread", false, false, "")
	value := "one\ntwo\nthree\nfour"
	if _, handled := legacy.handleComposerKey(testTextKey(string([]rune(value)))); !handled {
		t.Fatal("legacy run was not treated as an atomic paste")
	}
	if !strings.Contains(legacy.composer.Value(), "[Pasted text #1 +3 lines]") || legacy.expandedComposerValue() != value {
		t.Fatalf("legacy paste draft=%q expanded=%q", legacy.composer.Value(), legacy.expandedComposerValue())
	}
}

func TestPastePlaceholderDeletionIsAtomic(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, t.TempDir(), "model", "thread", false, false, "")
	model.insertPaste(strings.Repeat("large paste ", 100))
	placeholder := model.composer.Value()
	model.composer.CursorEnd()
	if !model.deletePastePlaceholder(true) || model.composer.Value() != "" {
		t.Fatalf("backspace left %q", model.composer.Value())
	}
	if _, exists := model.pasteBindings[placeholder]; exists {
		t.Fatal("deleted placeholder retained its hidden paste binding")
	}
}

func TestNativeTerminalSuppressesFirstClickAfterRefocus(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, t.TempDir(), "model", "thread", false, false, "")
	model.resize(100, 30)
	model.composer.SetValue("keep cursor here")
	_, _ = model.Update(tea.BlurMsg{})
	_, _ = model.Update(tea.FocusMsg{})
	updated, _ := model.Update(tea.MouseClickMsg{X: 3, Y: model.viewport.Height() + model.composer.Height() + 1, Button: tea.MouseLeft})
	if updated.composer.Value() != "keep cursor here" {
		t.Fatal("first click after refocus changed the draft")
	}
	if !updated.refocusedAt.IsZero() {
		t.Fatal("refocus suppression was not single-use")
	}
}

func TestPersistentInputHistoryFiltersAndRestores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "history.jsonl")
	history, err := loadInputHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"first request", "first request", "/help", "/skill:review", "second request"} {
		if err := history.add(value); err != nil {
			t.Fatal(err)
		}
	}
	restored, err := loadInputHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first request", "/skill:review", "second request"}
	if strings.Join(restored.entries, "|") != strings.Join(want, "|") {
		t.Fatalf("entries = %#v", restored.entries)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("history permissions = %o", info.Mode().Perm())
		}
	}
	if got, ok := restored.previous("request"); !ok || got != "second request" {
		t.Fatalf("previous = %q, %v", got, ok)
	}
	if got, ok := restored.next(); !ok || got != "request" {
		t.Fatalf("restored draft = %q, %v", got, ok)
	}
}

func TestBusyInputQueuesAndDrainsInOrder(t *testing.T) {
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}, &fakeEventStream{}}}
	model := newTUIModel(t.Context(), runner, t.TempDir(), "model", "thread", false, false, "")
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.running = true
	model.composer.SetValue("queued request")
	if command := model.submitComposer(); command != nil || len(model.inputQueue) != 1 {
		t.Fatalf("command=%v queue=%#v", command, model.inputQueue)
	}
	queuedTranscript := ansi.Strip(model.renderTranscript())
	if !strings.Contains(queuedTranscript, "> queued request") || !strings.Contains(queuedTranscript, "Queued input #1.") {
		t.Fatalf("queued transcript = %q", queuedTranscript)
	}
	model.running = false
	command := model.drainInputQueue()
	if command == nil || len(runner.inputs) != 1 || runner.inputs[0].Messages[0].TextContent() != "queued request" {
		t.Fatalf("command=%v inputs=%#v", command, runner.inputs)
	}
	dispatchedTranscript := ansi.Strip(model.renderTranscript())
	if strings.Count(dispatchedTranscript, "> queued request") != 1 {
		t.Fatalf("queued prompt was rendered again when dispatched: %q", dispatchedTranscript)
	}
}

func TestShellCommandsSeparateSharedAndIncognitoContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture uses printf")
	}
	shared := executeShellCommand(t.Context(), t.TempDir(), "printf shared", false)().(shellDoneMsg)
	private := executeShellCommand(t.Context(), t.TempDir(), "printf private", true)().(shellDoneMsg)
	if shared.err != nil || shared.output != "shared" || shared.incognito {
		t.Fatalf("shared = %#v", shared)
	}
	if private.err != nil || private.output != "private" || !private.incognito {
		t.Fatalf("private = %#v", private)
	}
	model := newTUIModel(t.Context(), &fakeRunner{}, t.TempDir(), "model", "thread", false, false, "")
	_, _ = model.finishShell(shared)
	_, _ = model.finishShell(private)
	if len(model.shellContext) != 1 || strings.Contains(strings.Join(model.shellContext, "\n"), "private") {
		t.Fatalf("shell context = %#v", model.shellContext)
	}
}

func TestSubmitComposerExpandsCollapsedPasteBeforeAgentInput(t *testing.T) {
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{result: dagent.Result{}}}}
	model := newTUIModel(t.Context(), runner, t.TempDir(), "model", "thread", false, false, "")
	pasted := strings.Repeat("payload", 200)
	model.insertPaste(pasted)
	if command := model.submitComposer(); command == nil {
		t.Fatal("submit did not start stream")
	}
	if got := runner.inputs[0].Messages[0].TextContent(); got != pasted {
		t.Fatalf("agent input length = %d, want %d", len(got), len(pasted))
	}
}

func TestSubmitComposerSendsDroppedMediaAsStructuredContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "screen.png")
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nfixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{result: dagent.Result{}}}}
	model := newTUIModel(t.Context(), runner, root, "model", "thread", false, false, "")
	model.composer.SetValue("describe ")
	model.insertPaste(path)
	if command := model.submitComposer(); command == nil {
		t.Fatal("media submit did not start stream")
	}
	content := runner.inputs[0].Messages[0].Content
	if len(content) != 2 || content[0].Type != damessage.BlockText || content[0].Text != "describe" {
		t.Fatalf("content = %#v", content)
	}
	if content[1].Type != damessage.BlockImage || content[1].MIMEType != "image/png" || string(content[1].Data[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("image content = %#v", content[1])
	}
	if strings.Contains(content[0].Text, "[image 1]") {
		t.Fatal("display placeholder leaked into model text")
	}
}

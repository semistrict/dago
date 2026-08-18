package dacode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestThemeBackgroundSequenceSurvivesOtherSequenceFlushAndNarrowModal(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.resize(20, 10)
	if !model.setTheme("langchain-light") || model.themeSequence != terminalBackgroundSequence("#F5F5F7") {
		t.Fatalf("light theme sequence = %q", model.themeSequence)
	}
	command := model.stageTerminalSequences(osc52ClipboardSequence("copy"), "")
	message := command()
	model.Update(message)
	if model.themeSequence != terminalBackgroundSequence("#F5F5F7") {
		t.Fatalf("clipboard flush cleared background intent: %q", model.themeSequence)
	}
	if !model.setTheme("ansi-dark") || model.themeSequence != terminalBackgroundResetSequence() {
		t.Fatalf("ANSI reset sequence = %q", model.themeSequence)
	}
	model.setTheme("langchain")
	model.themePicker = newThemePicker(model.themeRegistry, model.themeName, "")
	model.handleThemeKey(tea.KeyMsg{Type: tea.KeyDown})
	if model.themeSequence != terminalBackgroundSequence("#F5F5F7") {
		t.Fatalf("preview sequence = %q", model.themeSequence)
	}
	model.handleThemeKey(tea.KeyMsg{Type: tea.KeyEsc})
	if model.themeSequence != terminalBackgroundSequence("#11121D") {
		t.Fatalf("cancel sequence = %q", model.themeSequence)
	}
	model.themePicker = newThemePicker(model.themeRegistry, model.themeName, "")
	view := ansi.Strip(model.View())
	if width, height := widestLine(view), len(strings.Split(view, "\n")); width > 20 || height > 10 {
		t.Fatalf("narrow composed theme view = %dx%d\n%s", width, height, view)
	}
	if !strings.Contains(view, "LangChain") || !strings.Contains(view, "What would") {
		t.Fatalf("narrow composed theme view lost selection or composer:\n%s", view)
	}
}

func TestThemeModalFillsTerminalHeight(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.resize(80, 24)
	model.themePicker = newThemePicker(model.themeRegistry, model.themeName, "")
	view := ansi.Strip(model.View())
	if height := len(strings.Split(view, "\n")); height != model.height {
		t.Fatalf("theme modal height = %d, want %d\n%s", height, model.height, view)
	}
	lines := strings.Split(view, "\n")
	if !strings.Contains(lines[len(lines)-1], "Ready") {
		t.Fatalf("status is not on final physical row: %q", lines[len(lines)-1])
	}
}

func TestTranscriptReflowAnchorUsesBlockIdentityWithDuplicateWrappedLines(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	duplicate := strings.Repeat("duplicate wrapped transcript value ", 8)
	model.appendItem(transcriptItem{kind: itemNotice, text: duplicate})
	model.appendItem(transcriptItem{kind: itemNotice, text: duplicate})
	for index := 0; index < 20; index++ {
		model.appendItem(transcriptItem{kind: itemNotice, text: "tail row"})
	}
	model.resize(34, 12)
	var second transcriptBlockLayout
	found := false
	for _, block := range model.transcriptLayout {
		if block.id.kind == transcriptBlockItem && block.id.index == 1 {
			second, found = block, true
			break
		}
	}
	if !found || second.lines < 2 {
		t.Fatalf("second wrapped block = %#v", second)
	}
	model.chatScroll.userScrolled(second.start + 1)
	model.viewport.SetYOffset(model.chatScroll.Offset)
	model.resize(24, 12)
	anchor, ok := transcriptAnchorAt(model.transcriptLayout, model.chatScroll.Offset)
	if !ok || anchor.id.kind != transcriptBlockItem || anchor.id.index != 1 || anchor.line != 1 {
		t.Fatalf("reflow anchor = %#v, ok=%t", anchor, ok)
	}
}

func TestApprovalViewportFallbackReconcilesChatScroll(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	for index := 0; index < 40; index++ {
		model.appendItem(transcriptItem{kind: itemNotice, text: "approval history row"})
	}
	model.approval = newApprovalState(nil)
	model.approval.ready = true
	model.resize(60, 16)
	model.chatScroll.userScrolled(model.chatScroll.MaxOffset)
	model.viewport.SetYOffset(model.chatScroll.Offset)
	before := model.viewport.YOffset
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	if model.viewport.YOffset >= before {
		t.Fatalf("viewport did not scroll: before=%d after=%d", before, model.viewport.YOffset)
	}
	if model.chatScroll.Offset != model.viewport.YOffset || model.chatScroll.FollowBottom {
		t.Fatalf("chat scroll not reconciled: %#v viewport=%d", model.chatScroll, model.viewport.YOffset)
	}
}

func TestStartupTipDismissesForEveryAcceptedSubmissionPath(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*tuiModel)
	}{
		{name: "initial", run: func(model *tuiModel) { model.Update(initialPromptMsg("prompt")) }},
		{name: "goal", run: func(model *tuiModel) { model.Update(initialGoalMsg("goal")) }},
		{name: "shell", run: func(model *tuiModel) {
			model.setInputMode(inputShell)
			model.composer.SetValue("echo ok")
			model.submitComposer()
		}},
		{name: "command", run: func(model *tuiModel) {
			model.setInputMode(inputCommand)
			model.composer.SetValue("help")
			model.submitComposer()
		}},
		{name: "queued", run: func(model *tuiModel) { model.running = true; model.composer.SetValue("queued"); model.submitComposer() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
			model.startupTip = newStartupTipState(startupTipFresh, "editor", true, 0, true)
			test.run(model)
			if !model.startupTip.Dismissed || model.startupTip.Visible {
				t.Fatalf("tip survived %s submission: %#v", test.name, model.startupTip)
			}
		})
	}
}

func TestWelcomeTargetsAreInactiveUnderLifecycleAndOffscreen(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	for index := 0; index < 50; index++ {
		model.appendItem(transcriptItem{kind: itemNotice, text: "history"})
	}
	model.resize(80, 24)
	model.chatScroll.userScrolled(0)
	model.viewport.SetYOffset(0)
	model.View()
	if len(model.welcomeScreenHitTargets) == 0 {
		t.Fatal("welcome target missing")
	}
	target := model.welcomeScreenHitTargets[0]
	model.modelSelector = newModelSelector(nil, "", "")
	model.View()
	if len(model.welcomeScreenHitTargets) != 0 {
		t.Fatalf("model selector retained welcome targets: %#v", model.welcomeScreenHitTargets)
	}
	if _, handled := model.handleWelcomeMouse(tea.MouseMsg{X: target.X, Y: target.Y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}); handled {
		t.Fatal("model selector accepted stale welcome click")
	}
	model.modelSelector = nil
	model.View()
	target = model.welcomeScreenHitTargets[0]
	model.restarting = true
	if _, handled := model.handleWelcomeMouse(tea.MouseMsg{X: target.X, Y: target.Y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}); handled {
		t.Fatal("restart screen accepted stale welcome click")
	}
	model.restarting = false
	model.chatScroll.userScrolled(model.chatScroll.MaxOffset)
	model.viewport.SetYOffset(model.chatScroll.Offset)
	model.View()
	if len(model.welcomeScreenHitTargets) != 0 {
		t.Fatalf("offscreen targets = %#v", model.welcomeScreenHitTargets)
	}
}

func TestDisplaySettingsFlushPersistsLatestSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "display.json")
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.displaySettings = path
	model.showTimestamps, model.showScrollbar, model.showLineNumbers = true, true, false
	if err := model.flushDisplaySettings(); err != nil {
		t.Fatal(err)
	}
	settings, err := loadDisplaySettings(path)
	if err != nil || !settings.ShowMessageTimestamps || !settings.ShowScrollbar || settings.ShowDiffLineNumbers {
		t.Fatalf("settings=%#v err=%v", settings, err)
	}
}

func TestFinishTUIRunReportsSanitizedDisplayFlushFailureAndPreservesPrimaryError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.displaySettings = filepath.Join(blocker, "display.json")
	primary := errors.New("primary run failure")
	err := finishTUIRun(model, primary, io.Discard)
	if !errors.Is(err, primary) || !errors.Is(err, errDisplaySettingsFlush) {
		t.Fatalf("finish error = %v", err)
	}
	if strings.Contains(err.Error(), blocker) {
		t.Fatalf("finish error leaked path: %v", err)
	}
}

func TestFinishTUIRunPrintsExactDurableSessionResumeCommand(t *testing.T) {
	runner := &exitResumeRunner{
		fakeRunner: &fakeRunner{},
		session:    sessionInfo{ThreadID: "thread-123", CheckpointID: "checkpoint-123"},
	}
	model := newTUIModel(t.Context(), runner, "/work", "model", "thread-123", false, false, "")
	var output strings.Builder
	if err := finishTUIRun(model, nil, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "Resume this session:\ndacode resume thread-123\n" {
		t.Fatalf("exit output = %q", output.String())
	}

	runner.session = sessionInfo{}
	output.Reset()
	if err := finishTUIRun(model, nil, &output); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("empty session output = %q", output.String())
	}
}

type exitResumeRunner struct {
	*fakeRunner
	session sessionInfo
	err     error
}

func (runner *exitResumeRunner) SessionMetadata(context.Context, string) (sessionInfo, error) {
	return runner.session, runner.err
}

func widestLine(value string) int {
	widest := 0
	for _, line := range strings.Split(value, "\n") {
		widest = max(widest, ansi.StringWidth(line))
	}
	return widest
}

func TestWelcomeScreenTargetsFollowFinalBrowserLayoutAfterToast(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread-full", false, false, "")
	model.browserLinks = true
	model.configureWelcomeProject("project", "https://example.test/project")
	model.welcomeMCPServers = []mcpViewerServer{{Name: "server", Status: mcpViewerOK, Tools: []mcpViewerTool{{Name: "tool"}}}}
	model.resize(100, 24)
	assertTargets := func(want int) {
		t.Helper()
		view := ansi.Strip(model.View())
		if len(model.welcomeScreenHitTargets) != want {
			t.Fatalf("targets = %#v\n%s", model.welcomeScreenHitTargets, view)
		}
		lines := strings.Split(view, "\n")
		clip := max(len(lines)-model.height, 0)
		for _, target := range model.welcomeScreenHitTargets {
			row := target.Y + clip
			if row < 0 || row >= len(lines) || target.X < 0 || target.X+target.Width > len([]rune(lines[row])) {
				t.Fatalf("target outside view: %#v clip=%d lines=%d", target, clip, len(lines))
			}
		}
	}
	assertTargets(4)
	model.toasts.add("layout changed", toastInfo, maximumToastDuration, "", time.Now())
	model.relayout()
	assertTargets(4)
	for _, target := range model.welcomeScreenHitTargets {
		if target.Kind != welcomeHitThread {
			continue
		}
		if _, handled := model.handleWelcomeMouse(tea.MouseMsg{X: target.X, Y: target.Y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}); !handled {
			t.Fatalf("thread target not handled after toast: %#v", target)
		}
		return
	}
	t.Fatal("thread target missing after toast")
}

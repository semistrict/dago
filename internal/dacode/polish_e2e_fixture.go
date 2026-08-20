//go:build dacode_e2e_fixture

package dacode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dacost"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dahook"
	talonmcp "github.com/semistrict/dago/datalon/mcp"
)

type polishFixtureRunner struct {
	*notificationUpdateTraceFixtureRunner
}

func (*polishFixtureRunner) Rubric(context.Context, string) (dago.RubricSnapshot, error) {
	return dago.RubricSnapshot{Criteria: "Polish acceptance"}, nil
}

func (runner *polishFixtureRunner) Start(ctx context.Context, _ runInput) eventStream {
	stream := &notificationUpdateTraceFixtureStream{ctx: ctx}
	return &polishDelayedStream{notificationUpdateTraceFixtureStream: stream, delay: 1800 * time.Millisecond}
}

type polishDelayedStream struct {
	*notificationUpdateTraceFixtureStream
	delay time.Duration
}

func (stream *polishDelayedStream) Next(ctx context.Context) (dagent.Event, error) {
	select {
	case <-ctx.Done():
		return dagent.Event{}, ctx.Err()
	case <-time.After(stream.delay):
	}
	stream.delay = 0
	return stream.notificationUpdateTraceFixtureStream.Next(ctx)
}

type polishFixtureMCP struct{}

func (polishFixtureMCP) SnapshotMCP() ([]mcpViewerServer, bool, error) {
	return []mcpViewerServer{{
		Name: "fixture-tools", Transport: "stdio", Status: mcpViewerOK,
		Tools: []mcpViewerTool{{Name: "inspect", Description: "Inspect fixture state"}},
	}}, false, nil
}

func (polishFixtureMCP) LoginMCP(context.Context, string, talonmcp.Interaction) error { return nil }
func (polishFixtureMCP) ToggleMCPDisabled(context.Context, string) error              { return nil }
func (polishFixtureMCP) ReconnectMCP(context.Context) error                           { return nil }

// RunPolishFixture runs the deterministic offline terminal used by the polish browser suite.
func RunPolishFixture(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	options, err := parseCLI(arguments, stderr)
	if err != nil {
		return err
	}
	if options.serveXtermJS {
		return serveXtermJS(ctx, xtermJSServerOptions{
			Address: options.xtermJSAddress, Arguments: xtermSessionArguments(arguments), Stdout: stdout, Stderr: stderr,
		})
	}
	if err := os.MkdirAll(options.stateDir, 0o700); err != nil {
		return err
	}
	runner := &polishFixtureRunner{notificationUpdateTraceFixtureRunner: newNotificationUpdateTraceFixtureRunner()}
	if strings.TrimSpace(os.Getenv("DAGO_POLISH_MODE")) == "hook" {
		sink := newHookUISink()
		runner.dagoRunner.hookStatus = sink
		sink.Publish(dahook.Progress{OperationID: "fixture-hook", Event: dahook.UserPromptSubmit, Message: "Checking workspace", Active: true})
	}
	workspace := filepath.Join(options.stateDir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(workspace, ".git")); errors.Is(err, os.ErrNotExist) {
		if initErr := exec.CommandContext(ctx, "git", "init", "-q", "-b", "feature-polish", workspace).Run(); initErr != nil {
			return errors.New("initialize fixture repository")
		}
	}
	model := newTUIModel(ctx, runner, workspace, "fixture:polish-model", "fixture-thread-full-0123456789", false, false, options.message)
	model.initialGoal = strings.TrimSpace(options.goal)
	themePath := filepath.Join(options.stateDir, "theme.toml")
	themeDocument, _ := readThemeDocument(themePath)
	themeUI, _ := themeDocument["ui"].(map[string]any)
	_, persistedTheme := themeUI["theme"]
	model.configureTheme(themePath, "PolishTerminal")
	if !persistedTheme {
		model.setTheme(defaultThemeName)
	}
	model.contextWindow = 1_000
	model.totalTokens = 650
	model.costStats = sessionCostState{loaded: true, report: dacost.Report{
		Version: 1, InputTokens: 100, CacheReadTokens: 95, CacheWriteTokens: 10,
		CostUSD: 12.50, PricedRequestCount: 1,
	}}
	model.configureMCP(polishFixtureMCP{})
	model.applyWelcomeMCPSnapshot(welcomeMCPSnapshotMsg{servers: []mcpViewerServer{{
		Name: "fixture-tools", Status: mcpViewerOK, Tools: []mcpViewerTool{{Name: "inspect"}},
	}}})
	model.configureWelcomeProject("fixture project", "https://example.test/projects/fixture")
	_ = model.configureDisplaySettings(filepath.Join(options.stateDir, displaySettingsFilename))

	base := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 190; index++ {
		model.appendItem(transcriptItem{kind: itemNotice, text: "history row " + strconv.Itoa(index+1), timestamp: base.Add(time.Duration(index) * time.Minute)})
	}

	switch strings.TrimSpace(os.Getenv("DAGO_POLISH_MODE")) {
	case "failure":
		model.startupFailed = true
	case "queued":
		model.running = true
		model.status = "Fixture turn running"
	case "anchor":
		wrapped := strings.Repeat("duplicate browser anchor value ", 8)
		model.appendItem(transcriptItem{kind: itemSkill, name: "anchor", source: "block-one-marker", text: wrapped, expanded: true})
		for index := 0; index < 40; index++ {
			model.appendItem(transcriptItem{kind: itemNotice, text: "anchor spacer row " + strconv.Itoa(index+1)})
		}
		model.appendItem(transcriptItem{kind: itemSkill, name: "anchor", source: "block-two-marker", text: wrapped, expanded: true})
		// Leave enough material below the semantic anchor that a wider reflow
		// does not hit the physical bottom clamp and shift the captured row.
		for index := 0; index < 60; index++ {
			model.appendItem(transcriptItem{kind: itemNotice, text: "anchor tail row " + strconv.Itoa(index+1)})
		}
	case "resumed":
		model.suppressStartupTipAfterResume()
	case "fallback":
		model.restoreFallbackStartupTip()
	case "ascii":
		model.charset = charsetASCII
		model.glyphs = cloneUIGlyphs(asciiUIGlyphs)
		model.showScrollbar = true
	}

	program := tea.NewProgram(newProgramModel(model), tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout))
	defer fmt.Fprint(stdout, terminalBackgroundResetSequence())
	_, err = program.Run()
	_ = model.flushDisplaySettings()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

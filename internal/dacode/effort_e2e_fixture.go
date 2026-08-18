//go:build dacode_e2e_fixture

package dacode

import (
	"context"
	"errors"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/damodel"
)

type unsupportedEffortFixtureRunner struct{ *dagoRunner }

func (*unsupportedEffortFixtureRunner) Goal(context.Context, string) (*dagoal.Goal, error) {
	return nil, nil
}

// RunUnsupportedEffortFixture runs the browser-only unsupported-model fixture.
func RunUnsupportedEffortFixture(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	options, err := parseCLI(arguments, stderr)
	if err != nil {
		return err
	}
	if options.serveXtermJS {
		return serveXtermJS(ctx, xtermJSServerOptions{
			Address: options.xtermJSAddress, Arguments: xtermSessionArguments(arguments), Stdout: stdout, Stderr: stderr,
		})
	}
	profile := damodel.Profile{Provider: "fixture", Model: "plain-model", ContextWindow: 8_000}
	runner := &unsupportedEffortFixtureRunner{dagoRunner: &dagoRunner{
		profile: profile, effort: newReasoningEffortManager(profile, ""), agentState: &agentIdentity{name: defaultAgentName},
	}}
	model := newTUIModel(ctx, runner, "/fixture", profile.Model, "fixture-thread", false, false, "")
	program := tea.NewProgram(
		model,
		tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout),
	)
	_, err = program.Run()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

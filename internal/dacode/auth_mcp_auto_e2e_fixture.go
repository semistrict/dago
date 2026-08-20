//go:build dacode_e2e_fixture

package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/openai"
	"github.com/semistrict/dago/dastate"
	talonmcp "github.com/semistrict/dago/datalon/mcp"
)

type authMCPAutoFixtureRunner struct {
	*dagoRunner
	mu         sync.Mutex
	disabled   map[string]bool
	pending    bool
	lastPrompt string
	reconnects int
}

func newAuthMCPAutoFixtureRunner() *authMCPAutoFixtureRunner {
	profile := damodel.Profile{Provider: "fixture", Model: "main", ContextWindow: 32_000, StructuredOutput: true}
	return &authMCPAutoFixtureRunner{
		dagoRunner: &dagoRunner{
			profile: profile, effort: newReasoningEffortManager(profile, ""),
			agentState: &agentIdentity{name: defaultAgentName}, agentDefault: &agentIdentity{name: defaultAgentName},
		},
		disabled: make(map[string]bool),
	}
}

func (runner *authMCPAutoFixtureRunner) Start(_ context.Context, input runInput) eventStream {
	if len(input.Messages) > 0 {
		runner.mu.Lock()
		runner.lastPrompt = input.Messages[len(input.Messages)-1].TextContent()
		prompt := runner.lastPrompt
		runner.mu.Unlock()
		id := "fixture-allow"
		switch {
		case strings.Contains(prompt, "deny"):
			id = "fixture-deny"
		case strings.Contains(prompt, "fallback"):
			id = "fixture-fallback"
		}
		request := dagent.ApprovalRequest{Call: damessage.ToolCall{
			ID: id, Name: "execute", Arguments: json.RawMessage(`{"command":"fixture action"}`),
		}}
		interrupt := dagent.Interrupt{ID: "human_approval", Value: []dagent.ApprovalRequest{request}}
		return &authMCPAutoFixtureStream{
			events: []dagent.Event{{Mode: dagent.EventInterrupt, Interrupt: &interrupt}},
			result: dagent.Result{Interrupts: []dagent.Interrupt{interrupt}},
		}
	}
	runner.mu.Lock()
	prompt := runner.lastPrompt
	runner.mu.Unlock()
	status := damessage.ToolStatusSuccess
	if strings.Contains(prompt, "tool error") {
		status = damessage.ToolStatusError
	}
	result := damessage.Tool("fixture-allow", "fixture tool result")
	result.ToolStatus = status
	messages := []damessage.Message{result, damessage.Assistant("Fixture action completed.")}
	return &authMCPAutoFixtureStream{
		events: []dagent.Event{{Mode: dagent.EventUpdate, Update: dastate.Values{dagent.MessagesKey: messages}}},
		result: dagent.Result{Messages: messages},
	}
}

func (runner *authMCPAutoFixtureRunner) Review(_ context.Context, request approvalReviewRequest) (approvalReviewResult, error) {
	for _, pending := range request.Requests {
		if pending.Call.ID == "fixture-fallback" {
			return approvalReviewResult{}, errors.New("automatic review reached its human fallback threshold")
		}
	}
	result := approvalReviewResult{Assessments: make(map[string]approvalAssessment, len(request.Requests))}
	for _, pending := range request.Requests {
		assessment := approvalAssessment{ToolCallID: pending.Call.ID, RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "Fixture action is authorized."}
		if pending.Call.ID == "fixture-deny" {
			assessment.RiskLevel, assessment.UserAuthorization = "high", "low"
			assessment.Outcome, assessment.Rationale = "deny", "Fixture action is not authorized."
		}
		result.Assessments[pending.Call.ID] = assessment
	}
	return result, nil
}

func (*authMCPAutoFixtureRunner) ValidateAutoClassifier(ctx context.Context, spec string) (autoClassifierValidation, error) {
	if err := ctx.Err(); err != nil {
		return autoClassifierValidation{}, err
	}
	if strings.Contains(spec, "invalid") {
		return autoClassifierValidation{}, nil
	}
	if strings.Contains(spec, "missing") {
		return autoClassifierValidation{ModelAvailable: true}, nil
	}
	return autoClassifierValidation{ModelAvailable: true, CredentialsAvailable: true, StructuredOutput: autoClassifierStructuredSupported}, nil
}

func (runner *authMCPAutoFixtureRunner) SnapshotMCP() ([]mcpViewerServer, bool, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	servers := []mcpViewerServer{
		{Name: "oauth-error", Transport: "http", Status: mcpViewerUnauthenticated},
		{Name: "oauth-success", Transport: "http", Status: mcpViewerUnauthenticated},
		{Name: "slack-workspace", Transport: "http", Status: mcpViewerUnauthenticated},
		{Name: "broken", Transport: "stdio", Status: mcpViewerError, Detail: "bounded fixture connection failure"},
		{Name: "healthy", Transport: "stdio", Status: mcpViewerOK, Tools: []mcpViewerTool{{Name: "fixture_tool", Description: "Offline fixture tool"}}},
	}
	for index := range servers {
		if runner.disabled[servers[index].Name] {
			servers[index].Status = mcpViewerDisabled
			servers[index].Detail = "Disabled for this session."
			servers[index].PendingReconnect = runner.pending
		}
	}
	return servers, runner.pending, nil
}

func (*authMCPAutoFixtureRunner) LoginMCP(ctx context.Context, server string, interaction talonmcp.Interaction) error {
	switch server {
	case "oauth-success":
		_, err := interaction.Authorize(ctx, "https://example.test/oauth?client_id=public&state=opaque")
		return err
	case "oauth-error":
		if _, err := interaction.Authorize(ctx, "https://example.test/oauth?client_id=public&state=opaque"); err != nil {
			return err
		}
		return errors.New("fixture provider detail must not render")
	case "slack-workspace":
		selector, ok := interaction.(interface {
			SelectSlackWorkspace(context.Context) (string, error)
		})
		if !ok {
			return errors.New("workspace selection unavailable")
		}
		_, err := selector.SelectSlackWorkspace(ctx)
		return err
	default:
		return errors.New("login unavailable")
	}
}

func (runner *authMCPAutoFixtureRunner) ToggleMCPDisabled(ctx context.Context, server string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if server == "" {
		return errors.New("server required")
	}
	runner.disabled[server] = !runner.disabled[server]
	runner.pending = true
	return nil
}

func (runner *authMCPAutoFixtureRunner) ReconnectMCP(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runner.mu.Lock()
	runner.pending = false
	runner.reconnects++
	runner.mu.Unlock()
	return nil
}

func (*authMCPAutoFixtureRunner) Goal(context.Context, string) (*dagoal.Goal, error) { return nil, nil }
func (*authMCPAutoFixtureRunner) Rubric(context.Context, string) (dago.RubricSnapshot, error) {
	return dago.RubricSnapshot{}, nil
}
func (*authMCPAutoFixtureRunner) RubricSettings() (string, int)                { return "fixture:main", 3 }
func (*authMCPAutoFixtureRunner) SetRubricModel(context.Context, string) error { return nil }
func (*authMCPAutoFixtureRunner) SetRubricMaxIterations(int) error             { return nil }

type authMCPAutoFixtureStream struct {
	events []dagent.Event
	result dagent.Result
	index  int
}

func (stream *authMCPAutoFixtureStream) Next(context.Context) (dagent.Event, error) {
	if stream.index >= len(stream.events) {
		return dagent.Event{}, io.EOF
	}
	event := stream.events[stream.index]
	stream.index++
	return event, nil
}
func (stream *authMCPAutoFixtureStream) Result(context.Context) (dagent.Result, error) {
	return stream.result, nil
}
func (*authMCPAutoFixtureStream) Close() error { return nil }

type fixtureClassifierPreferences struct {
	mu    sync.Mutex
	value string
}

func (preferences *fixtureClassifierPreferences) Set(ctx context.Context, spec string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	preferences.mu.Lock()
	preferences.value = spec
	preferences.mu.Unlock()
	return nil
}

func (preferences *fixtureClassifierPreferences) Clear(ctx context.Context) error {
	return preferences.Set(ctx, "")
}

func fixtureSubscriptionLogin(mode string) authSubscriptionLogin {
	return func(ctx context.Context, openURL func(string) error, options openai.OAuthOptions) (*openai.OAuthSession, error) {
		if err := openURL("https://auth.openai.com/oauth/authorize?response_type=code&client_id=fixture&state=opaque"); err != nil {
			return nil, err
		}
		switch mode {
		case "error":
			return nil, errors.New("fixture provider secret must not render")
		case "cancel":
			<-ctx.Done()
			time.Sleep(20 * time.Millisecond)
			return nil, ctx.Err()
		default:
			if err := os.MkdirAll(filepath.Dir(options.StorePath), 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(options.StorePath, []byte("{\"fixture\":true}\n"), 0o600); err != nil {
				return nil, err
			}
			return nil, nil
		}
	}
}

// RunAuthMCPAutoFixture runs the browser-only deterministic management fixture.
func RunAuthMCPAutoFixture(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	options, err := parseCLI(arguments, stderr)
	if err != nil {
		return err
	}
	if options.serveXtermJS {
		return serveXtermJS(ctx, xtermJSServerOptions{
			Address: options.xtermJSAddress, Arguments: xtermSessionArguments(arguments), Stdout: stdout, Stderr: stderr,
		})
	}
	runner := newAuthMCPAutoFixtureRunner()
	model := newTUIModel(ctx, runner, "/fixture", options.model, "fixture-thread", false, true, "")
	manager := newAuthManager(
		dacredential.NewStore(filepath.Join(options.stateDir, "fixture-auth.json"), time.Now, dacredential.Options{}),
		func(string) (string, bool) { return "", false },
	)
	mode := os.Getenv("DACODE_FIXTURE_AUTH_MODE")
	model.authManager = newAuthTUIController(manager, filepath.Join(options.stateDir, "fixture-oauth.json"), fixtureSubscriptionLogin(mode), func(string) error { return nil })
	model.configureMCP(runner)
	model.configureAutoClassifier(options.model, options.approvalModel, &fixtureClassifierPreferences{})
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout))
	_, err = program.Run()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

var _ agentRunner = (*authMCPAutoFixtureRunner)(nil)
var _ autoClassifierRunner = (*authMCPAutoFixtureRunner)(nil)
var _ mcpRuntimeController = (*authMCPAutoFixtureRunner)(nil)

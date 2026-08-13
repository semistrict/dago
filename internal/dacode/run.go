package dacode

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	acp "github.com/coder/acp-go-sdk"
	"github.com/semistrict/dago/daacp"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/damessage"
)

type cliOptions struct {
	model          string
	message        string
	nonInteractive string
	resume         string
	resumePicker   bool
	workingDir     string
	stateDir       string
	yolo           bool
	autoApprove    bool
	approvalModel  string
	quiet          bool
	version        bool
	acp            bool
	serveXtermJS   bool
	xtermJSAddress string
}

type acpDagoRunner struct{ runner *dagoRunner }

func (runner *acpDagoRunner) Stream(ctx context.Context, input dagent.Input) *dagent.Stream {
	return runner.runner.agent.Stream(ctx, input)
}

func (runner *acpDagoRunner) Cancel(ctx context.Context, input dagent.Input) (dagent.Result, error) {
	return runner.runner.agent.Cancel(ctx, input)
}

func (runner *acpDagoRunner) LoadSession(ctx context.Context, id string) ([]damessage.Message, error) {
	return runner.runner.LoadSession(ctx, id)
}

type sessionClosers struct{ closers []io.Closer }

func (closers *sessionClosers) Close() error {
	var result error
	for _, closer := range closers.closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil && result == nil {
			result = err
		}
	}
	return result
}

func modelConfigOptions(model string) []acp.SessionConfigOption {
	category := acp.SessionConfigOptionCategoryModel
	values := acp.SessionConfigSelectOptionsUngrouped{{Name: model, Value: acp.SessionConfigValueId(model)}}
	return []acp.SessionConfigOption{{Select: &acp.SessionConfigOptionSelect{
		Id: "model", Name: "Model", Category: &category, CurrentValue: acp.SessionConfigValueId(model),
		Options: acp.SessionConfigSelectOptions{Ungrouped: &values},
	}}}
}

func Run(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	options, err := parseCLI(arguments, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if options.serveXtermJS {
		return serveXtermJS(ctx, xtermJSServerOptions{
			Address: options.xtermJSAddress, Arguments: xtermSessionArguments(arguments),
			Stdout: stdout, Stderr: stderr,
		})
	}
	if options.version {
		_, err := fmt.Fprintln(stdout, versionText())
		return err
	}
	if options.message != "" && options.nonInteractive != "" {
		return fmt.Errorf("--message and --non-interactive cannot be used together")
	}
	if options.resumePicker && options.nonInteractive != "" {
		return fmt.Errorf("the resume picker cannot be used with --non-interactive")
	}
	if options.resumePicker && options.message != "" {
		return fmt.Errorf("the resume picker cannot be used with --message")
	}
	workingDir, err := filepath.Abs(options.workingDir)
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	info, err := os.Stat(workingDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("working directory %q is not a directory", workingDir)
	}
	if options.stateDir == "" {
		configDir, configErr := os.UserConfigDir()
		if configErr != nil {
			return fmt.Errorf("resolve config directory: %w", configErr)
		}
		options.stateDir = filepath.Join(configDir, "dacode")
	}
	if options.model == "" {
		options.model = os.Getenv("OPENAI_MODEL")
		if options.model == "" {
			options.model = defaultModel
		}
	}
	if options.approvalModel == "" {
		options.approvalModel = os.Getenv("DACODE_APPROVAL_MODEL")
		if options.approvalModel == "" {
			options.approvalModel = defaultReviewModel
		}
	}
	authentication, err := resolveAuthentication(
		ctx, os.Getenv("OPENAI_API_KEY"), options.stateDir, stderr, defaultAuthenticationHooks(),
	)
	if err != nil {
		return err
	}

	nonInteractive := options.nonInteractive != ""
	if options.acp {
		factory := func(factoryContext context.Context, config daacp.SessionConfig) (daacp.Runner, io.Closer, error) {
			tools, mcpCloser, connectErr := connectMCPServers(factoryContext, config.MCPServers)
			if connectErr != nil {
				return nil, nil, connectErr
			}
			runner, runnerCloser, runnerErr := newACPSessionRunner(runnerOptions{
				Authentication: authentication, BaseURL: os.Getenv("OPENAI_BASE_URL"), Model: options.model,
				WorkingDir: config.CWD, StateDir: options.stateDir, ReviewTools: !options.yolo,
				Shell: true, AutoReview: false, ReviewModel: options.approvalModel, Tools: tools,
			})
			if runnerErr != nil {
				_ = mcpCloser.Close()
				return nil, nil, runnerErr
			}
			compiled, ok := runner.(*dagoRunner)
			if !ok {
				_ = runnerCloser.Close()
				_ = mcpCloser.Close()
				return nil, nil, fmt.Errorf("start ACP session: unsupported runner %T", runner)
			}
			return &acpDagoRunner{runner: compiled}, &sessionClosers{closers: []io.Closer{runnerCloser, mcpCloser}}, nil
		}
		server := daacp.New(nil, daacp.Options{
			Name: "dacode", Version: buildVersion(), ImagePrompts: true, AudioPrompts: true,
			EmbeddedContext: true, SessionFactory: factory, LoadSession: true,
			AuthMethods:   []acp.AuthMethod{{Agent: &acp.AuthMethodAgent{Id: "cursor_login", Name: "Use configured credentials"}}},
			ConfigOptions: modelConfigOptions(options.model),
		})
		return server.Serve(ctx, stdin, stdout)
	}
	runner, closer, err := newRunner(runnerOptions{
		Authentication: authentication, BaseURL: os.Getenv("OPENAI_BASE_URL"), Model: options.model,
		WorkingDir: workingDir, StateDir: options.stateDir,
		ReviewTools: !options.yolo, Shell: !nonInteractive || options.yolo || options.autoApprove,
		AutoReview: options.autoApprove && !options.acp, ReviewModel: options.approvalModel,
	})
	if err != nil {
		return err
	}
	defer closer.Close()

	threadID := options.resume
	if threadID == "" {
		threadID, err = newThreadID()
		if err != nil {
			return err
		}
	}
	if nonInteractive {
		return runNonInteractive(ctx, runner, workingDir, threadID, options.nonInteractive, options.autoApprove, options.quiet, stdout, stderr)
	}

	model := newTUIModel(ctx, runner, workingDir, options.model, threadID, options.yolo, options.autoApprove, options.message)
	if options.resumePicker {
		model.sessionPicker = &sessionPickerState{loading: true, startup: true}
	} else if options.resume != "" {
		model.sessionPicker = &sessionPickerState{
			sessions: []sessionInfo{{ThreadID: options.resume}}, resuming: true, startup: true,
		}
	}
	program := tea.NewProgram(
		model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithContext(ctx),
		tea.WithInput(stdin),
		tea.WithOutput(stdout),
	)
	_, err = program.Run()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func newACPSessionRunner(options runnerOptions) (runner agentRunner, closer io.Closer, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			runner, closer, err = nil, nil, fmt.Errorf("compile ACP session runner: %v", recovered)
		}
	}()
	return newRunner(options)
}

func parseCLI(arguments []string, output io.Writer) (cliOptions, error) {
	options := cliOptions{workingDir: ".", autoApprove: true, xtermJSAddress: defaultXtermJSAddress}
	resumeCommand := len(arguments) > 0 && arguments[0] == "resume"
	acpCommand := len(arguments) > 0 && arguments[0] == "acp"
	if resumeCommand || acpCommand {
		arguments = arguments[1:]
	}
	var manualReview bool
	flags := flag.NewFlagSet("dacode", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&options.model, "model", "", "model to use")
	flags.StringVar(&options.model, "M", "", "model to use")
	flags.StringVar(&options.message, "message", "", "initial prompt to submit")
	flags.StringVar(&options.message, "m", "", "initial prompt to submit")
	flags.StringVar(&options.nonInteractive, "non-interactive", "", "run one task and exit")
	flags.StringVar(&options.nonInteractive, "n", "", "run one task and exit")
	flags.StringVar(&options.resume, "resume", "", "resume a thread by ID")
	flags.StringVar(&options.resume, "r", "", "resume a thread by ID")
	flags.StringVar(&options.workingDir, "cwd", ".", "workspace directory")
	flags.StringVar(&options.stateDir, "state-dir", "", "state directory")
	flags.BoolVar(&options.yolo, "yolo", false, "run local actions without review")
	flags.BoolVar(&manualReview, "manual-review", false, "require user confirmation for gated actions")
	flags.BoolVar(&options.autoApprove, "approve-for-me", true, "review gated actions with a separate model")
	flags.BoolVar(&options.autoApprove, "auto-approve", true, "review gated actions with a separate model")
	flags.BoolVar(&options.autoApprove, "llm-approve", true, "review gated actions with a separate model")
	flags.BoolVar(&options.autoApprove, "y", true, "review gated actions with a separate model")
	flags.StringVar(&options.approvalModel, "approval-model", "", "model used to review gated actions")
	flags.BoolVar(&options.quiet, "quiet", false, "print only the final response in non-interactive mode")
	flags.BoolVar(&options.quiet, "q", false, "print only the final response in non-interactive mode")
	flags.BoolVar(&options.version, "version", false, "show version")
	flags.BoolVar(&options.version, "v", false, "show version")
	flags.BoolVar(&options.acp, "acp", false, "serve Agent Client Protocol over stdin and stdout")
	flags.BoolVar(&options.serveXtermJS, "serve-xtermjs", false, "serve the TUI through xterm.js")
	flags.StringVar(&options.xtermJSAddress, "xtermjs-address", defaultXtermJSAddress, "xterm.js loopback listen address")
	flags.Usage = func() { printUsage(output) }
	if err := flags.Parse(arguments); err != nil {
		return cliOptions{}, err
	}
	if acpCommand {
		options.acp = true
	}
	if resumeCommand && flags.NArg() > 1 {
		return cliOptions{}, fmt.Errorf("resume accepts at most one session ID")
	}
	if resumeCommand && flags.NArg() == 1 {
		if options.resume != "" {
			return cliOptions{}, fmt.Errorf("session ID was provided twice")
		}
		options.resume = flags.Arg(0)
	} else if !resumeCommand && flags.NArg() != 0 {
		return cliOptions{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if resumeCommand && options.resume == "" {
		options.resumePicker = true
	}
	if options.quiet && options.nonInteractive == "" {
		return cliOptions{}, fmt.Errorf("--quiet requires --non-interactive")
	}
	if options.acp {
		var incompatible []string
		for _, option := range []struct {
			name    string
			enabled bool
		}{
			{name: "--message", enabled: options.message != ""},
			{name: "--non-interactive", enabled: options.nonInteractive != ""},
			{name: "--resume", enabled: options.resume != "" || options.resumePicker},
			{name: "--serve-xtermjs", enabled: options.serveXtermJS},
		} {
			if option.enabled {
				incompatible = append(incompatible, option.name)
			}
		}
		if len(incompatible) > 0 {
			return cliOptions{}, fmt.Errorf("--acp cannot be used with %s", strings.Join(incompatible, ", "))
		}
	}
	if manualReview && options.yolo {
		return cliOptions{}, fmt.Errorf("--manual-review and --yolo cannot be used together")
	}
	if manualReview || options.yolo {
		options.autoApprove = false
	}
	return options, nil
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, "dacode — a Go coding agent")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Usage:")
	fmt.Fprintln(output, "  dacode [OPTIONS]                 Start an interactive thread")
	fmt.Fprintln(output, "  dacode resume [ID] [OPTIONS]     Browse or resume previous sessions")
	fmt.Fprintln(output, "  dacode acp [OPTIONS]             Serve ACP over standard I/O")
	fmt.Fprintln(output, "  dacode -n 'Summarize README.md' Run one task and exit")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Options:")
	fmt.Fprintln(output, "  -r, --resume ID          Resume a thread")
	fmt.Fprintln(output, "  -M, --model MODEL        Model to use")
	fmt.Fprintln(output, "  -m, --message TEXT       Initial prompt to submit")
	fmt.Fprintln(output, "  -n, --non-interactive M  Run one task and exit")
	fmt.Fprintln(output, "  --cwd PATH               Workspace directory")
	fmt.Fprintln(output, "  --manual-review          Require user confirmation for gated actions")
	fmt.Fprintln(output, "  --approval-model MODEL  Model used for automatic reviews (enabled by default)")
	fmt.Fprintln(output, "  --yolo                   Run local actions without review")
	fmt.Fprintln(output, "  -q, --quiet              Clean one-shot output")
	fmt.Fprintln(output, "  -v, --version            Show version")
	fmt.Fprintln(output, "  --acp                    Serve ACP over stdin and stdout")
	fmt.Fprintln(output, "  --serve-xtermjs          Serve the TUI through xterm.js")
	fmt.Fprintln(output, "  --xtermjs-address ADDR   Loopback listen address (default 127.0.0.1:0)")
	fmt.Fprintln(output, "  -h, --help               Show this help")
}

func runNonInteractive(ctx context.Context, runner agentRunner, workingDir, threadID, prompt string, autoReview, quiet bool, stdout, stderr io.Writer) error {
	var streamed strings.Builder
	transcript := "[user, trusted]\n" + prompt
	input := dagent.Input{
		Config:   dacheckpoint.Config{ThreadID: threadID},
		Messages: []damessage.Message{damessage.Human(prompt)}, SkipValueEvents: true,
	}
	var result dagent.Result
	for {
		stream := runner.Start(ctx, input)
		for {
			event, nextErr := stream.Next(ctx)
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				_ = stream.Close()
				return nextErr
			}
			switch event.Mode {
			case dagent.EventToken:
				if event.Chunk != nil {
					text := event.Chunk.MessageDelta.TextContent()
					streamed.WriteString(text)
					if !quiet {
						fmt.Fprint(stdout, text)
					}
				}
			case dagent.EventUpdate:
				if messages, ok := event.Update[dagent.MessagesKey].([]damessage.Message); ok {
					transcript += reviewMessages(messages)
				}
			case dagent.EventToolProgress:
				if !quiet && event.ToolProgress != nil && event.ToolProgress.Output != "" {
					fmt.Fprintf(stderr, "[%s] %s\n", event.ToolProgress.Name, event.ToolProgress.Output)
				}
			}
		}
		var resultErr error
		result, resultErr = stream.Result(ctx)
		_ = stream.Close()
		if resultErr != nil {
			return resultErr
		}
		if len(result.Interrupts) == 0 {
			goal, present := dagoal.FromState(result.State)
			if present && goal != nil && goal.Actionable() {
				input = dagent.Input{
					Config:   dacheckpoint.Config{ThreadID: threadID},
					Messages: []damessage.Message{dagoal.ContinuationMessage(*goal)}, SkipValueEvents: true,
				}
				continue
			}
			break
		}
		if !autoReview {
			return fmt.Errorf("action requires interactive approval; rerun interactively, remove --manual-review, or use --yolo")
		}
		requests, decodeErr := decodeApprovalRequests(result.Interrupts[0].Value)
		if decodeErr != nil {
			return decodeErr
		}
		review, reviewErr := runner.Review(ctx, approvalReviewRequest{
			WorkingDir: workingDir, Transcript: transcript, Requests: requests,
		})
		if reviewErr != nil {
			return fmt.Errorf("automatic approval review failed: %w", reviewErr)
		}
		decisions := make(map[string]dagent.ApprovalChoice, len(requests))
		for _, request := range requests {
			assessment, ok := review.Assessments[request.Call.ID]
			if !ok {
				return fmt.Errorf("automatic approval review omitted %s", request.Call.Name)
			}
			decision := dagent.ApprovalApprove
			if !assessment.approved() {
				decision = dagent.ApprovalReject
			}
			decisions[request.Call.ID] = dagent.ApprovalChoice{
				Decision: decision, Reason: assessment.Rationale, Message: assessment.Rationale,
			}
			if !quiet {
				verdict := "approved"
				if !assessment.approved() {
					verdict = "denied"
				}
				fmt.Fprintf(stderr, "[auto review] %s %s (risk: %s, authorization: %s): %s\n",
					verdict, request.Call.Name, assessment.RiskLevel, assessment.UserAuthorization, assessment.Rationale)
			}
		}
		input = dagent.Input{
			Config: dacheckpoint.Config{ThreadID: threadID},
			Resume: dagent.ApprovalResponse{Decisions: decisions}, SkipValueEvents: true,
		}
	}
	answer := ""
	for index := len(result.Messages) - 1; index >= 0; index-- {
		if result.Messages[index].Role == damessage.RoleAssistant && result.Messages[index].TextContent() != "" {
			answer = result.Messages[index].TextContent()
			break
		}
	}
	if quiet {
		_, err := fmt.Fprintln(stdout, answer)
		return err
	}
	if streamed.Len() == 0 && answer != "" {
		fmt.Fprint(stdout, answer)
	}
	_, err := fmt.Fprintln(stdout)
	return err
}

func reviewMessages(messages []damessage.Message) string {
	var output strings.Builder
	for _, message := range messages {
		switch message.Role {
		case damessage.RoleAssistant:
			fmt.Fprintf(&output, "\n\n[assistant, untrusted]\n%s", message.TextContent())
			for _, call := range message.ToolCalls {
				fmt.Fprintf(&output, "\n[tool request %s, untrusted]\n%s", call.Name, compactJSON(call.Arguments))
			}
		case damessage.RoleTool:
			fmt.Fprintf(&output, "\n\n[tool result %s, untrusted]\n%s", message.Name, message.TextContent())
		}
	}
	return output.String()
}

func versionText() string {
	return "dacode " + buildVersion()
}

func buildVersion() string {
	version := "development"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	return version
}

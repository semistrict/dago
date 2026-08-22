package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	charmterm "github.com/charmbracelet/x/term"
	acp "github.com/coder/acp-go-sdk"
	dagoapi "github.com/semistrict/dago"
	"github.com/semistrict/dago/daacp"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/daconfig"
	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/dainstall"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/daplugin"
	"github.com/semistrict/dago/daproviders/modelconfig"
	"github.com/semistrict/dago/daproviders/openai"
	"github.com/semistrict/dago/dasandbox"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datalon/mcp/oauthpolicy"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/daweb"
	"github.com/semistrict/dago/daworkflow"
)

var dotenvEnvironmentMu sync.Mutex

var errDisplaySettingsFlush = errors.New("save display preferences")

type cliOptions struct {
	model                      string
	modelParameters            map[string]any
	profileOverride            map[string]any
	maxRetries                 *int
	defaultModel               string
	clearDefaultModel          bool
	agent                      string
	defaultAgent               string
	recentAgent                string
	baseURL                    string
	apiKey                     string
	message                    string
	initialSkill               string
	goal                       string
	rubric                     string
	rubricModel                string
	rubricMax                  int
	startupCommand             string
	init                       bool
	nonInteractive             string
	headless                   bool
	resume                     string
	resumePicker               bool
	workingDir                 string
	stateDir                   string
	configPath                 string
	mcpConfigPath              string
	trustProjectMCP            bool
	yolo                       bool
	autoApprove                bool
	approvalModel              string
	shellAllowList             shellAllowList
	quiet                      bool
	noStream                   bool
	json                       bool
	stdin                      bool
	maxTurns                   int
	recursionLimit             int
	memoryAutoSave             bool
	timeout                    time.Duration
	version                    bool
	acp                        bool
	serveXtermJS               bool
	xtermJSAddress             string
	sandbox                    string
	sandboxID                  string
	sandboxSnapshot            string
	sandboxSetup               string
	sandboxDefault             string
	localDevExecutable         string
	localDevArguments          []string
	localDevEndpoint           string
	localDevHealthPath         string
	localDevInheritEnvironment []string
	updateChannel              string
	updateArtifact             string
	updateManifestBase         string
	updatePublicKey            string
	updateTarget               string
}

const (
	defaultHeadlessMaxTurns = 50
	defaultRecursionLimit   = 2000
)

type nonInteractiveOptions struct {
	AutoReview bool
	Quiet      bool
	NoStream   bool
	JSON       bool
	MaxTurns   int
	Timeout    time.Duration
	Rubric     string
}

type commandExitError struct {
	code int
	err  error
}

type silentCommandExitError struct{ commandExitError }

// SilentExit reports whether the command already rendered its complete
// user-facing result and only needs the process status propagated.
func SilentExit(err error) bool {
	var silent *silentCommandExitError
	return errors.As(err, &silent)
}

func (err *commandExitError) Error() string { return err.err.Error() }
func (err *commandExitError) Unwrap() error { return err.err }

// ExitCode returns the process status associated with err. Unclassified errors
// use status 1, making the zero-configuration behavior useful to callers.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *commandExitError
	if errors.As(err, &exitError) {
		return exitError.code
	}
	return 1
}

type acpDagoRunner struct {
	runner *dagoRunner
	model  string
}

func (runner *acpDagoRunner) Stream(ctx context.Context, options ...dagent.RunOption) *dagent.Stream {
	options = append(options,
		dagent.WithState(dastate.Values{
			sessionWorkingDirectoryKey: runner.runner.workingDir,
			sessionModelKey:            runner.model,
		}),
		dagent.WithConfigurable(map[string]any{dagent.RuntimeModelConfigKey: runner.model}),
	)
	return runner.runner.agent.Stream(ctx, options...)
}

func (runner *acpDagoRunner) Cancel(ctx context.Context, options ...dagent.RunOption) (dagent.Result, error) {
	return runner.runner.agent.Cancel(ctx, options...)
}

func (runner *acpDagoRunner) LoadACPSession(ctx context.Context, id string) (daacp.SessionState, error) {
	snapshot, err := runner.runner.agent.State(ctx, dacheckpoint.Config{ThreadID: id})
	if err != nil {
		return daacp.SessionState{}, fmt.Errorf("load ACP session %q: %w", id, err)
	}
	if snapshot.Config.ThreadID == "" {
		return daacp.SessionState{}, fmt.Errorf("ACP session %q was not found", id)
	}
	messages, err := runner.runner.LoadSession(ctx, id)
	if err != nil {
		return daacp.SessionState{}, err
	}
	if runner.runner.saver == nil {
		return daacp.SessionState{}, fmt.Errorf("ACP session %q has no checkpoint saver", id)
	}
	tuple, err := runner.runner.saver.GetTuple(ctx, snapshot.Config)
	if err != nil {
		return daacp.SessionState{}, fmt.Errorf("load ACP session %q checkpoint: %w", id, err)
	}
	if tuple == nil {
		return daacp.SessionState{}, fmt.Errorf("ACP session %q was not found", id)
	}
	cwd, ok := tuple.Checkpoint.ChannelValues[sessionWorkingDirectoryKey].(string)
	if !ok || !filepath.IsAbs(cwd) {
		return daacp.SessionState{}, fmt.Errorf("ACP session %q has no valid working directory", id)
	}
	model := ""
	if value, exists := tuple.Checkpoint.ChannelValues[sessionModelKey]; exists {
		var valid bool
		model, valid = value.(string)
		if !valid || model == "" {
			return daacp.SessionState{}, fmt.Errorf("ACP session %q has no valid model selection", id)
		}
	}
	return daacp.SessionState{Messages: messages, CWD: cwd, Model: model}, nil
}

func (runner *acpDagoRunner) SaveACPModelSelection(ctx context.Context, id, model string) error {
	if model == "" || model != runner.model {
		return fmt.Errorf("save ACP session model: runner model %q does not match %q", runner.model, model)
	}
	_, err := runner.runner.agent.UpdateState(ctx, dacheckpoint.Config{ThreadID: id}, dastate.Values{
		sessionWorkingDirectoryKey: runner.runner.workingDir,
		sessionModelKey:            model,
	})
	if err != nil {
		return fmt.Errorf("save ACP session model %q: %w", model, err)
	}
	return nil
}

func (runner *acpDagoRunner) SubscribeWorkflows() (<-chan daworkflow.Status, func()) {
	return runner.runner.workflows.Subscribe()
}

func (runner *acpDagoRunner) Workflows() []daworkflow.Status {
	return runner.runner.Workflows()
}

func (runner *acpDagoRunner) CancelWorkflow(runID string) bool {
	return runner.runner.CancelWorkflow(runID)
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
	entries := normalizeModelSelectorEntries(modelSelectorCatalog(nil))
	values := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(entries)+1)
	seen := make(map[string]struct{}, len(entries)+1)
	labels := make(map[string]string, len(entries))
	for _, entry := range entries {
		labels[entry.Spec] = entry.Label
	}
	appendModel := func(spec, label string) {
		if spec == "" {
			return
		}
		if _, exists := seen[spec]; exists {
			return
		}
		seen[spec] = struct{}{}
		values = append(values, acp.SessionConfigSelectOption{Name: label, Value: acp.SessionConfigValueId(spec)})
	}
	for _, compatibilitySpec := range []string{model, "gpt-5.6-sol", defaultModel, suggestedReviewModel} {
		label := labels[compatibilitySpec]
		if label == "" {
			label = labels["openai:"+compatibilitySpec]
		}
		if label == "" {
			label = modelSelectorDisplayName(compatibilitySpec)
		}
		appendModel(compatibilitySpec, label)
	}
	for _, entry := range entries {
		appendModel(entry.Spec, entry.Label)
	}
	return []acp.SessionConfigOption{{Select: &acp.SessionConfigOptionSelect{
		Id: "model", Name: "Model", Category: &category, CurrentValue: acp.SessionConfigValueId(model),
		Options: acp.SessionConfigSelectOptions{Ungrouped: &values},
	}}}
}

func Run(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return RunWithSandboxRegistry(ctx, arguments, stdin, stdout, stderr, dasandbox.NewRegistry(nil, dasandbox.RegistryOptions{}))
}

// RunWithSandboxRegistry runs the command with explicitly linked remote
// providers. Merely registering or configuring a provider never enables it;
// callers must also pass --sandbox.
func RunWithSandboxRegistry(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer, sandboxRegistry *dasandbox.Registry) error {
	dotenvEnvironmentMu.Lock()
	defer dotenvEnvironmentMu.Unlock()
	if len(arguments) > 0 && (arguments[0] == "install" || arguments[0] == "update" || arguments[0] == "doctor" || arguments[0] == "config" || arguments[0] == "agents" || arguments[0] == "skills" || arguments[0] == "auth" || arguments[0] == "mcp" || arguments[0] == "plugin" || arguments[0] == "plugins") {
		restoreEnvironment, err := loadCLIEnvironment(".", stderr)
		if err != nil {
			return err
		}
		defer restoreEnvironment()
	}
	if len(arguments) > 0 && arguments[0] == "install" {
		return runInstallCommand(ctx, arguments[1:], stdin, stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "update" {
		return runUpdateCommand(ctx, arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "doctor" {
		return runDoctorCommand(ctx, arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "config" {
		return runConfigCommand(ctx, arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "agents" {
		return runAgentsCommand(ctx, arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "skills" {
		return runSkillsCommand(ctx, arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "auth" {
		return runAuthCommand(ctx, arguments[1:], stdin, stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "mcp" {
		return runMCPCommand(ctx, arguments[1:], stdin, stdout, stderr)
	}
	if len(arguments) > 0 && (arguments[0] == "plugin" || arguments[0] == "plugins") {
		return runPluginCommand(ctx, arguments[1:], stdout)
	}
	preliminary, err := parseCLI(arguments, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if preliminary.version {
		_, err := fmt.Fprintln(stdout, versionText())
		return err
	}
	environment, err := newCLIEnvironmentOverlayWithOptions(preliminary.workingDir, stderr, preliminary.acp)
	if err != nil {
		return err
	}
	defer environment.Close()
	resolved, err := resolveCLIConfig(ctx, preliminary.configPath)
	if err != nil {
		return err
	}
	options, err := parseCLIResolved(arguments, stderr, &resolved)
	if err != nil {
		return err
	}
	if options.clearDefaultModel || options.defaultModel != "" {
		return runDefaultModelCommand(ctx, options, resolved.store, stdout)
	}
	if options.serveXtermJS {
		return serveXtermJS(ctx, xtermJSServerOptions{
			Address: options.xtermJSAddress, Arguments: xtermSessionArguments(arguments),
			Stdout: stdout, Stderr: stderr,
		})
	}
	if !options.acp {
		pipePrompt, piped, readErr := readHeadlessInput(stdin, options.stdin)
		if readErr != nil {
			return readErr
		}
		if piped {
			if options.nonInteractive != "" {
				options.nonInteractive = pipePrompt + "\n\n" + options.nonInteractive
			} else {
				options.nonInteractive = pipePrompt
			}
			options.headless = true
		}
	}
	nonInteractive := options.headless
	if options.headless && strings.TrimSpace(options.nonInteractive) == "" {
		return fmt.Errorf("non-interactive input is empty")
	}
	if options.message != "" && options.headless {
		return fmt.Errorf("--message and --non-interactive cannot be used together")
	}
	if strings.TrimSpace(options.goal) != "" {
		if nonInteractive {
			return errors.New("--goal requires interactive mode")
		}
		if options.message != "" || strings.TrimSpace(options.initialSkill) != "" {
			return errors.New("--goal cannot be combined with --message or --skill")
		}
		if strings.TrimSpace(options.rubric) != "" {
			return errors.New("--goal and --rubric are mutually exclusive")
		}
	}
	if strings.TrimSpace(options.rubric) != "" && !nonInteractive {
		return errors.New("--rubric requires non-interactive mode")
	}
	if !nonInteractive && strings.TrimSpace(options.rubricModel) != "" {
		return errors.New("--rubric-model requires non-interactive mode")
	}
	if !nonInteractive && options.rubricMax != 0 {
		return errors.New("--rubric-max-iterations requires non-interactive mode")
	}
	if options.resumePicker && options.headless {
		return fmt.Errorf("the resume picker cannot be used with --non-interactive")
	}
	if options.resumePicker && options.message != "" {
		return fmt.Errorf("the resume picker cannot be used with --message")
	}
	if options.resumePicker && strings.TrimSpace(options.initialSkill) != "" {
		return fmt.Errorf("the resume picker cannot be used with --skill")
	}
	if !nonInteractive {
		for _, incompatible := range []struct {
			name    string
			enabled bool
		}{
			{name: "--quiet", enabled: options.quiet},
			{name: "--no-stream", enabled: options.noStream},
			{name: "--json", enabled: options.json},
			{name: "--max-turns", enabled: options.maxTurns != 0},
			{name: "--timeout", enabled: options.timeout != 0},
		} {
			if incompatible.enabled {
				return fmt.Errorf("%s requires --non-interactive or piped stdin", incompatible.name)
			}
		}
	}
	if nonInteractive && options.yolo {
		return fmt.Errorf("--yolo cannot be used with --non-interactive; use --shell-allow-list all for unrestricted shell commands")
	}
	workingDir, err := filepath.Abs(options.workingDir)
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	info, err := os.Stat(workingDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("working directory %q is not a directory", workingDir)
	}
	if strings.TrimSpace(options.rubric) != "" {
		options.rubric, err = resolveRubricText(options.rubric, workingDir)
		if err != nil {
			return err
		}
	}
	var startupTranscript strings.Builder
	startupStdout := stdout
	if !nonInteractive {
		startupStdout = &startupTranscript
	}
	if err := runStartupCommand(
		ctx, options.startupCommand, workingDir, options.quiet || options.json, startupStdout, stderr,
	); err != nil {
		return err
	}
	if strings.TrimSpace(options.initialSkill) != "" {
		request := options.message
		if nonInteractive {
			request = options.nonInteractive
		}
		request, err = composeInitialSkillPrompt(workingDir, options.initialSkill, request)
		if err != nil {
			return err
		}
		if nonInteractive {
			options.nonInteractive = request
		} else {
			options.message = request
		}
	}
	if options.stateDir == "" {
		configDir, configErr := os.UserConfigDir()
		if configErr != nil {
			return fmt.Errorf("resolve config directory: %w", configErr)
		}
		options.stateDir = filepath.Join(configDir, "dacode")
	}
	options.stateDir, err = filepath.Abs(options.stateDir)
	if err != nil {
		return fmt.Errorf("resolve state directory: %w", err)
	}
	var acpLog *acpDiagnosticLog
	if options.acp {
		acpLog, err = openACPDiagnosticLog(options.stateDir)
		if err != nil {
			return fmt.Errorf("open ACP diagnostic log: %w", err)
		}
		defer acpLog.Close()
		acpLog.Event("process.start", "working_dir="+workingDir, "state_dir="+options.stateDir)
	}
	updateProfile, err := configuredTUIUpdateProfile(options)
	if err != nil {
		return fmt.Errorf("configure interactive updates: %w", err)
	}
	onboardingLaunch, onboardingDiagnostics := decideOnboardingLaunch(
		options.stateDir, !nonInteractive, options.init, resolved.snapshot, os.LookupEnv,
	)
	var localDevelopment *localDevRuntime
	if options.localDevExecutable != "" {
		serverOptions := options
		serverOptions.workingDir = workingDir
		serverOptions.stateDir, err = filepath.Abs(options.stateDir)
		if err != nil {
			return fmt.Errorf("resolve local development server state directory: %w", err)
		}
		localDevelopment = newLocalDevRuntime(serverOptions, serverConfigFor(serverOptions, !nonInteractive))
		if err := localDevelopment.Start(ctx); err != nil {
			return err
		}
		defer func() {
			if closeErr := localDevelopment.Close(context.Background()); closeErr != nil {
				_, _ = fmt.Fprintf(stderr, "Local development server cleanup failed: %s\n", sanitizeLocalDevDiagnostic(closeErr.Error(), 512))
			}
		}()
	}
	var runtimePlugins pluginRuntimeComponents
	if !options.acp {
		runtimePlugins, err = loadRuntimePlugins(ctx, filepath.Join(options.stateDir, "plugins"), workingDir, os.LookupEnv)
		if err != nil {
			return err
		}
		writePluginRuntimeWarnings(stderr, runtimePlugins.Warnings)
	}
	modelOptions := modelconfig.ResolveOptions{
		Parameters: options.modelParameters, ProfileOverrides: options.profileOverride,
		MaxRetries: options.maxRetries,
	}
	if options.baseURL != "" {
		modelOptions.BaseURL = new(options.baseURL)
	}
	var authentication modelAuthentication
	if !options.acp {
		authentication, err = resolveConfiguredModelAuthentication(
			ctx, options.model, options.apiKey, options.stateDir, stderr, modelOptions,
		)
		if err != nil {
			return err
		}
	}
	webTools, err := defaultWebTools(ctx)
	if err != nil {
		return err
	}
	homeDirectory, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve MCP user configuration directory: %w", err)
	}
	policyPath := filepath.Join(homeDirectory, ".deepagents", "config.toml")
	updatePreference, updatePreferenceDiagnostics := loadAutoUpdatePreference(policyPath, os.LookupEnv)
	updateStateStore := newUpdateStateStore(filepath.Join(options.stateDir, "update-state.json"))
	updateState, updateStateErr := updateStateStore.Load(ctx)
	if updateStateErr != nil {
		updatePreference = autoUpdatePreference{Source: "unavailable"}
		updateState = newUpdatePersistentState()
	}
	mcpOAuthTokenDirectory := filepath.Join(homeDirectory, ".deepagents", ".state", mcpTokenDirectory)
	var mcpResolution mcpConfigResolution
	if !options.acp {
		mcpResolution, err = resolveMCPConfig(
			ctx, homeDirectory, workingDir, options.mcpConfigPath,
			policyPath, options.trustProjectMCP, os.LookupEnv,
		)
		if err != nil {
			return err
		}
		mcpResolution = mergePluginMCP(mcpResolution, runtimePlugins.MCP)
		writeMCPResolutionDiagnostics(stderr, mcpResolution)
	}

	if options.acp {
		factory := func(factoryContext context.Context, config daacp.AgentSessionContext) (daacp.Runner, io.Closer, error) {
			model := config.Model
			if model == "" {
				model = options.model
			}
			acpLog.Event("session.new.start", "session_id="+config.ID, "model="+model, "cwd="+config.CWD)
			acpLog.Event("session.new.model_auth.start")
			sessionAuthentication, authenticationErr := resolveConfiguredModelAuthenticationForACP(
				factoryContext, model, options.apiKey, options.stateDir, stderr, modelOptions,
			)
			if authenticationErr != nil {
				acpLog.Failure("session.new.model_auth.failed", authenticationErr)
				return nil, nil, authenticationErr
			}
			acpLog.Event("session.new.model_auth.succeeded")
			acpLog.Event("session.new.sandbox.start")
			sandboxSession, sandboxErr := openSandboxSession(factoryContext, sandboxRegistry, config.CWD, options)
			if sandboxErr != nil {
				acpLog.Failure("session.new.sandbox.failed", sandboxErr)
				return nil, nil, sandboxErr
			}
			acpLog.Event("session.new.sandbox.succeeded")
			acpLog.Event("session.new.plugins.start")
			sessionPlugins, connectErr := loadRuntimePlugins(factoryContext, filepath.Join(options.stateDir, "plugins"), config.CWD, os.LookupEnv)
			if connectErr != nil {
				acpLog.Failure("session.new.plugins.failed", connectErr)
				if sandboxSession != nil {
					_ = sandboxSessionCloser{sandboxSession}.Close()
				}
				return nil, nil, connectErr
			}
			acpLog.Event("session.new.plugins.succeeded")
			writePluginRuntimeWarnings(stderr, sessionPlugins.Warnings)
			acpLog.Event("session.new.mcp_config.start")
			sessionMCPResolution, connectErr := resolveMCPConfig(
				factoryContext, homeDirectory, config.CWD, options.mcpConfigPath,
				policyPath, options.trustProjectMCP, os.LookupEnv,
			)
			if connectErr != nil {
				acpLog.Failure("session.new.mcp_config.failed", connectErr)
				if sandboxSession != nil {
					_ = sandboxSessionCloser{sandboxSession}.Close()
				}
				return nil, nil, connectErr
			}
			acpLog.Event("session.new.mcp_config.succeeded")
			sessionMCPResolution = mergePluginMCP(sessionMCPResolution, sessionPlugins.MCP)
			writeMCPResolutionDiagnostics(stderr, sessionMCPResolution)
			acpLog.Event("session.new.configured_mcp.start")
			configuredMCP, connectErr := connectConfiguredMCPServersWithOAuth(factoryContext, sessionMCPResolution.runtimeConnections(), mcpOAuthTokenDirectory)
			if connectErr != nil {
				acpLog.Failure("session.new.configured_mcp.failed", connectErr)
				if sandboxSession != nil {
					_ = sandboxSessionCloser{sandboxSession}.Close()
				}
				return nil, nil, connectErr
			}
			acpLog.Event("session.new.configured_mcp.succeeded")
			writeConfiguredMCPDiagnostics(stderr, configuredMCP.Servers)
			acpLog.Event("session.new.client_mcp.start")
			tools, mcpCloser, connectErr := connectMCPServers(factoryContext, config.MCPServers)
			if connectErr != nil {
				acpLog.Failure("session.new.client_mcp.failed", connectErr)
				_ = configuredMCP.Close()
				if sandboxSession != nil {
					_ = sandboxSessionCloser{sandboxSession}.Close()
				}
				return nil, nil, connectErr
			}
			acpLog.Event("session.new.client_mcp.succeeded")
			tools = append(append(append([]datool.Tool(nil), webTools...), configuredMCP.Tools...), tools...)
			runnerWorkingDir := config.CWD
			if sandboxSession != nil {
				runnerWorkingDir = sandboxSession.WorkingDir()
			}
			acpLog.Event("session.new.runner.start")
			runner, runnerCloser, runnerErr := newACPSessionRunner(runnerOptions{
				Authentication: sessionAuthentication, BaseURL: options.baseURL, Model: model,
				WorkingDir: runnerWorkingDir, ConfigurationDir: config.CWD,
				StateDir: options.stateDir, AgentName: options.agent,
				DefaultAgent: options.defaultAgent, RecentAgent: options.recentAgent, AgentConfig: resolved.store,
				ReviewTools: !options.yolo,
				Shell:       true, ShellAllowList: options.shellAllowList,
				AutoReview: true, ReviewModel: options.approvalModel, Tools: tools,
				RecursionLimit: options.recursionLimit, MemoryReadOnly: !options.memoryAutoSave,
				Backend: sandboxBackend(sandboxSession),
				Plugins: sessionPlugins,
			})
			if runnerErr != nil {
				acpLog.Failure("session.new.runner.failed", runnerErr)
				_ = mcpCloser.Close()
				_ = configuredMCP.Close()
				if sandboxSession != nil {
					_ = sandboxSessionCloser{sandboxSession}.Close()
				}
				return nil, nil, runnerErr
			}
			acpLog.Event("session.new.runner.succeeded")
			compiled, ok := runner.(*dagoRunner)
			if !ok {
				_ = runnerCloser.Close()
				_ = mcpCloser.Close()
				_ = configuredMCP.Close()
				err := fmt.Errorf("start ACP session: unsupported runner %T", runner)
				acpLog.Failure("session.new.runner_type.failed", err)
				return nil, nil, err
			}
			closers := []io.Closer{runnerCloser, mcpCloser, configuredMCP}
			if sandboxSession != nil {
				closers = append(closers, sandboxSessionCloser{sandboxSession})
			}
			acpLog.Event("session.new.succeeded")
			return &acpDagoRunner{runner: compiled, model: model}, &sessionClosers{closers: closers}, nil
		}
		server := daacp.NewFactory(factory, daacp.Options{
			Name: "dacode", Version: buildVersion(), ImagePrompts: true, AudioPrompts: true,
			EmbeddedContext: true, LoadSession: true,
			AuthMethods:   []acp.AuthMethod{{Agent: &acp.AuthMethodAgent{Id: "cursor_login", Name: "Use configured credentials"}}},
			ConfigOptions: modelConfigOptions(options.model),
		})
		acpLog.Event("transport.serve.start")
		err := server.Serve(ctx, stdin, stdout)
		if err != nil {
			acpLog.Failure("transport.serve.failed", err)
		} else {
			acpLog.Event("transport.serve.stopped")
		}
		return err
	}
	sandboxSession, err := openSandboxSession(ctx, sandboxRegistry, workingDir, options)
	if err != nil {
		return err
	}
	if sandboxSession != nil {
		defer sandboxSessionCloser{sandboxSession}.Close()
	}
	runnerWorkingDir := workingDir
	if sandboxSession != nil {
		runnerWorkingDir = sandboxSession.WorkingDir()
	}
	configuredMCP, err := connectConfiguredMCPServersWithOAuth(ctx, mcpResolution.runtimeConnections(), mcpOAuthTokenDirectory)
	if err != nil {
		return err
	}
	writeConfiguredMCPDiagnostics(stderr, configuredMCP.Servers)
	runtimeTools := append(append([]datool.Tool(nil), webTools...), configuredMCP.Tools...)
	hookStatus := newHookUISink()
	runner, closer, err := newRunner(runnerOptions{
		Authentication: authentication, BaseURL: options.baseURL, Model: options.model,
		WorkingDir: runnerWorkingDir, ConfigurationDir: workingDir,
		StateDir: options.stateDir, AgentName: options.agent,
		DefaultAgent: options.defaultAgent, RecentAgent: options.recentAgent, AgentConfig: resolved.store,
		ReviewTools: true, Shell: !nonInteractive || options.shellAllowList.configured(),
		ShellAllowList: options.shellAllowList,
		AutoReview:     options.autoApprove && !options.acp, ReviewModel: options.approvalModel,
		Tools:    runtimeTools,
		Headless: nonInteractive, RecursionLimit: options.recursionLimit, MemoryReadOnly: !options.memoryAutoSave,
		RubricModel: options.rubricModel, RubricMaxIterations: options.rubricMax,
		Backend:    sandboxBackend(sandboxSession),
		Plugins:    runtimePlugins,
		HookStatus: hookStatus,
	})
	if err != nil {
		_ = configuredMCP.Close()
		return err
	}
	initialCloser := &sessionClosers{closers: []io.Closer{closer, configuredMCP}}
	var runtimeCloser io.Closer = initialCloser
	if !nonInteractive {
		reloadFactory := func(reloadContext context.Context, sessionDisabled map[string]bool) (reloadableRuntimeBuild, error) {
			rollbackEnvironment, changes, reloadErr := environment.Reload(io.Discard)
			if reloadErr != nil {
				return reloadableRuntimeBuild{}, reloadErr
			}
			fail := func(err error) (reloadableRuntimeBuild, error) {
				rollbackEnvironment()
				return reloadableRuntimeBuild{}, err
			}
			reloadedConfig, reloadErr := resolveCLIConfig(reloadContext, preliminary.configPath)
			if reloadErr != nil {
				return fail(reloadErr)
			}
			reloadedOptions, reloadErr := parseCLIResolved(arguments, io.Discard, &reloadedConfig)
			if reloadErr != nil {
				return fail(reloadErr)
			}
			reloadedAuthentication, reloadErr := resolveConfiguredModelAuthentication(
				reloadContext, options.model, reloadedOptions.apiKey, options.stateDir, io.Discard, modelOptions,
			)
			if reloadErr != nil {
				return fail(reloadErr)
			}
			reloadedWebTools, reloadErr := defaultWebTools(reloadContext)
			if reloadErr != nil {
				return fail(reloadErr)
			}
			reloadedPlugins, reloadErr := loadRuntimePlugins(reloadContext, filepath.Join(options.stateDir, "plugins"), workingDir, os.LookupEnv)
			if reloadErr != nil {
				return fail(reloadErr)
			}
			reloadedMCP, reloadErr := resolveMCPConfig(
				reloadContext, homeDirectory, workingDir, options.mcpConfigPath,
				policyPath, options.trustProjectMCP, os.LookupEnv,
			)
			if reloadErr != nil {
				return fail(reloadErr)
			}
			reloadedMCP = mergePluginMCP(reloadedMCP, reloadedPlugins.MCP)
			connected, reloadErr := connectConfiguredMCPServersWithOAuth(reloadContext, filterMCPRuntimeConnections(reloadedMCP, sessionDisabled), mcpOAuthTokenDirectory)
			if reloadErr != nil {
				return fail(reloadErr)
			}
			reloadedTools := append(append([]datool.Tool(nil), reloadedWebTools...), connected.Tools...)
			reloadedRunner, reloadedCloser, reloadErr := newRunner(runnerOptions{
				Authentication: reloadedAuthentication, BaseURL: options.baseURL, Model: options.model,
				WorkingDir: runnerWorkingDir, ConfigurationDir: workingDir,
				StateDir: options.stateDir, AgentName: options.agent,
				DefaultAgent: options.defaultAgent, RecentAgent: options.recentAgent, AgentConfig: reloadedConfig.store,
				ReviewTools: true, Shell: true, ShellAllowList: reloadedOptions.shellAllowList,
				AutoReview: options.autoApprove, ReviewModel: options.approvalModel,
				Tools: reloadedTools, RecursionLimit: options.recursionLimit, MemoryReadOnly: !options.memoryAutoSave,
				RubricModel: options.rubricModel, RubricMaxIterations: options.rubricMax,
				Backend: sandboxBackend(sandboxSession), Plugins: reloadedPlugins, HookStatus: hookStatus,
			})
			if reloadErr != nil {
				_ = connected.Close()
				return fail(reloadErr)
			}
			return reloadableRuntimeBuild{
				runner: reloadedRunner, closer: &sessionClosers{closers: []io.Closer{reloadedCloser, connected}},
				loadedIDs: reloadedPlugins.LoadedIDs, warnings: reloadedPlugins.Warnings,
				changes: changes, rollback: rollbackEnvironment,
				mcp: reloadedMCP, mcpBundle: connected,
			}, nil
		}
		materializer := daplugin.NewSecureMaterializer(daweb.NewClient(daweb.Options{}), "", daplugin.MaterializerOptions{})
		manager := newPluginManagerService(filepath.Join(options.stateDir, "plugins"), materializer)
		reloadable := newReloadableRunner(reloadableRuntimeBuild{
			runner: runner, closer: initialCloser, loadedIDs: runtimePlugins.LoadedIDs, warnings: runtimePlugins.Warnings,
		}, manager, reloadFactory, hookStatus)
		reloadable.configureMCPRuntime(mcpResolution, configuredMCP, mcpOAuthTokenDirectory, oauthpolicy.Login)
		runner, runtimeCloser = reloadable, reloadable
	}
	defer runtimeCloser.Close()

	threadID := options.resume
	if threadID == "" {
		threadID, err = newThreadID()
		if err != nil {
			return err
		}
	}
	if nonInteractive {
		return runHeadless(ctx, runner, runnerWorkingDir, threadID, options.nonInteractive, nonInteractiveOptions{
			AutoReview: options.autoApprove, Quiet: options.quiet, NoStream: options.noStream,
			JSON: options.json, MaxTurns: options.maxTurns, Timeout: options.timeout,
			Rubric: options.rubric,
		}, stdout, stderr)
	}

	model := newTUIModel(ctx, runner, runnerWorkingDir, options.model, threadID, options.yolo, options.autoApprove, options.message)
	model.configureUpdates(updateProfile, newAutoUpdatePreferenceStore(policyPath), updateStateStore, updatePreference, updateState)
	model.configureAutoClassifier(options.model, options.approvalModel, newAutoClassifierPreferences(resolved.store))
	if mcpController, ok := runner.(mcpRuntimeController); ok {
		model.configureMCP(mcpController)
	}
	authPath, err := authStorePath("")
	if err != nil {
		return err
	}
	authManager := newAuthManager(dacredential.NewStore(authPath, time.Now, dacredential.Options{}), os.LookupEnv)
	model.authManager = newAuthTUIController(authManager, filepath.Join(options.stateDir, oauthStoreFilename), openai.Login, openExternalURL)
	oauthConfigured, oauthStatusErr := storedOAuthSession(filepath.Join(options.stateDir, oauthStoreFilename))
	availabilityResolver := modelconfig.NewResolver(authManager.store, os.LookupEnv, configuredModelFactories(), modelconfig.Options{})
	if oauthStatusErr != nil || model.configureModelProviderAvailability(ctx, availabilityResolver, oauthConfigured) != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Model availability could not be loaded; selector status is unknown."})
	}
	traceCommand, traceProject := configuredTraceTUI(ctx, authManager.store, os.LookupEnv)
	model.configureTrace(traceCommand, traceProject)
	var lifecycleRestart restartController
	if localDevelopment != nil {
		lifecycleRestart = localDevelopment.RestartController()
	}
	model.configureLifecycle(options.stateDir, lifecycleRestart, configuredSessionResumeOptions(resolved, runnerWorkingDir, cwdResumeAbortNone))
	model.installController = dainstall.New(dainstall.OSExecutor(), dacodeInstallCatalog(), dainstall.Options{LockPath: filepath.Join(options.stateDir, "install.lock")})
	if err := model.configureModelPreferences(modelconfig.NewPreferenceStore(resolved.store)); err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Could not load model preferences; using session-only selection."})
	}
	if onboardingLaunch {
		model.onboarding = newOnboardingState(onboardingDependencyCatalog(), modelSelectorCatalog(model.modelRecentSpecs), model.modelName)
	}
	for _, diagnostic := range onboardingDiagnostics {
		model.appendItem(transcriptItem{kind: itemError, text: "Onboarding: " + diagnostic})
	}
	for _, diagnostic := range updatePreferenceDiagnostics {
		model.appendItem(transcriptItem{kind: itemError, text: "Automatic updates: " + diagnostic})
	}
	if updateStateErr != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Automatic updates: durable state is unavailable; automatic updates are disabled."})
	}
	for _, diagnostic := range model.configureTheme(policyPath, os.Getenv("TERM_PROGRAM")) {
		model.appendItem(transcriptItem{kind: itemError, text: "Theme configuration: " + diagnostic})
	}
	suppressedWarnings, warningDiagnostics := loadSuppressedWarnings(policyPath)
	tavilyResolution, tavilyErr := authManager.store.Resolve(ctx, "tavily", os.LookupEnv)
	_, ripgrepErr := exec.LookPath("rg")
	webSearchConfigured := runner.Profile().SupportsWebSearch || tavilyErr == nil && tavilyResolution.Configured
	model.configureStartupNotifications(suppressedWarnings, ripgrepErr == nil, webSearchConfigured, options.yolo)
	for _, diagnostic := range warningDiagnostics {
		model.appendItem(transcriptItem{kind: itemError, text: "Notifications: " + diagnostic})
	}
	model.initialGoal = strings.TrimSpace(options.goal)
	model.startupTranscript = strings.TrimRight(startupTranscript.String(), "\r\n")
	if err := model.configureInput(filepath.Join(options.stateDir, "input-history.jsonl")); err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Could not load input history; using session-only history: " + err.Error()})
	}
	if err := model.configureDisplaySettings(filepath.Join(options.stateDir, displaySettingsFilename)); err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Could not load display preferences; using defaults: " + err.Error()})
	}
	if err := model.configureApprovalState(filepath.Join(options.stateDir, approvalPreferencesFilename), options.approvalModel, options.resume != ""); err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Could not load or persist approval mode; using Manual: " + err.Error()})
	}
	if options.resumePicker {
		model.sessionPicker = &sessionPickerState{loading: true, startup: true}
	} else if options.resume != "" {
		model.sessionPicker = &sessionPickerState{
			sessions: []sessionInfo{{ThreadID: options.resume}}, resuming: true, startup: true,
		}
	}
	stderrTTY := false
	if terminal, ok := stderr.(*os.File); ok {
		stderrTTY = charmterm.IsTerminal(terminal.Fd())
	}
	cursorGuide := loadITermCursorGuide(
		os.LookupEnv, stderrTTY,
		filepath.Join(homeDirectory, "Library", "Preferences", "com.googlecode.iterm2.plist"),
	)
	restoreCursorGuide := suspendITermCursorGuide(stderr, cursorGuide)
	defer restoreCursorGuide()
	program := tea.NewProgram(
		newProgramModel(model),
		tea.WithContext(ctx),
		tea.WithInput(stdin),
		tea.WithOutput(stdout),
	)
	defer fmt.Fprint(stdout, terminalBackgroundResetSequence())
	finalModel, err := program.Run()
	return finishTUIRun(finalModel, err, stdout)
}

func finishTUIRun(finalModel tea.Model, programErr error, stdout io.Writer) error {
	var flushErr error
	if wrapped, ok := finalModel.(programModel); ok {
		completed := wrapped.model
		if err := completed.flushDisplaySettings(); err != nil {
			flushErr = errDisplaySettingsFlush
		}
		if summary := completed.exitUsageSummary(); summary != "" {
			fmt.Fprintln(stdout, summary)
		}
		if command := completed.exitResumeCommand(); command != "" {
			fmt.Fprintf(stdout, "Resume this session:\n%s\n", command)
		}
	}
	if errors.Is(programErr, context.Canceled) {
		programErr = nil
	}
	return errors.Join(programErr, flushErr)
}

func (model *tuiModel) exitResumeCommand() string {
	if model == nil || model.runner == nil {
		return ""
	}
	reader, ok := model.runner.(sessionMetadataReader)
	if !ok {
		return ""
	}
	threadID := validThreadSelectorID(model.threadID)
	if threadID == "" || threadID != model.threadID {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	session, err := reader.SessionMetadata(ctx, threadID)
	if err != nil || session.ThreadID != threadID || session.CheckpointID == "" {
		return ""
	}
	return "dacode resume " + shellQuoteResumeThreadID(threadID)
}

func shellQuoteResumeThreadID(value string) string {
	safe := value != ""
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-._:", character) {
			continue
		}
		safe = false
		break
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func newACPSessionRunner(options runnerOptions) (runner agentRunner, closer io.Closer, err error) {
	// Foreground ACP tool approvals are still projected to the client by daacp.
	// Background workflow workers cannot suspend and surface those prompts through
	// the foreground session, so give them the same fail-closed model reviewer used
	// by interactive auto-review mode.
	options.AutoReview = true
	defer func() {
		if recovered := recover(); recovered != nil {
			runner, closer, err = nil, nil, fmt.Errorf("compile ACP session runner: %v", recovered)
		}
	}()
	return newRunner(options)
}

func parseCLI(arguments []string, output io.Writer) (cliOptions, error) {
	resolver := daconfig.NewResolver(cliConfigManifest, os.LookupEnv, daconfig.ResolverOptions{})
	snapshot, err := resolver.Resolve(nil, daconfig.Layer{})
	if err != nil {
		return cliOptions{}, err
	}
	return parseCLIResolved(arguments, output, &resolvedCLIConfig{snapshot: snapshot})
}

func parseCLIResolved(arguments []string, output io.Writer, resolved *resolvedCLIConfig) (cliOptions, error) {
	arguments = normalizeOptionalSandboxFlag(arguments)
	arguments = normalizeOptionalDefaultModelFlag(arguments)
	options := cliOptions{
		workingDir: ".", autoApprove: true, xtermJSAddress: defaultXtermJSAddress,
		model: defaultModel, approvalModel: defaultReviewModel,
		recursionLimit: defaultRecursionLimit, memoryAutoSave: true,
		localDevEndpoint: defaultLocalDevEndpoint, localDevHealthPath: defaultLocalDevHealthPath,
	}
	resumeCommand := len(arguments) > 0 && arguments[0] == "resume"
	acpCommand := len(arguments) > 0 && arguments[0] == "acp"
	if resumeCommand || acpCommand {
		arguments = arguments[1:]
	}
	var manualReview bool
	var rawShellAllowList string
	flags := flag.NewFlagSet("dacode", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&options.model, "model", options.model, "model to use")
	flags.StringVar(&options.model, "M", options.model, "model to use")
	flags.Func("model-params", "extra model constructor parameters as a JSON object", func(value string) error {
		parsed, parseErr := parseModelJSONObject("--model-params", value)
		if parseErr != nil {
			return parseErr
		}
		options.modelParameters = parsed
		return nil
	})
	flags.Func("profile-override", "runtime model profile fields as a JSON object", func(value string) error {
		parsed, parseErr := parseModelJSONObject("--profile-override", value)
		if parseErr != nil {
			return parseErr
		}
		options.profileOverride = parsed
		return nil
	})
	flags.Func("max-retries", "maximum transient model retries", func(value string) error {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil || parsed < 0 || parsed > 100 {
			return errors.New("--max-retries must be an integer from 0 through 100")
		}
		options.maxRetries = new(parsed)
		return nil
	})
	flags.StringVar(&options.defaultModel, "default-model", "", "set or show the default model")
	flags.BoolVar(&options.clearDefaultModel, "clear-default-model", false, "clear the default model")
	flags.StringVar(&options.agent, "agent", "", "named agent profile to use")
	flags.StringVar(&options.agent, "a", "", "named agent profile to use")
	flags.StringVar(&options.message, "message", "", "initial prompt to submit")
	flags.StringVar(&options.message, "m", "", "initial prompt to submit")
	flags.StringVar(&options.initialSkill, "skill", "", "invoke a project skill at startup")
	flags.StringVar(&options.initialSkill, "s", "", "invoke a project skill at startup")
	flags.StringVar(&options.goal, "goal", "", "draft and review a persistent goal")
	flags.StringVar(&options.rubric, "rubric", "", "grade a non-interactive task against literal criteria or @path")
	flags.StringVar(&options.rubricModel, "rubric-model", "", "model used to grade rubric criteria")
	flags.StringVar(&options.startupCommand, "startup-cmd", "", "run a shell command before the first prompt")
	flags.BoolVar(&options.init, "init", false, "run the guided interactive setup")
	setHeadless := func(value string) error {
		options.nonInteractive = value
		options.headless = true
		return nil
	}
	flags.Func("non-interactive", "run one task and exit", setHeadless)
	flags.Func("n", "run one task and exit", setHeadless)
	flags.StringVar(&options.resume, "resume", "", "resume a thread by ID")
	flags.StringVar(&options.resume, "r", "", "resume a thread by ID")
	flags.StringVar(&options.workingDir, "cwd", ".", "workspace directory")
	flags.StringVar(&options.stateDir, "state-dir", "", "state directory")
	flags.StringVar(&options.configPath, "config", "", "layered configuration file")
	flags.StringVar(&options.mcpConfigPath, "mcp-config", "", "explicit MCP server configuration file")
	flags.BoolVar(&options.trustProjectMCP, "trust-project-mcp", false, "trust project MCP servers for this run")
	flags.BoolVar(&options.yolo, "yolo", false, "run local actions without review")
	flags.BoolVar(&manualReview, "manual-review", false, "require user confirmation for gated actions")
	flags.BoolVar(&options.autoApprove, "approve-for-me", true, "review gated actions with a separate model")
	flags.BoolVar(&options.autoApprove, "auto-approve", true, "review gated actions with a separate model")
	flags.BoolVar(&options.autoApprove, "llm-approve", true, "review gated actions with a separate model")
	flags.BoolVar(&options.autoApprove, "y", true, "review gated actions with a separate model")
	flags.StringVar(&options.approvalModel, "approval-model", options.approvalModel, "model used to review gated actions")
	flags.StringVar(&rawShellAllowList, "shell-allow-list", "", "comma-separated shell commands, 'recommended', or 'all'")
	flags.StringVar(&rawShellAllowList, "S", "", "comma-separated shell commands, 'recommended', or 'all'")
	flags.BoolVar(&options.quiet, "quiet", false, "print only the final response in non-interactive mode")
	flags.BoolVar(&options.quiet, "q", false, "print only the final response in non-interactive mode")
	flags.BoolVar(&options.noStream, "no-stream", false, "buffer the response before writing it")
	flags.BoolVar(&options.json, "json", false, "write a JSON result in non-interactive mode")
	flags.BoolVar(&options.stdin, "stdin", false, "read the task from standard input")
	positiveInt := func(name string, assign func(int)) func(string) error {
		return func(value string) error {
			parsed, parseErr := strconv.Atoi(value)
			if parseErr != nil || parsed < 1 {
				return fmt.Errorf("%s must be a positive integer", name)
			}
			assign(parsed)
			return nil
		}
	}
	flags.Func("max-turns", "maximum agentic turns before exiting with status 124", positiveInt("--max-turns", func(value int) {
		options.maxTurns = value
	}))
	flags.Func("recursion-limit", "maximum graph steps per agent turn", positiveInt("--recursion-limit", func(value int) {
		options.recursionLimit = value
	}))
	flags.Func("rubric-max-iterations", "maximum rubric revision rounds", positiveInt("--rubric-max-iterations", func(value int) {
		options.rubricMax = value
	}))
	flags.BoolVar(&options.memoryAutoSave, "memory-auto-save", options.memoryAutoSave, "allow the agent to update its durable memory")
	flags.Func("timeout", "hard timeout in seconds before exiting with status 124", func(value string) error {
		seconds, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || seconds < 1 || seconds > int64((1<<63-1)/time.Second) {
			return fmt.Errorf("--timeout must be a positive integer")
		}
		options.timeout = time.Duration(seconds) * time.Second
		return nil
	})
	flags.BoolVar(&options.version, "version", false, "show version")
	flags.BoolVar(&options.version, "v", false, "show version")
	flags.BoolVar(&options.acp, "acp", false, "serve Agent Client Protocol over stdin and stdout")
	flags.BoolVar(&options.serveXtermJS, "serve-xtermjs", false, "serve the TUI through xterm.js")
	flags.StringVar(&options.xtermJSAddress, "xtermjs-address", defaultXtermJSAddress, "xterm.js loopback listen address")
	flags.StringVar(&options.sandbox, "sandbox", "", "remote sandbox provider (omit value to use configured default)")
	flags.StringVar(&options.sandboxID, "sandbox-id", "", "attach to an existing remote sandbox")
	flags.StringVar(&options.sandboxSnapshot, "sandbox-snapshot-name", "", "create from a provider snapshot or blueprint")
	flags.StringVar(&options.sandboxSetup, "sandbox-setup", "", "workspace-contained setup script uploaded to a new or attached sandbox")
	flags.StringVar(&options.localDevExecutable, "local-dev-server", "", "absolute executable path for a supervised local development server")
	flags.Func("local-dev-arg", "literal local development server argument (repeatable)", func(value string) error {
		options.localDevArguments = append(options.localDevArguments, value)
		return nil
	})
	flags.StringVar(&options.localDevEndpoint, "local-dev-endpoint", options.localDevEndpoint, "loopback HTTP origin for local development server readiness")
	flags.StringVar(&options.localDevHealthPath, "local-dev-health-path", options.localDevHealthPath, "absolute local development server readiness path")
	flags.Func("local-dev-inherit-env", "non-secret parent environment name to inherit (repeatable)", func(value string) error {
		options.localDevInheritEnvironment = append(options.localDevInheritEnvironment, value)
		return nil
	})
	flags.StringVar(&options.updateChannel, "update-channel", "", "trusted signed-update channel")
	flags.StringVar(&options.updateArtifact, "update-artifact", "", "trusted signed-update artifact")
	flags.StringVar(&options.updateManifestBase, "update-manifest-base", "", "trusted signed-update manifest base URL")
	flags.StringVar(&options.updatePublicKey, "update-public-key", "", "absolute Ed25519 update public-key path")
	flags.StringVar(&options.updateTarget, "update-target", "", "explicit update replacement target")
	flags.Usage = func() { printUsage(output) }
	if err := flags.Parse(arguments); err != nil {
		return cliOptions{}, err
	}
	explicit := map[string]bool{}
	flags.Visit(func(flag *flag.Flag) { explicit[flag.Name] = true })
	if err := validateLocalDevCLIOptions(options, explicit); err != nil {
		return cliOptions{}, err
	}
	if explicit["config"] && options.configPath == "" {
		return cliOptions{}, errors.New("--config requires a non-empty path")
	}
	if explicit["mcp-config"] && strings.TrimSpace(options.mcpConfigPath) == "" {
		return cliOptions{}, errors.New("--mcp-config requires a non-empty path")
	}
	if err := validateTUIUpdateOptions(options); err != nil {
		return cliOptions{}, err
	}
	if resolved != nil {
		applyResolvedCLIConfig(&options, &rawShellAllowList, explicit, *resolved)
		if resolved.store != nil {
			options.configPath = resolved.store.Path()
		}
	}
	if options.sandbox == "" && (options.sandboxID != "" || options.sandboxSnapshot != "" || options.sandboxSetup != "") {
		return cliOptions{}, errors.New("--sandbox-id, --sandbox-snapshot-name, and --sandbox-setup require --sandbox")
	}
	if options.sandboxID != "" && options.sandboxSnapshot != "" {
		return cliOptions{}, errors.New("--sandbox-id and --sandbox-snapshot-name are mutually exclusive")
	}
	if options.clearDefaultModel && options.defaultModel != "" {
		return cliOptions{}, errors.New("--default-model and --clear-default-model are mutually exclusive")
	}
	allowList, err := parseShellAllowList(rawShellAllowList)
	if err != nil {
		return cliOptions{}, err
	}
	options.shellAllowList = allowList
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
	if options.acp {
		var incompatible []string
		for _, option := range []struct {
			name    string
			enabled bool
		}{
			{name: "--message", enabled: options.message != ""},
			{name: "--skill", enabled: strings.TrimSpace(options.initialSkill) != ""},
			{name: "--startup-cmd", enabled: strings.TrimSpace(options.startupCommand) != ""},
			{name: "--init", enabled: options.init},
			{name: "--goal", enabled: strings.TrimSpace(options.goal) != ""},
			{name: "--rubric", enabled: strings.TrimSpace(options.rubric) != ""},
			{name: "--rubric-model", enabled: strings.TrimSpace(options.rubricModel) != ""},
			{name: "--rubric-max-iterations", enabled: options.rubricMax != 0},
			{name: "--non-interactive", enabled: options.headless},
			{name: "--resume", enabled: options.resume != "" || options.resumePicker},
			{name: "--serve-xtermjs", enabled: options.serveXtermJS},
			{name: "--stdin", enabled: options.stdin},
			{name: "--quiet", enabled: options.quiet},
			{name: "--no-stream", enabled: options.noStream},
			{name: "--json", enabled: options.json},
			{name: "--max-turns", enabled: options.maxTurns != 0},
			{name: "--timeout", enabled: options.timeout != 0},
			{name: "--local-dev-server", enabled: options.localDevExecutable != ""},
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
	if options.sandbox != "" {
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
	fmt.Fprintln(output, "  dacode config [COMMAND]          Inspect or manage layered configuration")
	fmt.Fprintln(output, "  dacode auth [COMMAND]            Inspect or manage stored credentials")
	fmt.Fprintln(output, "  dacode mcp login SERVER          Authorize a configured MCP server")
	fmt.Fprintln(output, "  dacode agents [list|reset]       List or reset named agent profiles")
	fmt.Fprintln(output, "  dacode doctor [OPTIONS]          Print offline installation diagnostics")
	fmt.Fprintln(output, "  dacode install NAME [OPTIONS]    Check an included Go integration")
	fmt.Fprintln(output, "  dacode update CHANNEL ARTIFACT   Check or apply a signed release")
	fmt.Fprintln(output, "  dacode -n 'Summarize README.md' Run one task and exit")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Options:")
	fmt.Fprintln(output, "  -r, --resume ID          Resume a thread")
	fmt.Fprintln(output, "  -M, --model MODEL        Model to use")
	fmt.Fprintln(output, "  --model-params JSON      Extra model constructor parameters")
	fmt.Fprintln(output, "  --profile-override JSON  Override runtime model profile fields")
	fmt.Fprintln(output, "  --max-retries N          Override transient model retries")
	fmt.Fprintln(output, "  --default-model [MODEL]  Show or set the default model")
	fmt.Fprintln(output, "  --clear-default-model    Clear the default model")
	fmt.Fprintln(output, "  -a, --agent NAME         Named agent profile to use")
	fmt.Fprintln(output, "  -m, --message TEXT       Initial prompt to submit")
	fmt.Fprintln(output, "  -s, --skill NAME         Invoke a project skill at startup")
	fmt.Fprintln(output, "  --startup-cmd CMD        Run a shell command before the first prompt")
	fmt.Fprintln(output, "  --init                   Run the guided interactive setup")
	fmt.Fprintln(output, "  -n, --non-interactive M  Run one task and exit")
	fmt.Fprintln(output, "  --cwd PATH               Workspace directory")
	fmt.Fprintln(output, "  --config PATH            Layered configuration file")
	fmt.Fprintln(output, "  --mcp-config PATH        Use one explicit MCP server configuration")
	fmt.Fprintln(output, "  --trust-project-mcp      Trust project MCP servers for this run")
	fmt.Fprintln(output, "  --sandbox [PROVIDER]     Use a registered remote sandbox; bare uses configured default")
	fmt.Fprintln(output, "  --sandbox-id ID          Attach without taking deletion ownership")
	fmt.Fprintln(output, "  --sandbox-snapshot-name NAME")
	fmt.Fprintln(output, "                           Create from a provider snapshot or blueprint")
	fmt.Fprintln(output, "  --sandbox-setup FILE     Upload and run a workspace-contained setup script")
	fmt.Fprintln(output, "  --local-dev-server PATH  Supervise an explicit local development server executable")
	fmt.Fprintln(output, "  --local-dev-arg VALUE    Pass one literal server argument; repeat as needed")
	fmt.Fprintln(output, "  --local-dev-endpoint URL Loopback HTTP readiness origin (default 127.0.0.1:2024)")
	fmt.Fprintln(output, "  --local-dev-health-path PATH")
	fmt.Fprintln(output, "                           Readiness path (default /ok)")
	fmt.Fprintln(output, "  --local-dev-inherit-env NAME")
	fmt.Fprintln(output, "                           Inherit one non-secret environment name")
	fmt.Fprintln(output, "  --update-channel NAME    Explicit trusted interactive update channel")
	fmt.Fprintln(output, "  --update-artifact NAME   Explicit trusted interactive update artifact")
	fmt.Fprintln(output, "  --update-manifest-base URL")
	fmt.Fprintln(output, "                           Explicit trusted HTTPS manifest directory")
	fmt.Fprintln(output, "  --update-public-key PATH Absolute Ed25519 release public-key path")
	fmt.Fprintln(output, "  --update-target PATH     Optional absolute replacement target")
	fmt.Fprintln(output, "  --manual-review          Require user confirmation for gated actions")
	fmt.Fprintln(output, "  --approval-model MODEL  Model used for automatic reviews (enabled by default)")
	fmt.Fprintln(output, "  -S, --shell-allow-list CMDS")
	fmt.Fprintln(output, "                           Auto-run only listed shell commands; use 'recommended' or 'all'")
	fmt.Fprintln(output, "  --yolo                   Run local actions without review")
	fmt.Fprintln(output, "  -q, --quiet              Clean one-shot output")
	fmt.Fprintln(output, "  --no-stream              Buffer the one-shot response")
	fmt.Fprintln(output, "  --json                   Write a versioned JSON result")
	fmt.Fprintln(output, "  --stdin                  Read the task from standard input")
	fmt.Fprintln(output, "  --max-turns N            Bound agentic turns (default 50)")
	fmt.Fprintln(output, "  --recursion-limit N      Bound graph steps per turn (default 2000)")
	fmt.Fprintln(output, "  --memory-auto-save       Allow durable memory updates (default true)")
	fmt.Fprintln(output, "  --timeout SECONDS        Bound wall-clock runtime")
	fmt.Fprintln(output, "  -v, --version            Show version")
	fmt.Fprintln(output, "  --acp                    Serve ACP over stdin and stdout")
	fmt.Fprintln(output, "  --serve-xtermjs          Serve the TUI through xterm.js")
	fmt.Fprintln(output, "  --xtermjs-address ADDR   Loopback listen address (default 127.0.0.1:0)")
	fmt.Fprintln(output, "  -h, --help               Show this help")
}

func runHeadless(ctx context.Context, runner agentRunner, workingDir, threadID, prompt string, options nonInteractiveOptions, stdout, stderr io.Writer) error {
	runCtx := ctx
	cancel := func() {}
	if options.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, options.Timeout)
	}
	defer cancel()
	err := runNonInteractive(runCtx, runner, workingDir, threadID, prompt, options, stdout, stderr)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		cancelCtx, cancelThread := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelThread()
		_ = runner.Cancel(cancelCtx, threadID)
		return &commandExitError{code: 124, err: fmt.Errorf("agent timed out after %s", options.Timeout)}
	}
	var limitError *headlessTurnLimitError
	if errors.As(err, &limitError) {
		return &commandExitError{code: 124, err: err}
	}
	return err
}

type headlessTurnLimitError struct{ limit int }

func (err *headlessTurnLimitError) Error() string {
	return fmt.Sprintf("exceeded %d agentic turns; increase --max-turns or split the task", err.limit)
}

func runNonInteractive(ctx context.Context, runner agentRunner, workingDir, threadID, prompt string, options nonInteractiveOptions, stdout, stderr io.Writer) error {
	var streamed strings.Builder
	transcript := "[user, trusted]\n" + prompt
	input := runInput{
		Config:   dacheckpoint.Config{ThreadID: threadID},
		Messages: []damessage.Message{damessage.Human(prompt)}, SkipValueEvents: true,
	}
	if options.Rubric != "" {
		input.State = dastate.Values{dagoapi.RubricKey: options.Rubric}
	}
	var result dagent.Result
	turnLimit := options.MaxTurns
	if turnLimit <= 0 {
		turnLimit = defaultHeadlessMaxTurns
	}
	turns := 0
	for {
		if turns >= turnLimit {
			return &headlessTurnLimitError{limit: turnLimit}
		}
		turns++
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
					if !options.Quiet && !options.NoStream && !options.JSON {
						fmt.Fprint(stdout, text)
					}
				}
			case dagent.EventUpdate:
				if messages, ok := event.Update[dagent.MessagesKey].([]damessage.Message); ok {
					transcript += reviewMessages(messages)
				}
			case dagent.EventToolProgress:
				if !options.Quiet && event.ToolProgress != nil && event.ToolProgress.Output != "" {
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
				input = runInput{
					Config:   dacheckpoint.Config{ThreadID: threadID},
					Messages: []damessage.Message{dagoal.ContinuationMessage(*goal)}, SkipValueEvents: true,
				}
				continue
			}
			break
		}
		if !options.AutoReview {
			return fmt.Errorf("action requires interactive approval; rerun interactively or remove --manual-review")
		}
		requests, decodeErr := decodeApprovalRequests(result.Interrupts[0].Value)
		if decodeErr != nil {
			return decodeErr
		}
		review, reviewErr := runner.Review(ctx, approvalReviewRequest{
			ThreadID: threadID, WorkingDir: workingDir, Transcript: transcript, Requests: requests,
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
			if !options.Quiet && !assessment.approved() {
				fmt.Fprintf(stderr, "[auto review] %s %s (risk: %s, authorization: %s): %s\n",
					"denied", request.Call.Name, assessment.RiskLevel, assessment.UserAuthorization, assessment.Rationale)
			}
		}
		input = runInput{
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
	if options.JSON {
		return json.NewEncoder(stdout).Encode(struct {
			Version  int    `json:"version"`
			ThreadID string `json:"thread_id"`
			Response string `json:"response"`
		}{Version: 1, ThreadID: threadID, Response: answer})
	}
	if options.Quiet {
		_, err := fmt.Fprintln(stdout, answer)
		return err
	}
	if options.NoStream {
		text := streamed.String()
		if text == "" {
			text = answer
		}
		fmt.Fprint(stdout, text)
	} else if streamed.Len() == 0 && answer != "" {
		fmt.Fprint(stdout, answer)
	}
	_, err := fmt.Fprintln(stdout)
	return err
}

func readHeadlessInput(stdin io.Reader, explicit bool) (string, bool, error) {
	piped := explicit
	if !piped {
		file, ok := stdin.(*os.File)
		if ok {
			info, err := file.Stat()
			if err != nil {
				return "", false, fmt.Errorf("inspect standard input: %w", err)
			}
			piped = info.Mode()&os.ModeCharDevice == 0
		}
	}
	if !piped {
		return "", false, nil
	}
	contents, err := io.ReadAll(stdin)
	if err != nil {
		return "", false, fmt.Errorf("read standard input: %w", err)
	}
	value := strings.TrimSpace(string(contents))
	if explicit && value == "" {
		return "", false, fmt.Errorf("--stdin was passed but standard input was empty")
	}
	return value, value != "", nil
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

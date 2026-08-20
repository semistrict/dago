//go:build dacode_e2e_fixture

package dacode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/daupdate"
)

type notificationUpdateTraceFixtureRunner struct{ *dagoRunner }

func newNotificationUpdateTraceFixtureRunner() *notificationUpdateTraceFixtureRunner {
	profile := damodel.Profile{Provider: "fixture", Model: "notifications", ContextWindow: 32_000}
	return &notificationUpdateTraceFixtureRunner{dagoRunner: &dagoRunner{
		profile: profile, effort: newReasoningEffortManager(profile, ""),
		agentState: &agentIdentity{name: defaultAgentName}, agentDefault: &agentIdentity{name: defaultAgentName},
	}}
}

func (*notificationUpdateTraceFixtureRunner) Start(ctx context.Context, _ runInput) eventStream {
	return &notificationUpdateTraceFixtureStream{ctx: ctx}
}

func (*notificationUpdateTraceFixtureRunner) Goal(context.Context, string) (*dagoal.Goal, error) {
	return nil, nil
}

func (*notificationUpdateTraceFixtureRunner) Rubric(context.Context, string) (dago.RubricSnapshot, error) {
	return dago.RubricSnapshot{}, nil
}

func (*notificationUpdateTraceFixtureRunner) RubricSettings() (string, int) {
	return "fixture:notifications", 3
}

func (*notificationUpdateTraceFixtureRunner) SetRubricModel(context.Context, string) error {
	return nil
}

func (*notificationUpdateTraceFixtureRunner) SetRubricMaxIterations(int) error { return nil }

type notificationUpdateTraceFixtureStream struct {
	ctx    context.Context
	done   bool
	result dagent.Result
}

func (stream *notificationUpdateTraceFixtureStream) Next(ctx context.Context) (dagent.Event, error) {
	if stream.done {
		return dagent.Event{}, io.EOF
	}
	select {
	case <-ctx.Done():
		return dagent.Event{}, ctx.Err()
	case <-stream.ctx.Done():
		return dagent.Event{}, stream.ctx.Err()
	case <-time.After(450 * time.Millisecond):
	}
	stream.done = true
	messages := []damessage.Message{damessage.Assistant("Offline fixture response.")}
	stream.result = dagent.Result{Messages: messages}
	return dagent.Event{Mode: dagent.EventUpdate, Update: dastate.Values{dagent.MessagesKey: messages}}, nil
}

func (stream *notificationUpdateTraceFixtureStream) Result(context.Context) (dagent.Result, error) {
	return stream.result, nil
}

func (*notificationUpdateTraceFixtureStream) Close() error { return nil }

type notificationUpdateTraceUpdateService struct {
	mode     string
	stateDir string
	mu       sync.Mutex
	checks   int
}

func (service *notificationUpdateTraceUpdateService) result() daupdate.Result {
	status := daupdate.UpdateAvailable
	latest := "v9.9.9"
	if service.mode == "current" {
		status, latest = daupdate.UpToDate, "v1.0.0"
	}
	return daupdate.Result{
		Status: status, Channel: "fixture-stable", Artifact: "fixture-binary",
		CurrentVersion: "v1.0.0", LatestVersion: latest, Verified: true,
	}
}

func (service *notificationUpdateTraceUpdateService) Check(ctx context.Context, _ string) (daupdate.Result, error) {
	service.mu.Lock()
	service.checks++
	check := service.checks
	service.mu.Unlock()
	if service.mode == "slow-check" {
		select {
		case <-ctx.Done():
			return daupdate.Result{}, ctx.Err()
		case <-time.After(800 * time.Millisecond):
		}
	}
	if service.mode == "retry" && check == 1 {
		return daupdate.Result{}, daupdate.ErrUpdateCheckFailed
	}
	return service.result(), nil
}

func (service *notificationUpdateTraceUpdateService) DryRun(ctx context.Context, current string) (daupdate.Result, error) {
	return service.Check(ctx, current)
}

func (service *notificationUpdateTraceUpdateService) Apply(ctx context.Context, _, _ string, authorization daupdate.Authorization) (daupdate.Result, error) {
	if authorization != daupdate.AuthorizationGranted {
		return daupdate.Result{}, daupdate.ErrAuthorization
	}
	result := service.result()
	if service.mode == "apply-fail" {
		return result, daupdate.ErrArtifactMismatch
	}
	if service.mode == "slow-apply" || service.mode == "shared-lock" {
		lock := filepath.Join(service.stateDir, "fixture-update.lock")
		file, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return result, context.DeadlineExceeded
		}
		_ = file.Close()
		defer os.Remove(lock)
		select {
		case <-ctx.Done():
			if service.mode != "slow-apply" {
				return result, ctx.Err()
			}
			// Activation already completed despite the UI cancellation.
		case <-time.After(700 * time.Millisecond):
		}
	}
	result.Applied = true
	return result, nil
}

type notificationUpdateTraceResolver struct{ mode string }

func (resolver notificationUpdateTraceResolver) ThreadURL(ctx context.Context, project, threadID string) (string, error) {
	if resolver.mode == "trace-timeout" {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if resolver.mode == "trace-fail" {
		return "", errors.New("fixture secret trace failure")
	}
	if resolver.mode == "trace-unsafe" {
		return "https://fixture-user:" + "fixture-secret@github.com/semistrict/dago/actions", nil
	}
	return "https://github.com/semistrict/dago/actions?project=" + project + "&thread=" + threadID, nil
}

// RunNotificationUpdateTraceFixture runs the offline browser fixture for the
// notification, trace, and signed-update interaction contracts.
func RunNotificationUpdateTraceFixture(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
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
	mode := strings.TrimSpace(os.Getenv("DACODE_FIXTURE_NOTIFY_MODE"))
	runner := newNotificationUpdateTraceFixtureRunner()
	threadID := "fixture-thread"
	if os.Getenv("DACODE_FIXTURE_ASCII") == "1" {
		threadID = "t"
	}
	model := newTUIModel(ctx, runner, "/", "f:m", threadID, false, true, "")
	if os.Getenv("DACODE_FIXTURE_ASCII") == "1" {
		model.glyphs = asciiUIGlyphs
	}

	configurationPath := filepath.Join(options.stateDir, "fixture-preferences.toml")
	if mode == "auto-unwritable" {
		configurationPath = os.DevNull
	}
	if mode == "preference-fail" {
		_ = os.WriteFile(configurationPath, []byte("[warnings]\nsuppress = 7\n"), 0o600)
	} else if mode == "auto-malformed" {
		_ = os.WriteFile(configurationPath, []byte("[update\nauto_update = true\n"), 0o600)
	} else if mode == "auto-symlink" {
		target := filepath.Join(options.stateDir, "fixture-preferences-target.toml")
		_ = os.WriteFile(target, []byte("[update]\nauto_update = false\n"), 0o600)
		_ = os.Symlink(target, configurationPath)
	}
	model.notificationStore = newNotificationPreferenceStore(configurationPath)
	suppressed, _ := loadSuppressedWarnings(configurationPath)
	model.addStartupNotification(newPendingNotification(
		"warning:ripgrep", "ripgrep is not installed", "Use the copied command to install ripgrep.",
		missingDependencyNotification{Tool: warningRipgrep, InstallCommand: "echo notification-copy-sentinel", URL: "https://github.com/BurntSushi/ripgrep"},
		notificationAction{ID: notificationCopyInstall, Label: "Copy install command", Primary: true},
		notificationAction{ID: notificationOpenWebsite, Label: "Open installation guide"},
		notificationAction{ID: notificationSuppress, Label: "Hide this warning"},
	))
	model.configureStartupNotifications(suppressed, true, false, true)
	model.toasts.add("Generic fixture notice", toastInfo, 0, "", time.Now())

	manager := newAuthManager(
		dacredential.NewStore(filepath.Join(options.stateDir, "fixture-auth.json"), time.Now, dacredential.Options{}),
		func(string) (string, bool) { return "", false },
	)
	model.authManager = newAuthTUIController(manager, filepath.Join(options.stateDir, "fixture-oauth.json"), fixtureSubscriptionLogin(""), func(string) error { return nil })

	traceMode := mode
	if !strings.HasPrefix(traceMode, "trace-") {
		traceMode = "trace-ok"
	}
	if traceMode == "trace-unconfigured" {
		model.configureTrace(newTraceCommand(nil), "")
	} else {
		model.configureTrace(newTraceCommand(notificationUpdateTraceResolver{mode: traceMode}), "fixture-project")
	}

	preferencePath := configurationPath
	preference, _ := loadAutoUpdatePreference(preferencePath, os.LookupEnv)
	stateStore := newUpdateStateStore(filepath.Join(options.stateDir, "fixture-update-state.json"))
	persistent, stateErr := stateStore.Load(ctx)
	if stateErr != nil {
		persistent = newUpdatePersistentState()
	}
	var profile *tuiUpdateProfile
	if strings.HasPrefix(mode, "update-") {
		updateMode := strings.TrimPrefix(mode, "update-")
		profile = &tuiUpdateProfile{service: &notificationUpdateTraceUpdateService{mode: updateMode, stateDir: options.stateDir}, current: "v1.0.0", target: filepath.Join(options.stateDir, "fixture-target")}
		if updateMode == "windows" {
			profile.target = ""
			profile.operatingSystem = "windows"
		}
	}
	model.configureUpdates(profile, newAutoUpdatePreferenceStore(preferencePath), stateStore, preference, persistent)

	program := tea.NewProgram(newProgramModel(model), tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout))
	_, err = program.Run()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

var _ agentRunner = (*notificationUpdateTraceFixtureRunner)(nil)
var _ updateService = (*notificationUpdateTraceUpdateService)(nil)

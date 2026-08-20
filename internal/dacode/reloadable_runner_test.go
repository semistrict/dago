package dacode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/semistrict/dago/dacost"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dahook"
	"github.com/semistrict/dago/damcp"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daskill"
	talonmcp "github.com/semistrict/dago/datalon/mcp"
	"github.com/semistrict/dago/datalon/mcp/oauthpolicy"
)

type reloadTestRunner struct {
	agentRunner
	name    string
	trusted string
}

func TestMCPDisableMutationSerializesWithReconnectAndRemainsPending(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	resolution := mcpConfigResolution{Connections: []damcp.Connection{{Name: "server", Transport: "stdio"}}}
	bundle := &configuredMCPBundle{Servers: []configuredMCPServerInfo{{Name: "server", Transport: "stdio"}}}
	reload := newReloadableRunner(reloadableRuntimeBuild{
		runner: &reloadTestRunner{name: "initial"}, closer: &countingCloser{}, mcp: resolution, mcpBundle: bundle,
	}, newPluginManagerService(t.TempDir(), nil), func(context.Context, map[string]bool) (reloadableRuntimeBuild, error) {
		once.Do(func() { close(started) })
		<-release
		return reloadableRuntimeBuild{runner: &reloadTestRunner{name: "next"}, closer: &countingCloser{}, mcp: resolution, mcpBundle: bundle}, nil
	}, newHookUISink())
	reload.configureMCPRuntime(resolution, bundle, t.TempDir(), func(context.Context, *http.Client, string, talonmcp.Server, talonmcp.TokenStore, talonmcp.Interaction, oauthpolicy.Options) error {
		return nil
	})
	reconnectDone := make(chan error, 1)
	go func() { reconnectDone <- reload.ReconnectMCP(t.Context()) }()
	<-started
	toggleDone := make(chan error, 1)
	go func() { toggleDone <- reload.ToggleMCPDisabled(t.Context(), "server") }()
	select {
	case err := <-toggleDone:
		t.Fatalf("toggle raced through reconnect: %v", err)
	default:
	}
	close(release)
	if err := <-reconnectDone; err != nil {
		t.Fatal(err)
	}
	if err := <-toggleDone; err != nil {
		t.Fatal(err)
	}
	servers, pending, err := reload.SnapshotMCP()
	if err != nil || !pending || len(servers) != 1 || servers[0].Status != mcpViewerDisabled {
		t.Fatalf("snapshot = %#v pending=%t err=%v", servers, pending, err)
	}
	_ = reload.Close()
}

func (runner *reloadTestRunner) Start(context.Context, runInput) eventStream {
	return &reloadTestStream{}
}
func (runner *reloadTestRunner) Profile() damodel.Profile { return damodel.Profile{Model: runner.name} }
func (runner *reloadTestRunner) AgentName() string        { return runner.name }
func (runner *reloadTestRunner) ListSkills(context.Context) ([]daskill.Entry, error) {
	return []daskill.Entry{{Skill: daskill.Skill{Name: runner.name + "-skill"}}}, nil
}
func (runner *reloadTestRunner) LoadSkill(_ context.Context, name string) (daskill.Entry, error) {
	return daskill.Entry{Skill: daskill.Skill{Name: name}}, nil
}
func (runner *reloadTestRunner) TrustSkill(_ context.Context, target string) error {
	runner.trusted = target
	return nil
}
func (runner *reloadTestRunner) CostReport(context.Context, string) (dacost.Report, error) {
	return dacost.Report{CostUSD: float64(len(runner.name))}, nil
}
func (*reloadTestRunner) CostPricingError() error { return nil }

type reloadTestStream struct{ closed atomic.Bool }

func (*reloadTestStream) Next(context.Context) (dagent.Event, error) { return dagent.Event{}, io.EOF }
func (*reloadTestStream) Result(context.Context) (dagent.Result, error) {
	return dagent.Result{}, nil
}
func (stream *reloadTestStream) Close() error { stream.closed.Store(true); return nil }

type countingCloser struct{ calls atomic.Int32 }

func (closer *countingCloser) Close() error { closer.calls.Add(1); return nil }

func TestReloadableRunnerSwapsOnlyWhileIdleAndKeepsOldOnFailure(t *testing.T) {
	initialCloser, replacementCloser := &countingCloser{}, &countingCloser{}
	hooks := newHookUISink()
	service := newPluginManagerService(t.TempDir(), nil)
	fail := atomic.Bool{}
	reload := newReloadableRunner(reloadableRuntimeBuild{
		runner: &reloadTestRunner{name: "initial"}, closer: initialCloser, loadedIDs: []string{"old@local"},
	}, service, func(ctx context.Context, _ map[string]bool) (reloadableRuntimeBuild, error) {
		if err := ctx.Err(); err != nil {
			return reloadableRuntimeBuild{}, err
		}
		if fail.Load() {
			return reloadableRuntimeBuild{}, errors.New("build failed")
		}
		return reloadableRuntimeBuild{runner: &reloadTestRunner{name: "replacement"}, closer: replacementCloser, loadedIDs: []string{"new@local"}}, nil
	}, hooks)

	stream := reload.Start(t.Context(), runInput{})
	if _, err := reload.ReloadPlugins(t.Context()); err == nil || reload.AgentName() != "initial" {
		t.Fatalf("busy reload changed runtime: name=%q err=%v", reload.AgentName(), err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := reload.ReloadPlugins(t.Context())
	if err != nil || reload.AgentName() != "replacement" || initialCloser.calls.Load() != 1 || len(result.Added) != 1 || result.Added[0] != "new@local" || len(result.Removed) != 1 || result.Removed[0] != "old@local" {
		t.Fatalf("reload = %#v name=%q old-close=%d err=%v", result, reload.AgentName(), initialCloser.calls.Load(), err)
	}
	entries, skillErr := reload.ListSkills(t.Context())
	loaded, loadErr := reload.LoadSkill(t.Context(), "requested")
	trustErr := reload.TrustSkill(t.Context(), "/trusted")
	report, costErr := reload.CostReport(t.Context(), "thread")
	current := reload.current().(*reloadTestRunner)
	if skillErr != nil || len(entries) != 1 || entries[0].Skill.Name != "replacement-skill" || loadErr != nil || loaded.Skill.Name != "requested" || trustErr != nil || current.trusted != "/trusted" || costErr != nil || report.CostUSD != float64(len("replacement")) {
		t.Fatalf("optional capabilities = entries:%#v loaded:%#v trusted:%q report:%#v errors:%v/%v/%v/%v", entries, loaded, current.trusted, report, skillErr, loadErr, trustErr, costErr)
	}
	fail.Store(true)
	if _, err := reload.ReloadPlugins(t.Context()); err == nil || reload.AgentName() != "replacement" || replacementCloser.calls.Load() != 0 {
		t.Fatalf("failed reload did not preserve runtime: name=%q close=%d err=%v", reload.AgentName(), replacementCloser.calls.Load(), err)
	}
	if err := reload.Close(); err != nil || replacementCloser.calls.Load() != 1 {
		t.Fatalf("close = %v calls=%d", err, replacementCloser.calls.Load())
	}
	if err := reload.Close(); err != nil || replacementCloser.calls.Load() != 1 {
		t.Fatalf("second close = %v calls=%d", err, replacementCloser.calls.Load())
	}
}

func TestReloadableRunnerKeepsStableHookStatusAndHonorsCancellation(t *testing.T) {
	hooks := newHookUISink()
	service := newPluginManagerService(t.TempDir(), nil)
	reload := newReloadableRunner(reloadableRuntimeBuild{
		runner: &reloadTestRunner{name: "initial"}, closer: &countingCloser{},
	}, service, func(ctx context.Context, _ map[string]bool) (reloadableRuntimeBuild, error) {
		<-ctx.Done()
		return reloadableRuntimeBuild{}, ctx.Err()
	}, hooks)
	hooks.Publish(dahook.Progress{OperationID: "hook", Event: dahook.SessionStart, Active: true, Message: "Loading session"})
	update, err := reload.NextHookStatus(t.Context())
	if err != nil || update.Status != "Loading session" {
		t.Fatalf("hook status = %#v, %v", update, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reload.ReloadPlugins(ctx); !errors.Is(err, context.Canceled) || reload.AgentName() != "initial" {
		t.Fatalf("cancelled reload = %v name=%q", err, reload.AgentName())
	}
	_ = reload.Close()
}

func TestReloadableRunnerRollsBackSideEffectsFromIncompleteBuild(t *testing.T) {
	rollbackCalls := atomic.Int32{}
	closer := &countingCloser{}
	reload := newReloadableRunner(reloadableRuntimeBuild{
		runner: &reloadTestRunner{name: "initial"}, closer: closer,
	}, newPluginManagerService(t.TempDir(), nil), func(context.Context, map[string]bool) (reloadableRuntimeBuild, error) {
		return reloadableRuntimeBuild{closer: &countingCloser{}, rollback: func() { rollbackCalls.Add(1) }}, nil
	}, newHookUISink())
	if _, err := reload.ReloadPlugins(t.Context()); err == nil {
		t.Fatal("incomplete build was accepted")
	}
	if rollbackCalls.Load() != 1 || reload.AgentName() != "initial" {
		t.Fatalf("rollback calls=%d current=%q", rollbackCalls.Load(), reload.AgentName())
	}
	_ = reload.Close()
}

package dacode

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dacost"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daskill"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/daworkflow"
)

type reloadableRuntimeBuild struct {
	runner    agentRunner
	closer    io.Closer
	loadedIDs []string
	warnings  []string
	changes   []string
	rollback  func()
	mcp       mcpConfigResolution
	mcpBundle *configuredMCPBundle
}

type runtimeReloadFactory func(context.Context, map[string]bool) (reloadableRuntimeBuild, error)

type pluginReloadResult struct {
	Loaded   []string
	Added    []string
	Removed  []string
	Warnings []string
	Changes  []string
}

type reloadableRunner struct {
	mu       sync.RWMutex
	reloadMu sync.Mutex
	runner   agentRunner
	closer   io.Closer
	loaded   []string
	active   int
	closed   bool

	plugins *pluginManagerService
	factory runtimeReloadFactory
	hooks   *hookUISink

	mcpResolution mcpConfigResolution
	mcpBundle     *configuredMCPBundle
	mcpDisabled   map[string]bool
	mcpPending    bool
	mcpTokenDir   string
	mcpLogin      mcpRuntimeLogin
}

// newReloadableRunner constructs the interactive runtime swap boundary. Every
// dependency is positional because plugin mutations, cleanup, and hook status
// continuity are required parts of a safe reload.
func newReloadableRunner(initial reloadableRuntimeBuild, plugins *pluginManagerService, factory runtimeReloadFactory, hooks *hookUISink) *reloadableRunner {
	if initial.runner == nil || initial.closer == nil || plugins == nil || factory == nil || hooks == nil {
		panic("dacode: complete reloadable runtime dependencies are required")
	}
	return &reloadableRunner{
		runner: initial.runner, closer: initial.closer, loaded: sortedPluginIDs(initial.loadedIDs),
		plugins: plugins, factory: factory, hooks: hooks,
	}
}

func (runner *reloadableRunner) configureMCPRuntime(resolution mcpConfigResolution, bundle *configuredMCPBundle, tokenDirectory string, login mcpRuntimeLogin) {
	if runner == nil || bundle == nil || tokenDirectory == "" || login == nil {
		panic("dacode: complete MCP runtime dependencies are required")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.mcpResolution = resolution
	runner.mcpBundle = bundle
	runner.mcpDisabled = make(map[string]bool)
	runner.mcpTokenDir = tokenDirectory
	runner.mcpLogin = login
}

func (runner *reloadableRunner) PluginSnapshot(ctx context.Context) (pluginManagerSnapshot, error) {
	runner.mu.RLock()
	loaded := append([]string(nil), runner.loaded...)
	closed := runner.closed
	runner.mu.RUnlock()
	if closed {
		return pluginManagerSnapshot{}, errors.New("plugin runtime is closed")
	}
	return runner.plugins.Snapshot(ctx, loaded)
}

func (runner *reloadableRunner) InstallPlugin(ctx context.Context, id string) error {
	return runner.plugins.Install(ctx, id)
}
func (runner *reloadableRunner) SetPluginEnabled(ctx context.Context, id string, enabled bool) error {
	return runner.plugins.SetEnabled(ctx, id, enabled)
}
func (runner *reloadableRunner) UninstallPlugin(ctx context.Context, id string) error {
	return runner.plugins.Uninstall(ctx, id)
}
func (runner *reloadableRunner) AddPluginMarketplace(ctx context.Context, source string) error {
	return runner.plugins.AddMarketplace(ctx, source)
}
func (runner *reloadableRunner) RemovePluginMarketplace(ctx context.Context, name string) error {
	return runner.plugins.RemoveMarketplace(ctx, name)
}

func (runner *reloadableRunner) ReloadPlugins(ctx context.Context) (pluginReloadResult, error) {
	runner.reloadMu.Lock()
	defer runner.reloadMu.Unlock()
	runner.mu.RLock()
	if runner.closed {
		runner.mu.RUnlock()
		return pluginReloadResult{}, errors.New("plugin runtime is closed")
	}
	if runner.active != 0 {
		runner.mu.RUnlock()
		return pluginReloadResult{}, errors.New("plugin runtime is busy")
	}
	previous := append([]string(nil), runner.loaded...)
	disabled := cloneMCPDisabled(runner.mcpDisabled)
	runner.mu.RUnlock()

	built, err := runner.factory(ctx, disabled)
	if err != nil {
		return pluginReloadResult{}, err
	}
	if built.runner == nil || built.closer == nil {
		if built.rollback != nil {
			built.rollback()
		}
		if built.closer != nil {
			_ = built.closer.Close()
		}
		return pluginReloadResult{}, errors.New("reload factory returned an incomplete runtime")
	}
	built.loadedIDs = sortedPluginIDs(built.loadedIDs)
	runner.mu.Lock()
	if runner.closed || runner.active != 0 {
		runner.mu.Unlock()
		if built.rollback != nil {
			built.rollback()
		}
		_ = built.closer.Close()
		return pluginReloadResult{}, errors.New("plugin runtime changed while reloading")
	}
	oldCloser := runner.closer
	runner.runner, runner.closer, runner.loaded = built.runner, built.closer, built.loadedIDs
	if built.mcpBundle != nil {
		runner.mcpResolution, runner.mcpBundle = built.mcp, built.mcpBundle
		runner.mcpPending = false
	}
	runner.mu.Unlock()

	result := pluginReloadResult{
		Loaded: append([]string(nil), built.loadedIDs...), Warnings: stablePluginManagerWarnings(built.warnings),
		Changes: append([]string(nil), built.changes...),
	}
	result.Added, result.Removed = pluginIDDifference(built.loadedIDs, previous), pluginIDDifference(previous, built.loadedIDs)
	if err := oldCloser.Close(); err != nil {
		result.Warnings = append(result.Warnings, "previous plugin runtime cleanup was incomplete")
	}
	return result, nil
}

func (runner *reloadableRunner) Start(ctx context.Context, input dagent.Input) eventStream {
	runner.mu.Lock()
	if runner.closed || runner.runner == nil {
		runner.mu.Unlock()
		return &errorEventStream{err: errors.New("agent runtime is closed")}
	}
	current := runner.runner
	runner.active++
	runner.mu.Unlock()
	stream := current.Start(ctx, input)
	if stream == nil {
		runner.streamDone()
		return &errorEventStream{err: errors.New("agent stream is unavailable")}
	}
	return &reloadTrackedStream{eventStream: stream, owner: runner}
}

func (runner *reloadableRunner) streamDone() {
	runner.mu.Lock()
	if runner.active > 0 {
		runner.active--
	}
	runner.mu.Unlock()
}

type reloadTrackedStream struct {
	eventStream
	owner    *reloadableRunner
	once     sync.Once
	closeErr error
}

func (stream *reloadTrackedStream) Close() error {
	stream.once.Do(func() {
		stream.closeErr = stream.eventStream.Close()
		stream.owner.streamDone()
	})
	return stream.closeErr
}

type errorEventStream struct{ err error }

func (stream *errorEventStream) Next(context.Context) (dagent.Event, error) {
	return dagent.Event{}, stream.err
}
func (stream *errorEventStream) Result(context.Context) (dagent.Result, error) {
	return dagent.Result{}, stream.err
}
func (*errorEventStream) Close() error { return nil }

func (runner *reloadableRunner) Close() error {
	runner.reloadMu.Lock()
	defer runner.reloadMu.Unlock()
	runner.mu.Lock()
	if runner.closed {
		runner.mu.Unlock()
		return nil
	}
	runner.closed = true
	closer := runner.closer
	runner.mu.Unlock()
	return closer.Close()
}

func (runner *reloadableRunner) current() agentRunner {
	runner.mu.RLock()
	defer runner.mu.RUnlock()
	if runner.closed {
		return nil
	}
	return runner.runner
}

func (runner *reloadableRunner) NextHookStatus(ctx context.Context) (hookStatusUpdate, error) {
	return runner.hooks.Next(ctx)
}

func (runner *reloadableRunner) Profile() damodel.Profile {
	if current := runner.current(); current != nil {
		return current.Profile()
	}
	return damodel.Profile{}
}
func (runner *reloadableRunner) ReasoningEffort() reasoningEffortContext {
	if current := runner.current(); current != nil {
		return current.ReasoningEffort()
	}
	return reasoningEffortContext{}
}
func (runner *reloadableRunner) SetReasoningEffort(value string) error {
	if current := runner.current(); current != nil {
		return current.SetReasoningEffort(value)
	}
	return errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) Tools() []datool.Definition {
	if current := runner.current(); current != nil {
		return current.Tools()
	}
	return nil
}
func (runner *reloadableRunner) AgentName() string {
	if current := runner.current(); current != nil {
		return current.AgentName()
	}
	return ""
}
func (runner *reloadableRunner) ListAgents(ctx context.Context) ([]agentInfo, error) {
	if current := runner.current(); current != nil {
		return current.ListAgents(ctx)
	}
	return nil, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) SwitchAgent(ctx context.Context, name string) error {
	if current := runner.current(); current != nil {
		return current.SwitchAgent(ctx, name)
	}
	return errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) SetDefaultAgent(ctx context.Context, name string) (string, error) {
	if current := runner.current(); current != nil {
		return current.SetDefaultAgent(ctx, name)
	}
	return "", errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) Cancel(ctx context.Context, threadID string) error {
	if current := runner.current(); current != nil {
		return current.Cancel(ctx, threadID)
	}
	return errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) Review(ctx context.Context, request approvalReviewRequest) (approvalReviewResult, error) {
	if current := runner.current(); current != nil {
		return current.Review(ctx, request)
	}
	return approvalReviewResult{}, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) ListSessions(ctx context.Context) ([]sessionInfo, error) {
	if current := runner.current(); current != nil {
		return current.ListSessions(ctx)
	}
	return nil, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) SessionMetadata(ctx context.Context, id string) (sessionInfo, error) {
	if current := runner.current(); current != nil {
		reader, ok := current.(sessionMetadataReader)
		if !ok {
			return sessionInfo{}, errors.New("agent runtime does not expose session metadata")
		}
		return reader.SessionMetadata(ctx, id)
	}
	return sessionInfo{}, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) LoadSession(ctx context.Context, id string) ([]damessage.Message, error) {
	if current := runner.current(); current != nil {
		return current.LoadSession(ctx, id)
	}
	return nil, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) DeleteSession(ctx context.Context, id, checkpointID, revision string) error {
	if current := runner.current(); current != nil {
		deleter, ok := current.(threadSessionDeleter)
		if !ok {
			return errors.New("agent runtime does not support session deletion")
		}
		return deleter.DeleteSession(ctx, id, checkpointID, revision)
	}
	return errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) LoadSessionCheckpoint(ctx context.Context, id, checkpointID string) ([]damessage.Message, error) {
	if current := runner.current(); current != nil {
		loader, ok := current.(sessionCheckpointLoader)
		if !ok {
			return nil, errors.New("agent runtime does not support exact-checkpoint loading")
		}
		return loader.LoadSessionCheckpoint(ctx, id, checkpointID)
	}
	return nil, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) CompactSession(ctx context.Context, id, checkpointID string) (sessionCompactionResult, error) {
	if current := runner.current(); current != nil {
		compactor, ok := current.(sessionCompactor)
		if !ok {
			return sessionCompactionResult{}, errors.New("agent runtime does not support conversation compaction")
		}
		return compactor.CompactSession(ctx, id, checkpointID)
	}
	return sessionCompactionResult{}, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) Goal(ctx context.Context, id string) (*dagoal.Goal, error) {
	if current := runner.current(); current != nil {
		return current.Goal(ctx, id)
	}
	return nil, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) SetGoal(ctx context.Context, id string, request dagoal.SetRequest) (*dagoal.Goal, error) {
	if current := runner.current(); current != nil {
		return current.SetGoal(ctx, id, request)
	}
	return nil, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) ClearGoal(ctx context.Context, id string) (bool, error) {
	if current := runner.current(); current != nil {
		return current.ClearGoal(ctx, id)
	}
	return false, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) StartWorkflow(ctx context.Context, request daworkflow.StartRequest) (daworkflow.Status, error) {
	current, ok := runner.current().(workflowRunner)
	if !ok {
		return daworkflow.Status{}, errors.New("workflow runtime is unavailable")
	}
	return current.StartWorkflow(ctx, request)
}
func (runner *reloadableRunner) Workflows() []daworkflow.Status {
	current, ok := runner.current().(workflowRunner)
	if !ok {
		return nil
	}
	return current.Workflows()
}
func (runner *reloadableRunner) RunningWorkflows() int {
	current, ok := runner.current().(workflowRunner)
	if !ok {
		return 0
	}
	return current.RunningWorkflows()
}
func (runner *reloadableRunner) CancelWorkflow(runID string) bool {
	current, ok := runner.current().(workflowRunner)
	return ok && current.CancelWorkflow(runID)
}
func (runner *reloadableRunner) WaitWorkflowCompletion(ctx context.Context) (daworkflow.Status, bool) {
	current, ok := runner.current().(workflowRunner)
	if !ok {
		return daworkflow.Status{}, false
	}
	return current.WaitWorkflowCompletion(ctx)
}
func (runner *reloadableRunner) DraftGoalCriteria(ctx context.Context, id string, request dagoal.CriteriaRequest) (dagoal.CriteriaProposal, error) {
	if current := runner.current(); current != nil {
		return current.DraftGoalCriteria(ctx, id, request)
	}
	return dagoal.CriteriaProposal{}, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) Rubric(ctx context.Context, id string) (dago.RubricSnapshot, error) {
	if current := runner.current(); current != nil {
		return current.Rubric(ctx, id)
	}
	return dago.RubricSnapshot{}, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) SetRubric(ctx context.Context, id, criteria string) (dago.RubricSnapshot, error) {
	if current := runner.current(); current != nil {
		return current.SetRubric(ctx, id, criteria)
	}
	return dago.RubricSnapshot{}, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) ClearRubric(ctx context.Context, id string) (bool, error) {
	if current := runner.current(); current != nil {
		return current.ClearRubric(ctx, id)
	}
	return false, errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) RubricSettings() (string, int) {
	if current := runner.current(); current != nil {
		return current.RubricSettings()
	}
	return "", 0
}
func (runner *reloadableRunner) SetRubricModel(ctx context.Context, value string) error {
	if current := runner.current(); current != nil {
		return current.SetRubricModel(ctx, value)
	}
	return errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) SetRubricMaxIterations(value int) error {
	if current := runner.current(); current != nil {
		return current.SetRubricMaxIterations(value)
	}
	return errors.New("agent runtime is closed")
}
func (runner *reloadableRunner) CostReport(ctx context.Context, id string) (dacost.Report, error) {
	current := runner.current()
	reporter, ok := current.(agentCostReporter)
	if !ok {
		return dacost.Report{}, errors.New("cost reporting is unavailable")
	}
	return reporter.CostReport(ctx, id)
}
func (runner *reloadableRunner) CostPricingError() error {
	current := runner.current()
	reporter, ok := current.(agentCostReporter)
	if !ok {
		return nil
	}
	return reporter.CostPricingError()
}
func (runner *reloadableRunner) ListSkills(ctx context.Context) ([]daskill.Entry, error) {
	current := runner.current()
	capability, ok := current.(skillRunner)
	if !ok {
		return nil, errors.New("skills are unavailable")
	}
	return capability.ListSkills(ctx)
}
func (runner *reloadableRunner) LoadSkill(ctx context.Context, name string) (daskill.Entry, error) {
	current := runner.current()
	capability, ok := current.(skillRunner)
	if !ok {
		return daskill.Entry{}, errors.New("skills are unavailable")
	}
	return capability.LoadSkill(ctx, name)
}
func (runner *reloadableRunner) TrustSkill(ctx context.Context, target string) error {
	current := runner.current()
	capability, ok := current.(skillRunner)
	if !ok {
		return errors.New("skills are unavailable")
	}
	return capability.TrustSkill(ctx, target)
}

func sortedPluginIDs(input []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(input))
	for _, id := range input {
		if id != "" && !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result
}

func pluginIDDifference(oldIDs, newIDs []string) []string {
	newSet := make(map[string]bool, len(newIDs))
	for _, id := range newIDs {
		newSet[id] = true
	}
	var result []string
	for _, id := range oldIDs {
		if !newSet[id] {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result
}

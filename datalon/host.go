package datalon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

var errStoppedByCommand = errors.New("stopped by channel command")

type conversation struct {
	gate       chan struct{}
	mu         sync.Mutex
	generation uint64
	waiting    int
	active     map[uint64]context.CancelCauseFunc
	next       uint64
}

// Host owns one runtime, its channels, and an optional scheduler.
type Host struct {
	runtime   Runtime
	config    Config
	channels  []Channel
	scheduler Scheduler

	lifecycle sync.Mutex
	running   bool
	root      context.Context
	cancel    context.CancelCauseFunc
	started   []Channel

	conversations sync.Map
	approvals     approvalRegistry
	work          sync.WaitGroup
}

// NewHost constructs a host. A nil runtime selects EchoRuntime. Static negative
// bounds and nil channels panic; operational configuration is validated when
// Start is called.
func NewHost(runtime Runtime, config Config, channels ...Channel) *Host {
	config.validateStaticBounds()
	if nilValue(runtime) {
		runtime = EchoRuntime{}
	}
	cloned := append([]Channel(nil), channels...)
	for _, channel := range cloned {
		if nilValue(channel) {
			panic("datalon: nil channel")
		}
	}
	return &Host{runtime: runtime, config: config, channels: cloned}
}

// NewScheduledHost constructs a host with a scheduler. The scheduler is a
// required positional dependency and must not be nil.
func NewScheduledHost(runtime Runtime, scheduler Scheduler, config Config, channels ...Channel) *Host {
	if nilValue(scheduler) {
		panic("datalon: nil scheduler")
	}
	host := NewHost(runtime, config, channels...)
	host.scheduler = scheduler
	return host
}

// Config returns the normalized active configuration after Start, or the
// configured values before Start.
func (host *Host) Config() Config {
	host.lifecycle.Lock()
	defer host.lifecycle.Unlock()
	return host.config
}

// Running reports whether the host currently owns its managed components.
func (host *Host) Running() bool {
	host.lifecycle.Lock()
	defer host.lifecycle.Unlock()
	return host.running
}

// Start prepares state, starts the runtime, then channels, then the scheduler.
// A partial failure rolls already-started components back in reverse order.
func (host *Host) Start(ctx context.Context) error {
	host.lifecycle.Lock()
	defer host.lifecycle.Unlock()
	if host.running {
		return nil
	}
	config, err := host.config.prepare()
	if err != nil {
		return err
	}
	if err := validateChannels(host.channels); err != nil {
		return err
	}
	root, cancel := context.WithCancelCause(context.Background())
	host.root, host.cancel, host.config = root, cancel, config
	if err := host.runtime.Start(ctx); err != nil {
		cancel(err)
		rollbackErr := host.rollback(ctx)
		return errors.Join(fmt.Errorf("start agent runtime: %w", err), rollbackErr)
	}
	for _, channel := range host.channels {
		current := channel
		if err := current.Start(ctx, func(callCtx context.Context, message Message) error {
			return host.Receive(callCtx, current, message)
		}); err != nil {
			rollbackErr := host.rollback(ctx)
			return errors.Join(fmt.Errorf("start channel %q: %w", current.ID(), err), rollbackErr)
		}
		host.started = append(host.started, current)
	}
	if host.scheduler != nil {
		if err := host.scheduler.Start(ctx, host.RunScheduled); err != nil {
			rollbackErr := host.rollback(ctx)
			return errors.Join(fmt.Errorf("start scheduler: %w", err), rollbackErr)
		}
	}
	host.running = true
	return nil
}

// Run starts the host, waits for cancellation, and performs bounded cleanup.
func (host *Host) Run(ctx context.Context) error {
	if err := host.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), host.Config().StopTimeout)
	defer cancel()
	if err := host.Stop(stopCtx); err != nil {
		return err
	}
	return ctx.Err()
}

// Stop cancels work and stops scheduler, channels in reverse order, and runtime.
// It is idempotent and joins cleanup errors.
func (host *Host) Stop(ctx context.Context) error {
	host.lifecycle.Lock()
	defer host.lifecycle.Unlock()
	if !host.running && host.root == nil {
		return nil
	}
	host.running = false
	if host.cancel != nil {
		host.cancel(context.Canceled)
	}
	host.approvals.clear(context.Canceled)
	var errs []error
	workDone := make(chan struct{})
	go func() {
		host.work.Wait()
		close(workDone)
	}()
	select {
	case <-workDone:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("wait for agent work: %w", ctx.Err()))
	}
	for index := len(host.started) - 1; index >= 0; index-- {
		if err := host.started[index].Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop channel %q: %w", host.started[index].ID(), err))
		}
	}
	if host.scheduler != nil {
		if err := host.scheduler.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop scheduler: %w", err))
		}
	}
	if err := host.runtime.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stop agent runtime: %w", err))
	}
	host.started = nil
	host.root, host.cancel = nil, nil
	return errors.Join(errs...)
}

func (host *Host) rollback(ctx context.Context) error {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), host.config.StopTimeout)
	defer cleanupCancel()
	if host.cancel != nil {
		host.cancel(context.Canceled)
	}
	host.approvals.clear(context.Canceled)
	var errs []error
	for index := len(host.started) - 1; index >= 0; index-- {
		if err := host.started[index].Stop(cleanupCtx); err != nil {
			errs = append(errs, fmt.Errorf("rollback channel %q: %w", host.started[index].ID(), err))
		}
	}
	if err := host.runtime.Stop(cleanupCtx); err != nil {
		errs = append(errs, fmt.Errorf("rollback agent runtime: %w", err))
	}
	host.started = nil
	host.root, host.cancel = nil, nil
	return errors.Join(errs...)
}

// Receive handles one channel message. Calls for a channel conversation are
// serialized; other conversations may run concurrently. The /stop command
// cancels the active call and invalidates calls already queued behind it.
func (host *Host) Receive(ctx context.Context, channel Channel, message Message) error {
	if nilValue(channel) {
		return fmt.Errorf("receive message: nil channel")
	}
	config, root, err := host.activeState()
	if err != nil {
		return err
	}
	defer host.work.Done()
	if strings.TrimSpace(message.ConversationID) == "" {
		return fmt.Errorf("%w: conversation ID is required", ErrInvalidMessage)
	}
	if len(message.ConversationID) > 1024 || len(message.SenderID) > 1024 || len(message.MessageID) > 1024 || len(message.Metadata) > 128 {
		return ErrInvalidMessage
	}
	if len(message.Text) > config.MaxMessageBytes {
		return ErrMessageTooLarge
	}
	key := channel.ID() + ":" + message.ConversationID
	state := host.conversation(key)
	if commandName(message.Text) == "/stop" {
		cancelled := state.stop()
		text := "No in-flight run to stop."
		if cancelled {
			text = "Stopped current run."
		}
		return host.send(ctx, channel, message.ConversationID, text, config.SendTimeout)
	}
	if host.approvals.consume(approvalKey{channelID: channel.ID(), conversationID: message.ConversationID}, message) {
		return nil
	}
	generation := state.currentGeneration()
	state.beginWait()
	select {
	case state.gate <- struct{}{}:
		state.endWait()
	case <-ctx.Done():
		state.endWait()
		return ctx.Err()
	case <-root.Done():
		state.endWait()
		return ErrHostNotRunning
	}
	defer func() { <-state.gate }()
	if generation != state.currentGeneration() {
		return nil
	}
	invokeCtx, cancel := joinedContext(root, ctx)
	id := state.register(cancel)
	defer func() {
		state.unregister(id)
		cancel(nil)
	}()
	metadata := cloneMetadata(message.Metadata)
	metadata["channel"] = channel.ID()
	if message.SenderID != "" {
		metadata["sender_id"] = message.SenderID
	}
	if message.MessageID != "" {
		metadata["message_id"] = message.MessageID
	}
	request := Request{
		ConversationID: key, Text: message.Text, Metadata: metadata,
		Workspace: config.Workspace, RecursionLimit: config.RecursionLimit,
	}
	request.ApprovalHandler = func(handlerCtx context.Context, approval ToolApprovalRequest) (ToolApprovalDecision, error) {
		approvalCtx, approvalCancel := joinedContext(invokeCtx, handlerCtx)
		defer approvalCancel(nil)
		return host.requestToolApproval(approvalCtx, channel, message.ConversationID, key, message.SenderID, approval, config)
	}
	result, err := host.runtime.Invoke(invokeCtx, request)
	if err != nil {
		if errors.Is(context.Cause(invokeCtx), errStoppedByCommand) {
			return nil
		}
		return err
	}
	if result.Text == "" {
		return nil
	}
	return host.send(ctx, channel, message.ConversationID, result.Text, config.SendTimeout)
}

// RunScheduled invokes one scheduler job through the same per-conversation
// serialization and bounds as channel traffic.
func (host *Host) RunScheduled(ctx context.Context, job ScheduledJob) (string, error) {
	config, root, err := host.activeState()
	if err != nil {
		return "", err
	}
	defer host.work.Done()
	if strings.TrimSpace(job.ConversationID) == "" {
		return "", fmt.Errorf("%w: scheduled conversation ID is required", ErrInvalidMessage)
	}
	if len(job.ConversationID) > 1024 || len(job.ID) > 1024 || len(job.ChannelID) > 128 || len(job.Metadata) > 128 {
		return "", ErrInvalidMessage
	}
	if len(job.Prompt) > config.MaxMessageBytes {
		return "", ErrMessageTooLarge
	}
	provider := job.ChannelID
	if provider == "" {
		provider = "cron"
	}
	key := provider + ":" + job.ConversationID
	state := host.conversation(key)
	generation := state.currentGeneration()
	state.beginWait()
	select {
	case state.gate <- struct{}{}:
		state.endWait()
	case <-ctx.Done():
		state.endWait()
		return "", ctx.Err()
	case <-root.Done():
		state.endWait()
		return "", ErrHostNotRunning
	}
	defer func() { <-state.gate }()
	if generation != state.currentGeneration() {
		return "", nil
	}
	invokeCtx, cancel := joinedContext(root, ctx)
	id := state.register(cancel)
	defer func() {
		state.unregister(id)
		cancel(nil)
	}()
	metadata := cloneMetadata(job.Metadata)
	metadata["trigger"] = "cron"
	metadata["cron_job_id"] = job.ID
	result, err := host.runtime.Invoke(invokeCtx, Request{
		ConversationID: key, Text: job.Prompt, Metadata: metadata,
		Workspace: config.Workspace, RecursionLimit: config.RecursionLimit,
	})
	return result.Text, err
}

func (host *Host) activeState() (Config, context.Context, error) {
	host.lifecycle.Lock()
	defer host.lifecycle.Unlock()
	if !host.running || host.root == nil {
		return Config{}, nil, ErrHostNotRunning
	}
	host.work.Add(1)
	return host.config, host.root, nil
}

func (host *Host) send(ctx context.Context, channel Channel, conversationID, text string, timeout time.Duration) error {
	sendCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result := channel.Send(sendCtx, conversationID, text)
	if result.Success {
		return nil
	}
	if result.Error == "" {
		return ErrSendFailed
	}
	return fmt.Errorf("%w: %s", ErrSendFailed, boundedString(result.Error, 1024))
}

func (host *Host) conversation(key string) *conversation {
	created := &conversation{gate: make(chan struct{}, 1), active: make(map[uint64]context.CancelCauseFunc)}
	actual, _ := host.conversations.LoadOrStore(key, created)
	return actual.(*conversation)
}

func (state *conversation) currentGeneration() uint64 {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.generation
}

func (state *conversation) beginWait() {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.waiting++
}

func (state *conversation) endWait() {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.waiting--
}

func (state *conversation) register(cancel context.CancelCauseFunc) uint64 {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.next++
	state.active[state.next] = cancel
	return state.next
}

func (state *conversation) unregister(id uint64) {
	state.mu.Lock()
	defer state.mu.Unlock()
	delete(state.active, id)
}

func (state *conversation) stop() bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.generation++
	cancelled := len(state.active) > 0
	for _, cancel := range state.active {
		cancel(errStoppedByCommand)
	}
	return cancelled
}

func validateChannels(channels []Channel) error {
	seen := make(map[string]struct{}, len(channels))
	for _, channel := range channels {
		id := strings.TrimSpace(channel.ID())
		if !assistantIDPattern.MatchString(id) || id == "." || id == ".." {
			return fmt.Errorf("%w: channel ID must contain 1-128 letters, numbers, dots, underscores, or hyphens", ErrInvalidConfig)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("%w: duplicate channel ID %q", ErrInvalidConfig, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func commandName(text string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(text), " ")
	return strings.ToLower(first)
}

func cloneMetadata(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source)+3)
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func joinedContext(root, call context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(root)
	stop := context.AfterFunc(call, func() { cancel(context.Cause(call)) })
	return ctx, func(cause error) {
		stop()
		cancel(cause)
	}
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	kind := reflect.ValueOf(value).Kind()
	return (kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice) && reflect.ValueOf(value).IsNil()
}

func boundedString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

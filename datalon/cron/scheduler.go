package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/datalon"
)

const Silent = "[SILENT]"

// Deliver sends a completed job result to its originating channel.
type Deliver func(context.Context, Job, string) error

// Event is one structured scheduler lifecycle record.
type Event struct {
	Event          string    `json:"event"`
	At             time.Time `json:"at"`
	DueCount       int       `json:"due_count,omitempty"`
	JobID          string    `json:"job_id,omitempty"`
	JobName        string    `json:"job_name,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	NextRunAt      time.Time `json:"next_run_at,omitzero"`
	Error          string    `json:"error,omitempty"`
	Silent         bool      `json:"silent,omitempty"`
}

// SchedulerOptions contains optional ticker, work, clock, and logging controls.
// Zero values select a one-minute tick, 100 jobs per tick, 30-minute job bound,
// the UTC system clock, and slog.Default.
type SchedulerOptions struct {
	TickInterval  time.Duration
	JobTimeout    time.Duration
	MaxDuePerTick int
	Now           func() time.Time
	Logger        *slog.Logger
	OnEvent       func(Event)
}

// Scheduler implements datalon.Scheduler with a persistent Store.
type Scheduler struct {
	store   *Store
	deliver Deliver
	options SchedulerOptions

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
	tickMu  sync.Mutex
}

// NewScheduler constructs a scheduler. The store and delivery function are
// required positional dependencies; typed nil values panic.
func NewScheduler(store *Store, deliver Deliver, options SchedulerOptions) *Scheduler {
	if store == nil {
		panic("datalon/cron: nil store")
	}
	if deliver == nil {
		panic("datalon/cron: nil delivery function")
	}
	if options.TickInterval < 0 || options.JobTimeout < 0 || options.MaxDuePerTick < 0 {
		panic("datalon/cron: scheduler limits cannot be negative")
	}
	if options.TickInterval == 0 {
		options.TickInterval = time.Minute
	}
	if options.JobTimeout == 0 {
		options.JobTimeout = 30 * time.Minute
	}
	if options.MaxDuePerTick == 0 {
		options.MaxDuePerTick = 100
	}
	if options.MaxDuePerTick > store.options.MaxJobs {
		options.MaxDuePerTick = store.options.MaxJobs
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Scheduler{store: store, deliver: deliver, options: options}
}

// Start begins the ticker. Repeated starts while running are idempotent.
func (scheduler *Scheduler) Start(_ context.Context, handler datalon.ScheduledHandler) error {
	if handler == nil {
		return fmt.Errorf("start cron scheduler: handler is required")
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.running {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	scheduler.running, scheduler.cancel, scheduler.done = true, cancel, make(chan struct{})
	go scheduler.loop(ctx, handler, scheduler.done)
	return nil
}

// Stop cancels and waits for the ticker. It is idempotent.
func (scheduler *Scheduler) Stop(ctx context.Context) error {
	scheduler.mu.Lock()
	if !scheduler.running {
		scheduler.mu.Unlock()
		return nil
	}
	cancel, done := scheduler.cancel, scheduler.done
	scheduler.running = false
	scheduler.cancel, scheduler.done = nil, nil
	scheduler.mu.Unlock()
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Tick claims and runs the jobs due at the current configured clock time.
func (scheduler *Scheduler) Tick(ctx context.Context, handler datalon.ScheduledHandler) error {
	if handler == nil {
		return fmt.Errorf("tick cron scheduler: handler is required")
	}
	scheduler.tickMu.Lock()
	defer scheduler.tickMu.Unlock()
	now := scheduler.options.Now().UTC()
	jobs, err := scheduler.store.due(ctx, now, scheduler.options.MaxDuePerTick)
	if err != nil {
		return err
	}
	scheduler.emit(Event{Event: "cron.tick", At: now, DueCount: len(jobs)})
	var errs []error
	for _, candidate := range jobs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		claimed, ok, err := scheduler.store.claim(ctx, candidate.ID, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok {
			continue
		}
		scheduler.emit(Event{Event: "cron.dispatch", At: now, JobID: claimed.ID, JobName: claimed.Name, ConversationID: claimed.Origin.ConversationID, NextRunAt: claimed.NextRunAt})
		jobCtx, cancel := context.WithTimeout(ctx, scheduler.options.JobTimeout)
		text, runErr := handler(jobCtx, datalon.ScheduledJob{
			ID: claimed.ID, ConversationID: claimed.Origin.ConversationID,
			Prompt: claimed.Prompt, ChannelID: claimed.Origin.ChannelID,
			Metadata: map[string]any{"cron_job_name": claimed.Name, "cron_origin_message_id": claimed.Origin.MessageID},
		})
		cancel()
		if runErr != nil {
			errorText := bounded(runErr.Error(), scheduler.store.options.MaxErrorBytes)
			if markErr := scheduler.store.mark(ctx, claimed.ID, StatusError, errorText, scheduler.options.Now()); markErr != nil {
				errs = append(errs, errors.Join(runErr, markErr))
			} else {
				errs = append(errs, runErr)
			}
			scheduler.emit(Event{Event: "cron.failure", At: scheduler.options.Now(), JobID: claimed.ID, JobName: claimed.Name, Error: errorText})
			continue
		}
		if err := scheduler.store.mark(ctx, claimed.ID, StatusOK, "", scheduler.options.Now()); err != nil {
			errs = append(errs, err)
			continue
		}
		silent := strings.HasPrefix(strings.TrimSpace(text), Silent)
		scheduler.emit(Event{Event: "cron.success", At: scheduler.options.Now(), JobID: claimed.ID, JobName: claimed.Name, Silent: silent})
		if silent {
			scheduler.emit(Event{Event: "cron.delivery_suppressed", At: scheduler.options.Now(), JobID: claimed.ID, JobName: claimed.Name})
			continue
		}
		if text == "" {
			continue
		}
		deliveryCtx, deliveryCancel := context.WithTimeout(ctx, scheduler.options.JobTimeout)
		deliveryErr := scheduler.deliver(deliveryCtx, claimed, text)
		deliveryCancel()
		if deliveryErr != nil {
			errorText := bounded("delivery failed: "+deliveryErr.Error(), scheduler.store.options.MaxErrorBytes)
			if markErr := scheduler.store.mark(ctx, claimed.ID, StatusError, errorText, scheduler.options.Now()); markErr != nil {
				errs = append(errs, errors.Join(deliveryErr, markErr))
			} else {
				errs = append(errs, deliveryErr)
			}
			scheduler.emit(Event{Event: "cron.delivery_failure", At: scheduler.options.Now(), JobID: claimed.ID, JobName: claimed.Name, Error: errorText})
			continue
		}
		scheduler.emit(Event{Event: "cron.delivery", At: scheduler.options.Now(), JobID: claimed.ID, JobName: claimed.Name, ConversationID: claimed.Origin.ConversationID})
	}
	return errors.Join(errs...)
}

func (scheduler *Scheduler) loop(ctx context.Context, handler datalon.ScheduledHandler, done chan struct{}) {
	defer close(done)
	for {
		if err := scheduler.Tick(ctx, handler); err != nil && !errors.Is(err, context.Canceled) {
			scheduler.options.Logger.Error("cron scheduler tick failed", "error", bounded(err.Error(), scheduler.store.options.MaxErrorBytes))
		}
		timer := time.NewTimer(scheduler.options.TickInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func (scheduler *Scheduler) emit(event Event) {
	if event.At.IsZero() {
		event.At = scheduler.options.Now().UTC()
	}
	encoded, err := json.Marshal(event)
	if err == nil {
		scheduler.options.Logger.Info("talon_event " + string(encoded))
	}
	if scheduler.options.OnEvent != nil {
		scheduler.options.OnEvent(event)
	}
}

package cron

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/dago/datalon"
)

func schedulerForTest(t *testing.T, store *Store, now time.Time, deliver Deliver, events *[]Event) *Scheduler {
	t.Helper()
	return NewScheduler(store, deliver, SchedulerOptions{
		TickInterval: time.Hour, JobTimeout: time.Second, Now: func() time.Time { return now },
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnEvent: func(event Event) { *events = append(*events, event) },
	})
}

func TestSchedulerClaimsRunsPersistsAndDelivers(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := created.Add(time.Minute)
	schedule, _ := ParseSchedule("in 1m")
	job, err := store.Create(t.Context(), "check", schedule, Origin{ConversationID: "chat", ChannelID: "telegram"}, CreateOptions{Name: "status", Now: created})
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	var delivered string
	scheduler := schedulerForTest(t, store, now, func(_ context.Context, claimed Job, text string) error {
		delivered = claimed.ID + ":" + text
		return nil
	}, &events)
	err = scheduler.Tick(t.Context(), func(_ context.Context, scheduled datalon.ScheduledJob) (string, error) {
		stored, listErr := store.List(t.Context(), nil)
		if listErr != nil {
			return "", listErr
		}
		if stored[0].Enabled || !stored[0].NextRunAt.IsZero() {
			t.Fatal("job was not claimed before execution")
		}
		if scheduled.ConversationID != "chat" || scheduled.ChannelID != "telegram" || scheduled.Prompt != "check" {
			t.Fatalf("scheduled = %+v", scheduled)
		}
		return "done", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if delivered != job.ID+":done" {
		t.Fatalf("delivered = %q", delivered)
	}
	stored, err := store.List(t.Context(), nil)
	if err != nil || stored[0].LastStatus != StatusOK || stored[0].LastError != "" {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	want := []string{"cron.tick", "cron.dispatch", "cron.success", "cron.delivery"}
	for index, name := range want {
		if events[index].Event != name {
			t.Fatalf("event %d = %q, want %q", index, events[index].Event, name)
		}
	}
}

func TestSchedulerPersistsRunAndDeliveryFailures(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, eventName     string
		runErr, deliveryErr error
	}{
		{name: "run", runErr: errors.New("model unavailable"), eventName: "cron.failure"},
		{name: "delivery", deliveryErr: errors.New("bridge unavailable"), eventName: "cron.delivery_failure"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := newTestStore(t)
			created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			schedule, _ := ParseSchedule("every 1m")
			job, err := store.Create(t.Context(), "check", schedule, Origin{ConversationID: "chat"}, CreateOptions{Now: created})
			if err != nil {
				t.Fatal(err)
			}
			var events []Event
			scheduler := schedulerForTest(t, store, created.Add(time.Minute), func(context.Context, Job, string) error { return testCase.deliveryErr }, &events)
			err = scheduler.Tick(t.Context(), func(context.Context, datalon.ScheduledJob) (string, error) { return "done", testCase.runErr })
			if err == nil {
				t.Fatal("Tick unexpectedly succeeded")
			}
			jobs, listErr := store.List(t.Context(), nil)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if jobs[0].ID != job.ID || jobs[0].LastStatus != StatusError || jobs[0].LastError == "" {
				t.Fatalf("stored = %+v", jobs[0])
			}
			if events[len(events)-1].Event != testCase.eventName {
				t.Fatalf("last event = %q", events[len(events)-1].Event)
			}
		})
	}
}

func TestSchedulerSuppressesSilentResult(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	schedule, _ := ParseSchedule("in 1m")
	if _, err := store.Create(t.Context(), "quiet", schedule, Origin{ConversationID: "chat"}, CreateOptions{Now: created}); err != nil {
		t.Fatal(err)
	}
	var events []Event
	deliveries := 0
	scheduler := schedulerForTest(t, store, created.Add(time.Minute), func(context.Context, Job, string) error { deliveries++; return nil }, &events)
	if err := scheduler.Tick(t.Context(), func(context.Context, datalon.ScheduledJob) (string, error) { return " [SILENT] no change", nil }); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 || events[len(events)-1].Event != "cron.delivery_suppressed" {
		t.Fatalf("deliveries = %d, events = %+v", deliveries, events)
	}
}

func TestSchedulerStartStopAndCancellation(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	schedule, _ := ParseSchedule("in 1m")
	if _, err := store.Create(t.Context(), "wait", schedule, Origin{ConversationID: "chat"}, CreateOptions{Now: created}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var once sync.Once
	scheduler := NewScheduler(store, func(context.Context, Job, string) error { return nil }, SchedulerOptions{
		TickInterval: time.Hour, JobTimeout: time.Hour, Now: func() time.Time { return created.Add(time.Minute) },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := scheduler.Start(t.Context(), func(ctx context.Context, _ datalon.ScheduledJob) (string, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return "", ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := scheduler.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentTicksClaimOneIntervalOnce(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	schedule, _ := ParseSchedule("in 1m")
	if _, err := store.Create(t.Context(), "once", schedule, Origin{ConversationID: "chat"}, CreateOptions{Now: created}); err != nil {
		t.Fatal(err)
	}
	var events []Event
	var mu sync.Mutex
	deliveries := 0
	scheduler := schedulerForTest(t, store, created.Add(time.Minute), func(context.Context, Job, string) error {
		mu.Lock()
		deliveries++
		mu.Unlock()
		return nil
	}, &events)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			errs <- scheduler.Tick(t.Context(), func(context.Context, datalon.ScheduledJob) (string, error) { return "done", nil })
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if deliveries != 1 {
		t.Fatalf("deliveries = %d", deliveries)
	}
}

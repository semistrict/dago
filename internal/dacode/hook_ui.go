package dacode

import (
	"context"
	"errors"
	"fmt"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago/dahook"
)

func waitForHookStatus(ctx context.Context, source hookStatusSource) tea.Cmd {
	if ctx == nil || source == nil {
		panic("dacode: hook status context and source are required")
	}
	return func() tea.Msg {
		update, err := source.NextHookStatus(ctx)
		return hookStatusMsg{update: update, err: err}
	}
}

type hookStatusUpdate struct {
	Status string
	Event  dahook.Event
	Active bool
}

type hookStatusSource interface {
	NextHookStatus(context.Context) (hookStatusUpdate, error)
}

type activeHookStatus struct {
	progress dahook.Progress
	order    uint64
}

// hookUISink reduces concurrent handler progress to the most recently started
// active status. Its notification channel is coalescing: a slow terminal sees
// the newest snapshot without ever blocking hook subprocess completion.
type hookUISink struct {
	mu         sync.Mutex
	active     map[string]activeHookStatus
	latest     hookStatusUpdate
	sequence   uint64
	generation uint64
	notify     chan struct{}
}

func newHookUISink() *hookUISink {
	return &hookUISink{active: map[string]activeHookStatus{}, notify: make(chan struct{}, 1)}
}

func (sink *hookUISink) Publish(progress dahook.Progress) {
	if sink == nil {
		return
	}
	sink.mu.Lock()
	sink.sequence++
	if progress.Active {
		sink.active[progress.OperationID] = activeHookStatus{progress: progress, order: sink.sequence}
	} else {
		delete(sink.active, progress.OperationID)
	}
	var newest activeHookStatus
	for _, candidate := range sink.active {
		if candidate.order > newest.order {
			newest = candidate
		}
	}
	sink.latest = hookStatusUpdate{}
	if newest.order != 0 {
		message := newest.progress.Message
		if message == "" {
			message = fmt.Sprintf("Running %s hook...", newest.progress.Event)
		}
		sink.latest = hookStatusUpdate{Status: message, Event: newest.progress.Event, Active: true}
	} else {
		sink.latest = hookStatusUpdate{Event: progress.Event}
	}
	sink.generation++
	sink.mu.Unlock()
	select {
	case sink.notify <- struct{}{}:
	default:
	}
}

func (sink *hookUISink) Next(ctx context.Context) (hookStatusUpdate, error) {
	if sink == nil {
		return hookStatusUpdate{}, errors.New("hook status source is unavailable")
	}
	select {
	case <-ctx.Done():
		return hookStatusUpdate{}, ctx.Err()
	case <-sink.notify:
		sink.mu.Lock()
		latest := sink.latest
		sink.mu.Unlock()
		return latest, nil
	}
}

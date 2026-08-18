package notifications

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"sync"
)

// Dispatcher routes notification events to registered backend channels.
type Dispatcher struct {
	mu       sync.RWMutex
	channels []Channel
	logger   *slog.Logger
}

// NewDispatcher creates a new notification dispatcher.
func NewDispatcher(logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{logger: logger}
}

// Register adds a backend channel to the dispatcher.
func (d *Dispatcher) Register(ch Channel) {
	if nilChannel(ch) || strings.TrimSpace(ch.Name()) == "" {
		panic("notification channel is required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.channels = append(d.channels, ch)
}

// ReplaceChannels atomically replaces the entire channel set.
func (d *Dispatcher) ReplaceChannels(channels []Channel) {
	for _, channel := range channels {
		if nilChannel(channel) || strings.TrimSpace(channel.Name()) == "" {
			panic("notification channel is required")
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.channels = channels
}

func nilChannel(channel Channel) bool {
	if channel == nil {
		return true
	}
	value := reflect.ValueOf(channel)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Channels returns a snapshot of current registered channels.
func (d *Dispatcher) Channels() []Channel {
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make([]Channel, len(d.channels))
	copy(result, d.channels)
	return result
}

// Dispatch sends an event to all registered backend channels.
// It does not block on individual channel failures.
func (d *Dispatcher) Dispatch(ctx context.Context, event Event) {
	d.mu.RLock()
	channels := d.channels
	d.mu.RUnlock()

	for _, ch := range channels {
		if err := ch.Send(ctx, event); err != nil {
			d.logger.Warn(
				"notification channel failed",
				"channel", ch.Name(),
				"event", string(event.Type),
				"error", err,
			)
		}
	}
}

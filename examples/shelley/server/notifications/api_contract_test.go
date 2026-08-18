package notifications

import (
	"context"
	"log/slog"
	"testing"
)

type nilTestChannel struct{}

func (*nilTestChannel) Name() string                      { return "test" }
func (*nilTestChannel) Send(context.Context, Event) error { return nil }

type unnamedTestChannel struct{}

func (unnamedTestChannel) Name() string                      { return "  " }
func (unnamedTestChannel) Send(context.Context, Event) error { return nil }

func TestDispatcherRejectsTypedNilChannelsAndDefaultsLogger(t *testing.T) {
	dispatcher := NewDispatcher(nil)
	if dispatcher.logger != slog.Default() {
		t.Fatal("nil logger did not use the process default")
	}
	var channel *nilTestChannel
	defer func() {
		if recover() == nil {
			t.Fatal("typed-nil channel was accepted")
		}
	}()
	dispatcher.Register(channel)
}

func TestDispatcherRejectsUnnamedChannel(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("unnamed channel was accepted")
		}
	}()
	NewDispatcher(nil).Register(unnamedTestChannel{})
}

func TestRegistryRejectsMissingFactory(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil factory was accepted")
		}
	}()
	Register("contract-nil-factory", nil)
}

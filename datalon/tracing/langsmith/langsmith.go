// Package langsmith adapts LangSmith run ingestion to datalon tracing.
package langsmith

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	ls "github.com/langchain-ai/langsmith-go"
	"github.com/semistrict/dago/datalon/tracing"
)

// ErrClientFactory reports a failed or invalid caller client factory without
// exposing provider error text that may contain credentials.
var ErrClientFactory = errors.New("LangSmith tracing client factory failed")

// Client is the narrow caller-owned LangSmith ingestion surface.
type Client interface {
	CreateRun(*ls.RunCreate) error
	UpdateRun(*ls.RunUpdate) error
}

// ClientFactory constructs a caller-owned LangSmith client from the resolved
// endpoint and API key. Implementations retain HTTP, retry, flush, and shutdown
// ownership and must honor cancellation during construction.
type ClientFactory interface {
	NewTracingClient(context.Context, string, string) (Client, error)
}

// Factory bridges managed tracing configuration to the LangSmith sink.
type Factory struct{ clients ClientFactory }

// NewFactory constructs the bridge without reading credentials or performing I/O.
func NewFactory(clients ClientFactory) *Factory {
	if nilValue(clients) {
		panic("LangSmith tracing client factory is required")
	}
	return &Factory{clients: clients}
}

// NewTracingSink implements tracing.SinkFactory.
func (factory *Factory) NewTracingSink(ctx context.Context, endpoint, apiKey string) (tracing.Sink, error) {
	if factory == nil || nilValue(factory.clients) {
		panic("initialized LangSmith tracing client factory is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, err := callClientFactory(ctx, factory.clients, endpoint, apiKey)
	if err != nil || nilValue(client) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrClientFactory
	}
	return New(client), nil
}

func callClientFactory(ctx context.Context, factory ClientFactory, endpoint, apiKey string) (client Client, err error) {
	defer func() {
		if recover() != nil {
			client, err = nil, ErrClientFactory
		}
	}()
	return factory.NewTracingClient(ctx, endpoint, apiKey)
}

// Sink sends provider-neutral run records through a caller-owned client.
type Sink struct{ client Client }

// New constructs a sink without reading credentials or starting network work.
func New(client Client) *Sink {
	if nilValue(client) {
		panic("LangSmith tracing client is required")
	}
	return &Sink{client: client}
}

func (sink *Sink) Begin(ctx context.Context, run tracing.Run) (tracing.Span, error) {
	if sink == nil || nilValue(sink.client) {
		panic("initialized LangSmith tracing sink is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := uuid.New()
	dotted := fmt.Sprintf("%s%06dZ%s", run.StartTime.UTC().Format("20060102T150405"), run.StartTime.UTC().Nanosecond()/1000, id.String())
	if err := sink.client.CreateRun(&ls.RunCreate{
		ID: id, TraceID: id, Name: run.Name, RunType: "chain",
		Inputs: map[string]any{"input": run.Input}, Extra: map[string]any{"metadata": run.Metadata}, Tags: append([]string(nil), run.Tags...),
		StartTime: run.StartTime, DottedOrder: dotted, SessionName: run.Project,
	}); err != nil {
		return nil, err
	}
	return &span{client: sink.client, id: id, dotted: dotted}, nil
}

type span struct {
	client Client
	id     uuid.UUID
	dotted string
}

func (span *span) End(ctx context.Context, completion tracing.Completion) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	update := &ls.RunUpdate{ID: span.id, TraceID: span.id, DottedOrder: span.dotted, EndTime: completion.EndTime}
	if completion.Error != "" {
		update.Error = completion.Error
	} else {
		update.Outputs = map[string]any{"output": completion.Output}
	}
	return span.client.UpdateRun(update)
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	kind := reflect.ValueOf(value).Kind()
	return (kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice) && reflect.ValueOf(value).IsNil()
}

var _ tracing.Sink = (*Sink)(nil)
var _ tracing.Span = (*span)(nil)
var _ tracing.SinkFactory = (*Factory)(nil)

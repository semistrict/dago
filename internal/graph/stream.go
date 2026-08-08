package graph

import (
	"context"
	"errors"
	"io"
	"sync"
)

// Stream is an owned graph event stream. Consumers must call Close when they stop
// before io.EOF.
type Stream struct {
	events <-chan Event
	done   <-chan struct{}
	cancel context.CancelCauseFunc

	mu        *sync.Mutex
	execution *Execution
	err       *error
	closed    bool
}

// Stream starts execution in a managed goroutine with bounded event backpressure.
func (graph *Compiled) Stream(ctx context.Context, invocation Invocation, buffer int) *Stream {
	if buffer < 0 {
		buffer = 0
	}
	streamContext, cancel := context.WithCancelCause(ctx)
	events := make(chan Event, buffer)
	done := make(chan struct{})
	mutex := &sync.Mutex{}
	execution := &Execution{}
	var terminal error
	writers := []EventWriter{channelWriter{events: events}}
	if graph.options.Writer != nil {
		writers = append(writers, graph.options.Writer)
	}
	copy := *graph
	copy.options = graph.options
	copy.options.Writer = multiWriter(writers)

	go func() {
		defer close(done)
		defer close(events)
		result, err := copy.Invoke(streamContext, invocation)
		mutex.Lock()
		*execution = result
		terminal = err
		mutex.Unlock()
	}()
	return &Stream{
		events: events, done: done, cancel: cancel,
		mu: mutex, execution: execution, err: &terminal,
	}
}

func (stream *Stream) Next(ctx context.Context) (Event, error) {
	select {
	case event, ok := <-stream.events:
		if ok {
			return event, nil
		}
		stream.mu.Lock()
		defer stream.mu.Unlock()
		if *stream.err != nil {
			return Event{}, *stream.err
		}
		return Event{}, io.EOF
	case <-ctx.Done():
		return Event{}, ctx.Err()
	}
}

func (stream *Stream) Result(ctx context.Context) (Execution, error) {
	select {
	case <-stream.done:
		stream.mu.Lock()
		defer stream.mu.Unlock()
		return *stream.execution, *stream.err
	case <-ctx.Done():
		return Execution{}, ctx.Err()
	}
}

func (stream *Stream) Close() error {
	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		return nil
	}
	stream.closed = true
	stream.mu.Unlock()
	stream.cancel(errors.New("graph stream closed"))
	return nil
}

type channelWriter struct {
	events chan<- Event
}

func (writer channelWriter) Write(ctx context.Context, event Event) error {
	select {
	case writer.events <- event:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

type multiWriter []EventWriter

func (writers multiWriter) Write(ctx context.Context, event Event) error {
	for _, writer := range writers {
		if err := writer.Write(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

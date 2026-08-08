// Package modeltest provides deterministic model doubles for tests and examples.
package modeltest

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/semistrict/dago/model"
)

// Step is one expected invocation or stream.
type Step struct {
	Check    func(model.Request) error
	Response model.Response
	Chunks   []model.Chunk
	Error    error
}

// Scripted consumes a fixed sequence of steps safely across concurrent callers.
type Scripted struct {
	mu      sync.Mutex
	profile model.Profile
	steps   []Step
	next    int
}

func New(profile model.Profile, steps ...Step) *Scripted {
	return &Scripted{profile: profile, steps: append([]Step(nil), steps...)}
}

func (script *Scripted) Invoke(ctx context.Context, request model.Request) (model.Response, error) {
	step, err := script.take(request)
	if err != nil {
		return model.Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Response{}, err
	}
	if step.Error != nil {
		return model.Response{}, step.Error
	}
	return step.Response, nil
}

func (script *Scripted) Stream(ctx context.Context, request model.Request) (model.Stream, error) {
	step, err := script.take(request)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if step.Error != nil {
		return nil, step.Error
	}
	return &stream{chunks: append([]model.Chunk(nil), step.Chunks...)}, nil
}

func (script *Scripted) Profile() model.Profile { return script.profile }

func (script *Scripted) Remaining() int {
	script.mu.Lock()
	defer script.mu.Unlock()
	return len(script.steps) - script.next
}

func (script *Scripted) take(request model.Request) (Step, error) {
	script.mu.Lock()
	defer script.mu.Unlock()
	if script.next >= len(script.steps) {
		return Step{}, fmt.Errorf("scripted model received unexpected call %d", script.next+1)
	}
	step := script.steps[script.next]
	script.next++
	if step.Check != nil {
		if err := step.Check(request); err != nil {
			return Step{}, fmt.Errorf("scripted model call %d: %w", script.next, err)
		}
	}
	return step, nil
}

type stream struct {
	mu     sync.Mutex
	chunks []model.Chunk
	next   int
	closed bool
}

func (stream *stream) Next(ctx context.Context) (model.Chunk, error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.closed || stream.next >= len(stream.chunks) {
		return model.Chunk{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return model.Chunk{}, err
	}
	chunk := stream.chunks[stream.next]
	stream.next++
	return chunk, nil
}

func (stream *stream) Close() error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.closed = true
	return nil
}

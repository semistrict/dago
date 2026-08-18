package datalon

import "context"

// EchoRuntime is the useful no-model fallback. It returns the input text and
// has no resources to start or stop.
type EchoRuntime struct{}

func (EchoRuntime) Start(context.Context) error { return nil }
func (EchoRuntime) Stop(context.Context) error  { return nil }
func (EchoRuntime) Invoke(_ context.Context, request Request) (Result, error) {
	return Result{Text: request.Text}, nil
}

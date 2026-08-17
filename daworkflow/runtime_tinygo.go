//go:build tinygo

package daworkflow

import (
	"context"
	"fmt"
)

func runWorkflow(context.Context, AgentRunner, string, any, Options) (Result, error) {
	return Result{}, fmt.Errorf("%w in TinyGo builds", ErrUnavailable)
}

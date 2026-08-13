// Package studio provides a network-free agent for exercising dago with
// LangSmith Studio.
package studio

import (
	"context"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/daserver"
)

// NewAgent constructs one agent using the server-owned durable state.
func NewAgent(_ context.Context, runtime daserver.Runtime) (*dagent.Agent, error) {
	return dago.NewAgent(
		modeltest.NewPredictable(modeltest.PredictableOptions{}),
		dago.WithName("studio-example"),
		dago.WithSaver(runtime.Saver),
		dago.WithStore(runtime.Store),
		dago.WithDependencies(runtime.Deps),
	), nil
}

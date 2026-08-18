package dacode

import (
	"context"
	"errors"
)

// restartController is the app-neutral seam consumed by the restart command
// and confirmation modal. It deliberately exposes no process handle,
// environment, or endpoint mutation authority.
type restartController interface {
	Restart(context.Context) error
}

type localDevRestartController struct {
	server *localDevServer
}

func newLocalDevRestartController(server *localDevServer) *localDevRestartController {
	if server == nil {
		panic("dacode: local development server is required")
	}
	return &localDevRestartController{server: server}
}

func (controller *localDevRestartController) Restart(ctx context.Context) error {
	if controller == nil || controller.server == nil {
		return errors.New("local development server restart is unavailable")
	}
	return controller.server.Restart(ctx)
}

var _ restartController = (*localDevRestartController)(nil)

package dago

import (
	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
)

func backendRuntime(runtime agent.Runtime) backend.Runtime {
	return backend.Runtime{
		Context: runtime.Context, ThreadID: runtime.Config.ThreadID,
		Namespace: runtime.Config.Namespace, CheckpointID: runtime.Config.CheckpointID,
		TaskID: runtime.TaskID, Store: runtime.Store,
	}
}

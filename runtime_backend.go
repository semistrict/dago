package dago

import (
	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dagent"
)

func backendRuntime(runtime dagent.Runtime) dabackend.Runtime {
	return dabackend.Runtime{
		Context: runtime.Context, ThreadID: runtime.Config.ThreadID,
		Namespace: runtime.Config.Namespace, CheckpointID: runtime.Config.CheckpointID,
		TaskID: runtime.TaskID, Store: runtime.Store,
	}
}

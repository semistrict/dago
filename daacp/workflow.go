package daacp

import (
	"context"
	"encoding/json"

	acp "github.com/coder/acp-go-sdk"
	"github.com/semistrict/dago/daworkflow"
)

const (
	// WorkflowUpdateMethod publishes latest-state workflow snapshots from the
	// agent to ACP clients that understand dago workflow observability.
	WorkflowUpdateMethod = "_dago/workflow/update"
	// WorkflowCancelMethod requests cancellation of one background workflow.
	WorkflowCancelMethod = "_dago/workflow/cancel"
	// WorkflowListMethod returns current workflow snapshots for resynchronization.
	WorkflowListMethod = "_dago/workflow/list"
)

// WorkflowUpdate is the versioned ACP extension notification payload.
type WorkflowUpdate struct {
	Version   int               `json:"version"`
	SessionID string            `json:"session_id"`
	Workflow  daworkflow.Status `json:"workflow"`
}

type workflowCancelRequest struct {
	Version   int    `json:"version"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
}

type workflowListRequest struct {
	Version   int    `json:"version"`
	SessionID string `json:"session_id"`
}

// WorkflowSource is optionally implemented by a session runner that exposes
// background workflow lifecycle state to ACP clients.
type WorkflowSource interface {
	SubscribeWorkflows() (<-chan daworkflow.Status, func())
	Workflows() []daworkflow.Status
	CancelWorkflow(string) bool
}

func (agent *protocolAgent) attachWorkflowNotifications(sessionID string, current *session) {
	if current == nil {
		return
	}
	agent.mu.Lock()
	if agent.sessions[sessionID] != current {
		agent.mu.Unlock()
		return
	}
	source, ok := current.runner.(WorkflowSource)
	if !ok {
		agent.mu.Unlock()
		return
	}
	updates, unsubscribe := source.SubscribeWorkflows()
	watchContext, cancel := context.WithCancel(agent.root)
	stop := func() {
		cancel()
		unsubscribe()
	}
	current.workflowCancel = stop
	agent.mu.Unlock()
	go func() {
		defer unsubscribe()
		for {
			select {
			case <-watchContext.Done():
				return
			case status, open := <-updates:
				if !open {
					return
				}
				connection, err := agent.waitForConnection(watchContext)
				if err != nil {
					return
				}
				_ = connection.NotifyExtension(watchContext, WorkflowUpdateMethod, WorkflowUpdate{
					Version: 1, SessionID: sessionID, Workflow: status,
				})
			}
		}
	}()
}

// HandleExtensionMethod implements dago's ACP request extensions. Unknown
// extension methods retain standard method-not-found behavior.
func (agent *protocolAgent) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case ModelListMethod:
		var request ModelListRequest
		if err := json.Unmarshal(params, &request); err != nil || request.Version != 1 {
			return nil, acp.NewInvalidParams(map[string]any{"models": "invalid list request"})
		}
		return modelListResponse(agent.options.ConfigOptions), nil
	case WorkflowCancelMethod:
		var request workflowCancelRequest
		if err := json.Unmarshal(params, &request); err != nil || request.Version != 1 || request.SessionID == "" || request.RunID == "" {
			return nil, acp.NewInvalidParams(map[string]any{"workflow": "invalid cancellation request"})
		}
		agent.mu.Lock()
		current := agent.sessions[request.SessionID]
		agent.mu.Unlock()
		source, ok := workflowSource(current)
		if !ok {
			return nil, acp.NewInvalidParams(map[string]any{"sessionId": "workflow runtime is unavailable"})
		}
		if !source.CancelWorkflow(request.RunID) {
			return nil, acp.NewInvalidParams(map[string]any{"runId": "workflow is not running"})
		}
		return map[string]any{"version": 1, "run_id": request.RunID, "status": "cancelling"}, nil
	case WorkflowListMethod:
		var request workflowListRequest
		if err := json.Unmarshal(params, &request); err != nil || request.Version != 1 || request.SessionID == "" {
			return nil, acp.NewInvalidParams(map[string]any{"workflow": "invalid list request"})
		}
		agent.mu.Lock()
		current := agent.sessions[request.SessionID]
		agent.mu.Unlock()
		source, ok := workflowSource(current)
		if !ok {
			return nil, acp.NewInvalidParams(map[string]any{"sessionId": "workflow runtime is unavailable"})
		}
		return map[string]any{"version": 1, "workflows": source.Workflows()}, nil
	default:
		return nil, acp.NewMethodNotFound(method)
	}
}

func workflowSource(current *session) (WorkflowSource, bool) {
	if current == nil {
		return nil, false
	}
	source, ok := current.runner.(WorkflowSource)
	return source, ok
}

var _ acp.ExtensionMethodHandler = (*protocolAgent)(nil)

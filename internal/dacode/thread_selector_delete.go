package dacode

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// threadSessionDeleter is the intentionally narrow integration seam for a
// checkpoint backend. The caller must pass the authorization returned by the
// selector; the state validates it again when the asynchronous result arrives.
type threadSessionDeleter interface {
	DeleteSession(context.Context, string, string, string) error
}

type threadDeleteCompletedMsg struct {
	Authorization threadDeleteAuthorization
	Err           error
}

func deleteSelectedThread(ctx context.Context, deleter threadSessionDeleter, authorization threadDeleteAuthorization) tea.Cmd {
	return func() tea.Msg {
		if authorization.SelectorID == 0 || authorization.Generation == 0 ||
			validThreadSelectorID(authorization.ThreadID) != authorization.ThreadID ||
			validThreadSelectorID(authorization.CheckpointID) != authorization.CheckpointID ||
			validThreadRevision(authorization.ThreadRevision) != authorization.ThreadRevision {
			return threadDeleteCompletedMsg{Authorization: authorization, Err: fmt.Errorf("thread deletion authorization is invalid")}
		}
		if deleter == nil {
			return threadDeleteCompletedMsg{Authorization: authorization, Err: fmt.Errorf("thread deletion is unavailable")}
		}
		return threadDeleteCompletedMsg{
			Authorization: authorization,
			Err:           deleter.DeleteSession(ctx, authorization.ThreadID, authorization.CheckpointID, authorization.ThreadRevision),
		}
	}
}

package dacode

import "github.com/semistrict/dago/dagent"

// subagentPanelEventFromAgentEvent is the pure runtime-integration seam. Only
// top-level child lifecycle events affect the panel; nested event updates stay
// in the ordinary transcript/tool surfaces.
func subagentPanelEventFromAgentEvent(event dagent.Event) (subagentPanelEvent, bool) {
	if event.Mode != dagent.EventChild || event.Child == nil {
		return subagentPanelEvent{}, false
	}
	child := event.Child
	result := subagentPanelEvent{
		ID: child.ToolCallID, EvalID: event.TaskID, SubagentType: "subagent", Label: child.Name,
	}
	switch child.Phase {
	case dagent.ChildStarted:
		result.Phase = subagentPanelEventStart
	case dagent.ChildCompleted:
		result.Phase = subagentPanelEventComplete
	case dagent.ChildFailed:
		result.Phase, result.Error = subagentPanelEventError, child.Error
	case dagent.ChildInterrupted:
		result.Phase = subagentPanelEventCancelled
	default:
		return subagentPanelEvent{}, false
	}
	return result, true
}

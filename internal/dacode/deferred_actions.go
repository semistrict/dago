package dacode

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	maximumDeferredPayloadArguments = 16
	maximumDeferredPayloadBytes     = 4096
	maximumDeferredActionKinds      = 8
)

type deferredActionKind string

const (
	deferredModelSwitch               deferredActionKind = "model_switch"
	deferredThreadSwitch              deferredActionKind = "thread_switch"
	deferredChatOutput                deferredActionKind = "chat_output"
	deferredAgentSwitch               deferredActionKind = "agent_switch"
	deferredMCPLogin                  deferredActionKind = "mcp_login"
	deferredMCPReconnect              deferredActionKind = "mcp_reconnect"
	deferredRubricModelSwitch         deferredActionKind = "rubric_model_switch"
	deferredRubricMaxIterationsSwitch deferredActionKind = "rubric_max_iterations_switch"
)

// deferredActionPayload is the bounded immutable value handed to a deferred
// executor. New call sites use ExecutePayload; Execute remains available for
// existing zero-payload actions.
type deferredActionPayload struct {
	Identity   string
	Arguments  []string
	Generation uint64
}

type deferredAction struct {
	Kind           deferredActionKind
	Payload        deferredActionPayload
	Execute        tea.Cmd
	ExecutePayload func(deferredActionPayload) tea.Msg
}

type deferredActionCompletedMsg struct {
	Kind    deferredActionKind
	Payload deferredActionPayload
	Message tea.Msg
	Failed  bool
}

type deferredDrainCompletedMsg struct {
	Actions      []deferredActionCompletedMsg
	Prompt       tea.Msg
	PromptFailed bool
}

type deferredDiscardReason string

const (
	deferredDiscardForceClear  deferredDiscardReason = "force_clear"
	deferredDiscardMCPRecovery deferredDiscardReason = "mcp_recovery"
)

type deferredDiscardedAction struct {
	Kind    deferredActionKind
	Payload deferredActionPayload
}

type deferredDiscardReport struct {
	Reason  deferredDiscardReason
	Actions []deferredDiscardedAction
}

type deferredActionQueue struct {
	actions []deferredAction
}

// deferAction is last-write-wins within a kind. Replacing an action moves the
// new immutable request to the back of the queue so execution order reflects
// the user's most recent sequence of choices. With one slot per finite kind,
// the queue can never exceed maximumDeferredActionKinds.
func (queue *deferredActionQueue) deferAction(action deferredAction) {
	if queue == nil {
		panic("dacode: initialized deferred action queue is required")
	}
	action = cloneDeferredAction(action)
	validateDeferredAction(action)
	for index, existing := range queue.actions {
		if existing.Kind == action.Kind {
			copy(queue.actions[index:], queue.actions[index+1:])
			queue.actions[len(queue.actions)-1] = action
			return
		}
	}
	if len(queue.actions) == maximumDeferredActionKinds {
		panic("dacode: deferred action queue exceeds its finite kind set")
	}
	queue.actions = append(queue.actions, action)
}

func (queue *deferredActionQueue) pop() (tea.Cmd, bool) {
	if queue == nil {
		panic("dacode: initialized deferred action queue is required")
	}
	if len(queue.actions) == 0 {
		return nil, false
	}
	action := queue.actions[0]
	copy(queue.actions, queue.actions[1:])
	queue.actions[len(queue.actions)-1] = deferredAction{}
	queue.actions = queue.actions[:len(queue.actions)-1]
	return func() tea.Msg { return executeDeferredAction(action) }, true
}

// drainBeforePrompt snapshots and clears the finite queue, then executes every
// deferred action in FIFO order before invoking the prompt command. A panic in
// one action is reported and does not prevent later actions or the prompt.
func (queue *deferredActionQueue) drainBeforePrompt(prompt tea.Cmd) (tea.Cmd, bool) {
	if queue == nil {
		panic("dacode: initialized deferred action queue is required")
	}
	if len(queue.actions) == 0 {
		return prompt, false
	}
	actions := append([]deferredAction(nil), queue.actions...)
	clear(queue.actions)
	queue.actions = nil
	return func() tea.Msg {
		message := deferredDrainCompletedMsg{Actions: make([]deferredActionCompletedMsg, 0, len(actions))}
		for _, action := range actions {
			message.Actions = append(message.Actions, executeDeferredAction(action))
		}
		if prompt != nil {
			func() {
				defer func() {
					if recover() != nil {
						message.PromptFailed = true
					}
				}()
				message.Prompt = prompt()
			}()
		}
		return message
	}, true
}

func executeDeferredAction(action deferredAction) (message deferredActionCompletedMsg) {
	message.Kind = action.Kind
	message.Payload = cloneDeferredPayload(action.Payload)
	defer func() {
		if recover() != nil {
			message.Message = nil
			message.Failed = true
		}
	}()
	if action.ExecutePayload != nil {
		message.Message = action.ExecutePayload(cloneDeferredPayload(action.Payload))
	} else {
		message.Message = action.Execute()
	}
	return message
}

func (queue *deferredActionQueue) discardFor(reason deferredDiscardReason) deferredDiscardReport {
	if queue == nil {
		panic("dacode: initialized deferred action queue is required")
	}
	if reason != deferredDiscardForceClear && reason != deferredDiscardMCPRecovery {
		panic("dacode: deferred discard reason is invalid")
	}
	report := deferredDiscardReport{Reason: reason, Actions: make([]deferredDiscardedAction, len(queue.actions))}
	for index, action := range queue.actions {
		report.Actions[index] = deferredDiscardedAction{Kind: action.Kind, Payload: cloneDeferredPayload(action.Payload)}
	}
	clear(queue.actions)
	queue.actions = nil
	return report
}

// discard preserves the original kind-only API. Force-clear and MCP recovery
// integrations use discardFor so the reason and exact payloads are reportable.
func (queue *deferredActionQueue) discard() []deferredActionKind {
	if queue == nil {
		panic("dacode: initialized deferred action queue is required")
	}
	kinds := make([]deferredActionKind, len(queue.actions))
	for index, action := range queue.actions {
		kinds[index] = action.Kind
	}
	clear(queue.actions)
	queue.actions = nil
	return kinds
}

func (queue *deferredActionQueue) length() int {
	if queue == nil {
		panic("dacode: initialized deferred action queue is required")
	}
	return len(queue.actions)
}

func validateDeferredAction(action deferredAction) {
	if !validDeferredActionKind(action.Kind) || (action.Execute == nil) == (action.ExecutePayload == nil) ||
		!validDeferredPayload(action.Payload) {
		panic("dacode: deferred action is invalid")
	}
	if action.Execute != nil && (action.Payload.Identity != "" || len(action.Payload.Arguments) != 0 || action.Payload.Generation != 0) {
		panic("dacode: legacy deferred action cannot carry a payload")
	}
}

func validDeferredPayload(payload deferredActionPayload) bool {
	if len(payload.Identity) > maximumDeferredPayloadBytes || hasModelSelectorControl(payload.Identity) ||
		len(payload.Arguments) > maximumDeferredPayloadArguments {
		return false
	}
	total := len(payload.Identity)
	for _, argument := range payload.Arguments {
		total += len(argument)
		if len(argument) > maximumDeferredPayloadBytes || total > maximumDeferredPayloadBytes || hasModelSelectorControl(argument) {
			return false
		}
	}
	return true
}

func cloneDeferredAction(action deferredAction) deferredAction {
	action.Payload = cloneDeferredPayload(action.Payload)
	return action
}

func cloneDeferredPayload(payload deferredActionPayload) deferredActionPayload {
	payload.Identity = strings.Clone(payload.Identity)
	payload.Arguments = append([]string(nil), payload.Arguments...)
	for index := range payload.Arguments {
		payload.Arguments[index] = strings.Clone(payload.Arguments[index])
	}
	return payload
}

func validDeferredActionKind(kind deferredActionKind) bool {
	switch kind {
	case deferredModelSwitch, deferredThreadSwitch, deferredChatOutput, deferredAgentSwitch,
		deferredMCPLogin, deferredMCPReconnect, deferredRubricModelSwitch, deferredRubricMaxIterationsSwitch:
		return true
	default:
		return false
	}
}

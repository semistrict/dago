// Package dahook implements the versioned lifecycle-hook protocol used by dago
// hosts. Hook commands are trusted operator configuration; server-owned events
// cross an interrupt boundary and are executed by the client-side Engine.
package dahook

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Event identifies one Hooks v2 lifecycle event.
type Event string

const (
	SessionStart       Event = "SessionStart"
	UserPromptSubmit   Event = "UserPromptSubmit"
	SessionEnd         Event = "SessionEnd"
	PermissionRequest  Event = "PermissionRequest"
	Notification       Event = "Notification"
	PreToolUse         Event = "PreToolUse"
	PostToolUse        Event = "PostToolUse"
	PostToolUseFailure Event = "PostToolUseFailure"
	PreCompact         Event = "PreCompact"
	Stop               Event = "Stop"
	SubagentStart      Event = "SubagentStart"
	SubagentStop       Event = "SubagentStop"
)

var allEvents = map[Event]struct{}{
	SessionStart: {}, UserPromptSubmit: {}, SessionEnd: {}, PermissionRequest: {},
	Notification: {}, PreToolUse: {}, PostToolUse: {}, PostToolUseFailure: {},
	PreCompact: {}, Stop: {}, SubagentStart: {}, SubagentStop: {},
}

func (invocation Invocation) validate() error {
	if _, ok := allEvents[invocation.Event]; !ok {
		return fmt.Errorf("dahook: invalid lifecycle event %q", invocation.Event)
	}
	if strings.TrimSpace(invocation.SessionID) == "" || strings.TrimSpace(invocation.CWD) == "" {
		return fmt.Errorf("dahook: hook invocation requires session id and working directory")
	}
	requireString := func(key string) error {
		value, ok := invocation.Data[key].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("dahook: %s requires %s", invocation.Event, key)
		}
		return nil
	}
	requireStringValue := func(key string) error {
		if _, ok := invocation.Data[key].(string); !ok {
			return fmt.Errorf("dahook: %s requires %s", invocation.Event, key)
		}
		return nil
	}
	requireOneOf := func(key string, allowed ...string) error {
		if err := requireString(key); err != nil {
			return err
		}
		value := invocation.Data[key].(string)
		for _, item := range allowed {
			if value == item {
				return nil
			}
		}
		return fmt.Errorf("dahook: %s has invalid %s", invocation.Event, key)
	}
	switch invocation.Event {
	case SessionStart:
		return requireOneOf("source", "startup", "resume", "clear", "compact")
	case UserPromptSubmit:
		return requireString("prompt")
	case SessionEnd:
		return requireOneOf("reason", "clear", "resume", "prompt_input_exit", "other")
	case Notification:
		if err := requireOneOf("notification_type", "permission_required", "agent_needs_input", "agent_completed"); err != nil {
			return err
		}
		return requireString("message")
	case PermissionRequest, PreToolUse:
		if err := requireString("tool_name"); err != nil {
			return err
		}
		if _, ok := invocation.Data["tool_input"]; !ok {
			return fmt.Errorf("dahook: %s requires tool_input", invocation.Event)
		}
		return nil
	case PostToolUse:
		if err := requireString("tool_name"); err != nil {
			return err
		}
		if _, ok := invocation.Data["tool_response"]; !ok {
			return fmt.Errorf("dahook: PostToolUse requires tool_response")
		}
		return nil
	case PostToolUseFailure:
		if err := requireString("tool_name"); err != nil {
			return err
		}
		return requireString("error")
	case PreCompact:
		return requireOneOf("trigger", "manual", "auto")
	case Stop:
		return requireStringValue("last_assistant_message")
	case SubagentStart:
		if invocation.AgentID == "" || invocation.AgentType == "" {
			return fmt.Errorf("dahook: SubagentStart requires agent identity")
		}
		return requireString("agent_name")
	case SubagentStop:
		if invocation.AgentID == "" || invocation.AgentType == "" || invocation.AgentTranscriptPath == "" {
			return fmt.Errorf("dahook: SubagentStop requires agent identity and transcript")
		}
		if err := requireString("agent_name"); err != nil {
			return err
		}
		return requireStringValue("last_assistant_message")
	}
	return nil
}

// Owner reports which side originates an event.
type Owner string

const (
	ClientOwner Owner = "client"
	ServerOwner Owner = "server"
)

// EventOwner returns the protocol owner of event. Invalid events panic because
// they are static programmer configuration.
func EventOwner(event Event) Owner {
	if _, ok := allEvents[event]; !ok {
		panic("dahook: invalid lifecycle event: " + string(event))
	}
	switch event {
	case PreToolUse, PostToolUse, PostToolUseFailure, PreCompact, Stop, SubagentStart, SubagentStop:
		return ServerOwner
	default:
		return ClientOwner
	}
}

// Invocation is the JSON object sent to a hook. Data supplies event-specific
// fields; reserved envelope fields are always supplied by the host.
type Invocation struct {
	Event               Event
	SessionID           string
	TranscriptPath      string
	CWD                 string
	PromptID            string
	PermissionMode      string
	Effort              string
	AgentID             string
	AgentType           string
	AgentTranscriptPath string
	Data                map[string]any
}

func (invocation Invocation) envelope() (map[string]any, error) {
	if _, ok := allEvents[invocation.Event]; !ok {
		panic("dahook: invalid lifecycle event: " + string(invocation.Event))
	}
	value := cloneMap(invocation.Data)
	reserved := map[string]any{
		"hook_event_name": invocation.Event, "session_id": invocation.SessionID,
		"transcript_path": invocation.TranscriptPath, "cwd": invocation.CWD,
	}
	optional := map[string]string{
		"prompt_id": invocation.PromptID, "permission_mode": invocation.PermissionMode,
		"effort": invocation.Effort, "agent_id": invocation.AgentID,
		"agent_type": invocation.AgentType, "agent_transcript_path": invocation.AgentTranscriptPath,
	}
	for key, item := range reserved {
		value[key] = item
	}
	for key, item := range optional {
		if item != "" {
			value[key] = item
		}
	}
	return value, nil
}

func cloneMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input)+8)
	for key, value := range input {
		result[key] = value
	}
	return result
}

// PermissionDecision is the normalized permission effect of a hook.
type PermissionDecision string

const (
	PermissionNone  PermissionDecision = ""
	PermissionAllow PermissionDecision = "allow"
	PermissionDeny  PermissionDecision = "deny"
	PermissionAsk   PermissionDecision = "ask"
	PermissionDefer PermissionDecision = "defer"
)

// Decision is the deterministic reduction of every matching handler. All
// handlers run even when an earlier, higher-precedence handler stops the event.
type Decision struct {
	Continue               bool               `json:"continue"`
	StopReason             string             `json:"stopReason,omitempty"`
	Permission             PermissionDecision `json:"permissionDecision,omitempty"`
	PermissionReason       string             `json:"permissionDecisionReason,omitempty"`
	AdditionalContext      []string           `json:"additionalContext,omitempty"`
	SystemMessages         []string           `json:"systemMessages,omitempty"`
	SuppressOriginalPrompt bool               `json:"suppressOriginalPrompt,omitempty"`
	ContinueLoop           bool               `json:"continueLoop,omitempty"`
	Diagnostics            []Diagnostic       `json:"diagnostics,omitempty"`
}

// Diagnostic is bounded, non-secret operational information.
type Diagnostic struct {
	HandlerID string `json:"handlerId,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// InvocationRequest is the versioned server-to-client interrupt payload.
type InvocationRequest struct {
	ProtocolVersion int        `json:"protocol_version"`
	InvocationID    string     `json:"invocation_id"`
	SnapshotID      string     `json:"snapshot_id"`
	RunID           string     `json:"run_id"`
	Invocation      Invocation `json:"invocation"`
	Deadline        time.Time  `json:"deadline"`
	CapabilityMAC   string     `json:"capability_mac"`
}

// InvocationResponse fulfills exactly one request from exactly one snapshot.
type InvocationResponse struct {
	ProtocolVersion int      `json:"protocol_version"`
	InvocationID    string   `json:"invocation_id"`
	SnapshotID      string   `json:"snapshot_id"`
	Decision        Decision `json:"decision"`
	CapabilityMAC   string   `json:"capability_mac"`
}

// Interrupt is the stable graph-interrupt envelope.
type Interrupt struct {
	Type    string            `json:"type"`
	Request InvocationRequest `json:"request"`
}

// MarshalJSON keeps Invocation's event-specific data flattened on the wire.
func (invocation Invocation) MarshalJSON() ([]byte, error) {
	value, err := invocation.envelope()
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// UnmarshalJSON restores the flattened transport envelope and retains unknown
// event-specific fields in Data.
func (invocation *Invocation) UnmarshalJSON(raw []byte) error {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	decodeString := func(key string, target *string) error {
		item, ok := value[key]
		if !ok {
			return nil
		}
		delete(value, key)
		return json.Unmarshal(item, target)
	}
	var event Event
	if item, ok := value["hook_event_name"]; ok {
		delete(value, "hook_event_name")
		if err := json.Unmarshal(item, &event); err != nil {
			return err
		}
	}
	if _, ok := allEvents[event]; !ok {
		return fmt.Errorf("dahook: invalid lifecycle event %q", event)
	}
	invocation.Event = event
	for key, target := range map[string]*string{
		"session_id": &invocation.SessionID, "transcript_path": &invocation.TranscriptPath,
		"cwd": &invocation.CWD, "prompt_id": &invocation.PromptID,
		"permission_mode": &invocation.PermissionMode, "effort": &invocation.Effort,
		"agent_id": &invocation.AgentID, "agent_type": &invocation.AgentType,
		"agent_transcript_path": &invocation.AgentTranscriptPath,
	} {
		if err := decodeString(key, target); err != nil {
			return err
		}
	}
	invocation.Data = make(map[string]any, len(value))
	for key, item := range value {
		var decoded any
		if err := json.Unmarshal(item, &decoded); err != nil {
			return err
		}
		invocation.Data[key] = decoded
	}
	return nil
}

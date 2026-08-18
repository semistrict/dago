package dacode

import (
	"encoding/json"
	"strings"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

const (
	hookToolOutputLimit            = 2_000
	hookToolOutputTruncationMarker = "...[output truncated]"
)

type hookToolUsePayload struct {
	ToolName string         `json:"tool_name"`
	ToolID   string         `json:"tool_id"`
	ToolArgs map[string]any `json:"tool_args"`
}

type hookToolResultPayload struct {
	ToolName   string         `json:"tool_name"`
	ToolID     string         `json:"tool_id,omitempty"`
	ToolArgs   map[string]any `json:"tool_args"`
	ToolStatus string         `json:"tool_status"`
	ToolOutput string         `json:"tool_output"`
}

type hookToolErrorPayload struct {
	ToolNames []string `json:"tool_names"`
}

func buildHookToolUsePayload(call damessage.ToolCall) hookToolUsePayload {
	return hookToolUsePayload{ToolName: call.Name, ToolID: call.ID, ToolArgs: hookToolArguments(call.Arguments)}
}

func buildHookToolResultPayload(call damessage.ToolCall, result datool.Result, executionError error) hookToolResultPayload {
	status := normalizeHookToolStatus(result.Status, executionError)
	output := hookToolResultText(result)
	if executionError != nil {
		output = executionError.Error()
	}
	return hookToolResultPayload{
		ToolName: call.Name, ToolID: call.ID, ToolArgs: hookToolArguments(call.Arguments),
		ToolStatus: status, ToolOutput: boundHookToolOutput(output),
	}
}

func buildHookToolErrorPayload(toolName string) hookToolErrorPayload {
	return hookToolErrorPayload{ToolNames: []string{toolName}}
}

func hookToolArguments(raw json.RawMessage) map[string]any {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return map[string]any{}
	}
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{"value": value}
}

func normalizeHookToolStatus(status damessage.ToolStatus, executionError error) string {
	if executionError != nil || status == damessage.ToolStatusError {
		return string(damessage.ToolStatusError)
	}
	if status == "" || status == damessage.ToolStatusSuccess {
		return string(damessage.ToolStatusSuccess)
	}
	return string(damessage.ToolStatusError)
}

func hookToolResultText(result datool.Result) string {
	message := damessage.Message{Content: result.Content}
	if text := message.TextContent(); text != "" {
		return text
	}
	if len(result.Content) == 0 {
		return ""
	}
	encoded, err := json.Marshal(result.Content)
	if err != nil {
		return "<tool output could not be rendered>"
	}
	return string(encoded)
}

func boundHookToolOutput(value string) string {
	value = strings.ReplaceAll(value, "\x00", "")
	runes := []rune(value)
	if len(runes) <= hookToolOutputLimit {
		return value
	}
	marker := []rune(hookToolOutputTruncationMarker)
	keep := max(hookToolOutputLimit-len(marker), 0)
	return string(runes[:keep]) + hookToolOutputTruncationMarker
}

package daserver

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
)

func inputToAgent(request createRunRequest, threadID string) ([]dagent.RunOption, error) {
	options := []dagent.RunOption{dagent.FromCheckpoint(checkpointConfig(request, threadID))}
	var state dastate.Values
	if request.Command != nil {
		if resume, exists := request.Command["resume"]; exists {
			options = append(options, dagent.Resume(resume))
		}
		if update, exists := request.Command["update"]; exists {
			values, err := inputValues(update)
			if err != nil {
				return nil, fmt.Errorf("command update: %w", err)
			}
			state = values
		}
	}
	if request.Input == nil {
		if len(state) > 0 {
			options = append(options, dagent.WithState(state))
		}
		return options, nil
	}
	values, err := inputValues(request.Input)
	if err != nil {
		return nil, err
	}
	if rawMessages, exists := values[dagent.MessagesKey]; exists {
		messages, err := protocolMessages(rawMessages)
		if err != nil {
			return nil, fmt.Errorf("input messages: %w", err)
		}
		options = append(options, dagent.Messages(messages))
		delete(values, dagent.MessagesKey)
	}
	if len(values) > 0 {
		if state == nil {
			state = dastate.Values{}
		}
		for key, value := range values {
			state[key] = value
		}
	}
	if len(state) > 0 {
		options = append(options, dagent.WithState(state))
	}
	return options, nil
}

func checkpointConfig(request createRunRequest, threadID string) (config dacheckpoint.Config) {
	config.ThreadID = threadID
	config.CheckpointID = request.CheckpointID
	if request.Checkpoint != nil {
		config.Namespace = request.Checkpoint.Namespace
		if request.Checkpoint.CheckpointID != "" {
			config.CheckpointID = request.Checkpoint.CheckpointID
		}
	}
	return config
}

func inputValues(value any) (dastate.Values, error) {
	if text, ok := value.(string); ok {
		return dastate.Values{dagent.MessagesKey: []any{map[string]any{"role": "user", "content": text}}}, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("input must be an object or string, got %T", value)
	}
	result := make(dastate.Values, len(object))
	for key, item := range object {
		result[key] = item
	}
	return result, nil
}

func protocolStateValues(value any) (dastate.Values, error) {
	if value == nil {
		return dastate.Values{}, nil
	}
	values, err := inputValues(value)
	if err != nil {
		return nil, err
	}
	if raw, exists := values[dagent.MessagesKey]; exists {
		messages, err := protocolMessages(raw)
		if err != nil {
			return nil, fmt.Errorf("messages: %w", err)
		}
		values[dagent.MessagesKey] = messages
	}
	return values, nil
}

func protocolMessages(value any) ([]damessage.Message, error) {
	if messages, ok := value.([]damessage.Message); ok {
		result := make([]damessage.Message, len(messages))
		for index := range messages {
			result[index] = messages[index].Clone()
		}
		return result, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("messages must be an array, got %T", value)
	}
	result := make([]damessage.Message, 0, len(items))
	for index, item := range items {
		message, err := protocolMessage(item)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		result = append(result, message)
	}
	return result, nil
}

func protocolMessage(value any) (damessage.Message, error) {
	if text, ok := value.(string); ok {
		return damessage.Human(text), nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return damessage.Message{}, fmt.Errorf("must be an object, got %T", value)
	}
	roleText := stringValue(object["role"])
	if roleText == "" {
		roleText = stringValue(object["type"])
	}
	role, err := protocolRole(roleText)
	if err != nil {
		return damessage.Message{}, err
	}
	message := damessage.Message{
		ID: stringValue(object["id"]), Role: role, Name: stringValue(object["name"]),
		ToolCallID: stringValue(object["tool_call_id"]),
	}
	message.Content, err = protocolContent(object["content"])
	if err != nil {
		return damessage.Message{}, err
	}
	if calls, exists := object["tool_calls"]; exists {
		message.ToolCalls, err = protocolToolCalls(calls)
		if err != nil {
			return damessage.Message{}, err
		}
	}
	if status := stringValue(object["status"]); status != "" {
		message.ToolStatus = damessage.ToolStatus(status)
	}
	if role == damessage.RoleTool && message.ToolStatus == "" {
		message.ToolStatus = damessage.ToolStatusSuccess
	}
	if err := message.Validate(); err != nil {
		return damessage.Message{}, err
	}
	return message, nil
}

func protocolRole(value string) (damessage.Role, error) {
	switch strings.ToLower(value) {
	case "human", "user":
		return damessage.RoleHuman, nil
	case "assistant", "ai":
		return damessage.RoleAssistant, nil
	case "system":
		return damessage.RoleSystem, nil
	case "tool":
		return damessage.RoleTool, nil
	default:
		return "", fmt.Errorf("unsupported message role %q", value)
	}
}

func protocolContent(value any) ([]damessage.ContentBlock, error) {
	switch content := value.(type) {
	case nil:
		return nil, nil
	case string:
		return []damessage.ContentBlock{{Type: damessage.BlockText, Text: content}}, nil
	case []any:
		blocks := make([]damessage.ContentBlock, 0, len(content))
		for _, raw := range content {
			if text, ok := raw.(string); ok {
				blocks = append(blocks, damessage.ContentBlock{Type: damessage.BlockText, Text: text})
				continue
			}
			object, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content block must be an object or string")
			}
			typeName := stringValue(object["type"])
			if typeName == "text" || typeName == "text_delta" {
				blocks = append(blocks, damessage.ContentBlock{Type: damessage.BlockText, Text: stringValue(object["text"])})
				continue
			}
			encoded, err := json.Marshal(object)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, damessage.ContentBlock{Type: damessage.BlockNonStandard, NonStandard: encoded})
		}
		return blocks, nil
	default:
		return nil, fmt.Errorf("content must be a string or array, got %T", value)
	}
}

func protocolToolCalls(value any) ([]damessage.ToolCall, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("tool_calls must be an array")
	}
	result := make([]damessage.ToolCall, 0, len(items))
	for _, raw := range items {
		object, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tool call must be an object")
		}
		arguments := object["args"]
		if arguments == nil {
			arguments = object["arguments"]
		}
		var encoded json.RawMessage
		if text, ok := arguments.(string); ok {
			encoded = json.RawMessage(text)
		} else {
			data, err := json.Marshal(arguments)
			if err != nil {
				return nil, err
			}
			encoded = data
		}
		if !json.Valid(encoded) {
			return nil, fmt.Errorf("tool call arguments are invalid JSON")
		}
		result = append(result, damessage.ToolCall{
			ID: stringValue(object["id"]), Name: stringValue(object["name"]), Arguments: encoded,
		})
	}
	return result, nil
}

func stateToProtocol(values dastate.Values) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		if key == dagent.MessagesKey {
			if messages, err := protocolMessages(value); err == nil {
				items := make([]any, len(messages))
				for index, message := range messages {
					items[index] = messageToProtocol(message)
				}
				result[key] = items
				continue
			}
		}
		result[key] = value
	}
	return result
}

func messageToProtocol(message damessage.Message) map[string]any {
	typeName := string(message.Role)
	switch message.Role {
	case damessage.RoleAssistant:
		typeName = "ai"
	case damessage.RoleHuman:
		typeName = "human"
	}
	content := any(message.TextContent())
	if len(message.Content) > 1 || (len(message.Content) == 1 && message.Content[0].Type != damessage.BlockText) {
		content = message.Content
	}
	result := map[string]any{
		"id": message.ID, "type": typeName, "content": content,
		"name": nullableString(message.Name), "additional_kwargs": map[string]any{},
		"response_metadata": rawMap(message.ResponseMetadata),
	}
	if message.Role == damessage.RoleAssistant {
		calls := make([]any, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			var args any
			_ = json.Unmarshal(call.Arguments, &args)
			calls = append(calls, map[string]any{"name": call.Name, "args": args, "id": call.ID, "type": "tool_call"})
		}
		result["tool_calls"] = calls
		result["invalid_tool_calls"] = []any{}
	}
	if message.Role == damessage.RoleTool {
		result["tool_call_id"] = message.ToolCallID
		result["status"] = message.ToolStatus
		if len(message.Artifact) > 0 {
			var artifact any
			_ = json.Unmarshal(message.Artifact, &artifact)
			result["artifact"] = artifact
		}
	}
	return result
}

func rawMap(values map[string]json.RawMessage) map[string]any {
	result := make(map[string]any, len(values))
	for key, raw := range values {
		var value any
		if json.Unmarshal(raw, &value) == nil {
			result[key] = value
		}
	}
	return result
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

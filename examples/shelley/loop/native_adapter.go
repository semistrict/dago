package loop

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	dtool "github.com/semistrict/dago/tool"

	"shelley.exe.dev/llm"
)

const (
	shelleyMessageMetadata  = "shelley.message.v1"
	shelleyToolArtifact     = "shelley.tool_result.v1"
	openAIReasoningStateKey = "openai.responses.reasoning"
	displayWidthKey         = "shelley.display_width"
	displayHeightKey        = "shelley.display_height"
)

type openAIReasoningState struct {
	ID               string                                `json:"id"`
	Summary          []llm.OpenAIResponsesReasoningSummary `json:"summary"`
	EncryptedContent string                                `json:"encrypted_content"`
}

type toolArtifactEnvelope struct {
	Version    int                 `json:"version"`
	Kind       string              `json:"kind"`
	Content    llm.Content         `json:"content"`
	OtherUsage []llm.PurposedUsage `json:"other_usage,omitempty"`
}

// validateTools validates the Dago-native executable set before agent creation.
func validateTools(items []dtool.Tool) ([]dtool.Tool, error) {
	names := make(map[string]struct{}, len(items))
	result := make([]dtool.Tool, 0, len(items))
	for _, item := range items {
		if item == nil {
			return nil, fmt.Errorf("native tool is nil")
		}
		definition := item.Definition()
		if err := definition.Validate(); err != nil {
			return nil, fmt.Errorf("native tool %q: %w", definition.Name, err)
		}
		if _, exists := names[definition.Name]; exists {
			return nil, fmt.Errorf("duplicate native tool %q", definition.Name)
		}
		names[definition.Name] = struct{}{}
		result = append(result, item)
	}
	return result, nil
}

// messagesToDago converts persisted Shelley context into Dago checkpoint state.
func messagesToDago(items []llm.Message) ([]dmessage.Message, error) {
	var result []dmessage.Message
	for _, item := range items {
		if item.ExcludedFromContext || item.ErrorType != llm.ErrorTypeNone {
			continue
		}
		role := dmessage.RoleHuman
		if item.Role == llm.MessageRoleAssistant {
			role = dmessage.RoleAssistant
		}
		base := dmessage.Message{Role: role}
		var toolMessages []dmessage.Message
		for _, content := range item.Content {
			switch content.Type {
			case llm.ContentTypeToolUse:
				base.ToolCalls = append(base.ToolCalls, dmessage.ToolCall{
					ID: content.ID, Name: content.ToolName,
					Arguments: append(json.RawMessage(nil), content.ToolInput...),
				})
			case llm.ContentTypeToolResult:
				artifact, err := json.Marshal(toolArtifactEnvelope{Version: 1, Kind: shelleyToolArtifact, Content: content})
				if err != nil {
					return nil, err
				}
				status := dmessage.ToolStatusSuccess
				if content.ToolError {
					status = dmessage.ToolStatusError
				}
				toolMessages = append(toolMessages, dmessage.Message{
					Role: dmessage.RoleTool, ToolCallID: content.ToolUseID,
					ToolStatus: status, Content: contentToDago(content.ToolResult), Artifact: artifact,
				})
			default:
				base.Content = append(base.Content, contentToDago([]llm.Content{content})...)
			}
		}
		if len(base.Content) > 0 || len(base.ToolCalls) > 0 || len(toolMessages) == 0 {
			fragment := item
			fragment.Content = removeToolResults(item.Content)
			encoded, err := json.Marshal(fragment)
			if err != nil {
				return nil, err
			}
			base.Metadata = map[string]json.RawMessage{shelleyMessageMetadata: encoded}
			result = append(result, base)
		}
		result = append(result, toolMessages...)
	}
	return result, nil
}

// messagesFromDago returns the Shelley representation used by the existing UI
// projection and provider adapters.
func messagesFromDago(items []dmessage.Message) ([]llm.Message, error) {
	result := make([]llm.Message, 0, len(items))
	for _, item := range items {
		if raw := item.Metadata[shelleyMessageMetadata]; len(raw) > 0 {
			var exact llm.Message
			if err := json.Unmarshal(raw, &exact); err != nil {
				return nil, fmt.Errorf("decode Shelley message metadata: %w", err)
			}
			result = append(result, exact)
			continue
		}
		if item.Role == dmessage.RoleSystem || item.Role == dmessage.RoleRemove {
			continue
		}
		if item.Role == dmessage.RoleTool {
			content, _, err := toolResultFromDago(item)
			if err != nil {
				return nil, err
			}
			result = append(result, llm.Message{Role: llm.MessageRoleUser, Content: []llm.Content{content}})
			continue
		}
		converted := llm.Message{Role: llm.MessageRoleUser}
		if item.Role == dmessage.RoleAssistant {
			converted.Role = llm.MessageRoleAssistant
			converted.EndOfTurn = len(item.ToolCalls) == 0
		}
		converted.Content = contentFromDago(item.Content)
		for _, call := range item.ToolCalls {
			converted.Content = append(converted.Content, llm.Content{
				ID: call.ID, Type: llm.ContentTypeToolUse, ToolName: call.Name,
				ToolInput: append(json.RawMessage(nil), call.Arguments...),
			})
		}
		result = append(result, converted)
	}
	return combineToolResultMessages(result), nil
}

// responseFromDago recovers exact provider response metadata when available and
// otherwise projects a native assistant message into Shelley's UI record.
func responseFromDago(item dmessage.Message) (*llm.Response, bool, error) {
	if item.Role != dmessage.RoleAssistant {
		return nil, false, nil
	}
	if len(item.ResponseMetadata[dmodel.ResponseMetadataKey]) == 0 {
		return nil, false, nil
	}
	response := llm.Response{
		ID: item.ID, Role: llm.MessageRoleAssistant,
		Content: contentFromDago(item.Content), StopReason: llm.StopReasonEndTurn,
	}
	finishReason, refusal := dmodel.Outcome(item)
	switch finishReason {
	case dmodel.FinishReasonMaxTokens:
		response.StopReason = llm.StopReasonMaxTokens
	case dmodel.FinishReasonRefusal:
		response.StopReason = llm.StopReasonRefusal
		if refusal != nil {
			response.RefusalDetails = &llm.RefusalDetails{Category: refusal.Category, Explanation: refusal.Explanation}
		}
	case dmodel.FinishReasonToolCalls:
		response.StopReason = llm.StopReasonToolUse
	}
	for _, call := range item.ToolCalls {
		response.Content = append(response.Content, llm.Content{
			ID: call.ID, Type: llm.ContentTypeToolUse, ToolName: call.Name,
			ToolInput: append(json.RawMessage(nil), call.Arguments...),
		})
	}
	if len(item.ToolCalls) > 0 {
		response.StopReason = llm.StopReasonToolUse
	}
	if item.Usage != nil {
		response.Usage.InputTokens = uint64(max(0, item.Usage.InputTokens))
		response.Usage.OutputTokens = uint64(max(0, item.Usage.OutputTokens))
		response.Usage.CacheCreationInputTokens = uint64(max(0, item.Usage.InputDetails["cache_creation"]))
		response.Usage.CacheReadInputTokens = uint64(max(0, item.Usage.InputDetails["cache_read"]))
		response.Usage.CostUSD = item.Usage.CostUSD
		response.Usage.Model = item.Usage.Model
		response.Usage.URL = item.Usage.URL
	}
	return &response, true, nil
}

// toolResultFromDago recovers one exact tool result and its indirect usage.
func toolResultFromDago(item dmessage.Message) (llm.Content, []llm.PurposedUsage, error) {
	if len(item.Artifact) > 0 {
		var artifact toolArtifactEnvelope
		if err := json.Unmarshal(item.Artifact, &artifact); err == nil && artifact.Kind == shelleyToolArtifact {
			return artifact.Content, artifact.OtherUsage, nil
		}
	}
	content := llm.Content{
		Type: llm.ContentTypeToolResult, ToolUseID: item.ToolCallID,
		ToolError:  item.ToolStatus == dmessage.ToolStatusError,
		ToolResult: contentFromDago(item.Content),
	}
	return content, purposedUsageFromDago(item.OtherUsage), nil
}

func purposedUsageFromDago(items []dmessage.PurposedUsage) []llm.PurposedUsage {
	if len(items) == 0 {
		return nil
	}
	result := make([]llm.PurposedUsage, 0, len(items))
	for _, item := range items {
		usage := llm.Usage{
			InputTokens:              uint64(max(0, item.InputTokens)),
			CacheCreationInputTokens: uint64(max(0, item.InputDetails["cache_creation"])),
			CacheReadInputTokens:     uint64(max(0, item.InputDetails["cache_read"])),
			OutputTokens:             uint64(max(0, item.OutputTokens)),
			CostUSD:                  item.CostUSD,
			Model:                    item.Model,
			URL:                      item.URL,
		}
		if started, err := time.Parse(time.RFC3339Nano, item.StartedAt); err == nil {
			usage.StartTime = &started
		}
		if finished, err := time.Parse(time.RFC3339Nano, item.FinishedAt); err == nil {
			usage.EndTime = &finished
		}
		result = append(result, llm.PurposedUsage{Purpose: item.Purpose, Usage: usage})
	}
	return result
}

func contentToDago(items []llm.Content) []dmessage.ContentBlock {
	result := make([]dmessage.ContentBlock, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case llm.ContentTypeText:
			if item.MediaType != "" && item.Data != "" {
				data, err := base64.StdEncoding.DecodeString(item.Data)
				if err != nil {
					// Old Shelley databases may contain pre-native raw image data.
					// Preserve it during checkpoint hydration; newly projected data
					// is always canonical base64.
					data = []byte(item.Data)
				}
				width, _ := json.Marshal(item.DisplayWidth)
				height, _ := json.Marshal(item.DisplayHeight)
				result = append(result, dmessage.ContentBlock{
					Type: dmessage.BlockImage, ID: item.ID, URL: item.Text,
					MIMEType: item.MediaType, Data: data,
					Extra: map[string]json.RawMessage{displayWidthKey: width, displayHeightKey: height},
				})
				continue
			}
			result = append(result, dmessage.ContentBlock{Type: dmessage.BlockText, ID: item.ID, Text: item.Text})
		case llm.ContentTypeThinking, llm.ContentTypeRedactedThinking:
			block := dmessage.ContentBlock{Type: dmessage.BlockReasoning, ID: item.ID, Reasoning: item.Thinking}
			if item.OpenAIResponsesReasoning != nil {
				state := openAIReasoningState{
					ID: item.OpenAIResponsesReasoning.ID, Summary: append([]llm.OpenAIResponsesReasoningSummary(nil), item.OpenAIResponsesReasoning.Summary...),
					EncryptedContent: item.OpenAIResponsesReasoning.EncryptedContent,
				}
				if raw, err := json.Marshal(state); err == nil {
					block.Extra = map[string]json.RawMessage{openAIReasoningStateKey: raw}
				}
			}
			result = append(result, block)
		case llm.ContentTypeToolUse, llm.ContentTypeToolResult:
			// Calls/results use the dedicated Dago fields.
		default:
			raw, err := json.Marshal(item)
			if err == nil {
				result = append(result, dmessage.ContentBlock{Type: dmessage.BlockNonStandard, ID: item.ID, NonStandard: raw})
			}
		}
	}
	return result
}

func contentFromDago(items []dmessage.ContentBlock) []llm.Content {
	result := make([]llm.Content, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case dmessage.BlockText:
			content := llm.Content{ID: item.ID, Type: llm.ContentTypeText, Text: item.Text}
			if len(item.Citations) > 0 {
				citations := make([]map[string]any, 0, len(item.Citations))
				for _, citation := range item.Citations {
					citations = append(citations, map[string]any{
						"type": "url_citation", "url": citation.URL, "title": citation.Title,
						"start_index": citation.StartIndex, "end_index": citation.EndIndex,
						"cited_text": citation.CitedText,
					})
				}
				content.Citations, _ = json.Marshal(citations)
			}
			result = append(result, content)
		case dmessage.BlockReasoning:
			content := llm.Content{ID: item.ID, Type: llm.ContentTypeThinking, Thinking: item.Reasoning}
			if raw := item.Extra[openAIReasoningStateKey]; len(raw) > 0 {
				var state openAIReasoningState
				if json.Unmarshal(raw, &state) == nil {
					content.OpenAIResponsesReasoning = &llm.OpenAIResponsesReasoningMetadata{
						ID: state.ID, EncryptedContent: state.EncryptedContent,
						Summary: append([]llm.OpenAIResponsesReasoningSummary(nil), state.Summary...),
					}
				}
			}
			result = append(result, content)
		case dmessage.BlockImage:
			var width, height int
			_ = json.Unmarshal(item.Extra[displayWidthKey], &width)
			_ = json.Unmarshal(item.Extra[displayHeightKey], &height)
			result = append(result, llm.Content{
				ID: item.ID, Type: llm.ContentTypeText, Text: item.URL,
				MediaType: item.MIMEType, Data: base64.StdEncoding.EncodeToString(item.Data),
				DisplayWidth: width, DisplayHeight: height,
			})
		case dmessage.BlockServerTool:
			arguments := item.Extra["arguments"]
			if len(arguments) == 0 {
				arguments = json.RawMessage(`{}`)
			}
			result = append(result, llm.Content{ID: item.ID, Type: llm.ContentTypeServerToolUse, ToolName: item.Name, ToolInput: append(json.RawMessage(nil), arguments...)})
		case dmessage.BlockSearchResult:
			result = append(result, llm.Content{ID: item.ID, Type: llm.ContentTypeWebSearchResult, Title: item.Name, URL: item.URL})
		case dmessage.BlockNonStandard:
			var exact llm.Content
			if json.Unmarshal(item.NonStandard, &exact) == nil {
				result = append(result, exact)
			}
		}
	}
	return result
}

func removeToolResults(items []llm.Content) []llm.Content {
	result := make([]llm.Content, 0, len(items))
	for _, item := range items {
		if item.Type != llm.ContentTypeToolResult {
			result = append(result, item)
		}
	}
	return result
}

func combineToolResultMessages(items []llm.Message) []llm.Message {
	result := make([]llm.Message, 0, len(items))
	for _, item := range items {
		if len(result) > 0 && isToolResultOnly(result[len(result)-1]) && isToolResultOnly(item) {
			result[len(result)-1].Content = append(result[len(result)-1].Content, item.Content...)
			continue
		}
		result = append(result, item)
	}
	return result
}

func isToolResultOnly(item llm.Message) bool {
	if item.Role != llm.MessageRoleUser || len(item.Content) == 0 {
		return false
	}
	for _, content := range item.Content {
		if content.Type != llm.ContentTypeToolResult {
			return false
		}
	}
	return true
}

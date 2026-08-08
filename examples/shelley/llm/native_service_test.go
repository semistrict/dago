package llm

import (
	"context"
	"encoding/json"
	"testing"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
)

func TestServiceConvertsMessagesToolsAndResponse(t *testing.T) {
	chat := modeltest.New(dmodel.Profile{Provider: "test", Model: "dago-test", ContextWindow: 12345}, modeltest.Step{
		Check: func(request dmodel.Request) error {
			if len(request.Messages) != 2 || request.Messages[0].Role != dmessage.RoleSystem || request.Messages[1].TextContent() != "hello" {
				t.Fatalf("messages = %#v", request.Messages)
			}
			if len(request.Tools) != 1 || request.Tools[0].Name != "lookup" {
				t.Fatalf("tools = %#v", request.Tools)
			}
			return nil
		},
		Response: dmodel.Response{Message: dmessage.Message{
			ID:        "response-1",
			Role:      dmessage.RoleAssistant,
			Content:   []dmessage.ContentBlock{{Type: dmessage.BlockText, Text: "working"}},
			ToolCalls: []dmessage.ToolCall{{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"q":"go"}`)}},
			Usage:     &dmessage.Usage{InputTokens: 3, OutputTokens: 4},
		}},
	})
	service, err := NewNativeService(chat)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Do(context.Background(), &Request{
		System:   []SystemContent{{Type: "text", Text: "system"}},
		Messages: []Message{UserStringMessage("hello")},
		Tools:    []*Tool{{Name: "lookup", Description: "look something up", InputSchema: MustSchema(`{"type":"object","properties":{"q":{"type":"string"}}}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "response-1" || response.StopReason != StopReasonToolUse || response.Usage.InputTokens != 3 || response.Usage.OutputTokens != 4 {
		t.Fatalf("response = %#v", response)
	}
	if len(response.Content) != 2 || response.Content[1].ToolName != "lookup" || string(response.Content[1].ToolInput) != `{"q":"go"}` {
		t.Fatalf("content = %#v", response.Content)
	}
}

func TestServiceStreamsTextAndToolInput(t *testing.T) {
	chat := modeltest.New(dmodel.Profile{Provider: "test", Model: "stream"}, modeltest.Step{Chunks: []dmodel.Chunk{
		{MessageDelta: dmessage.Assistant("hel")},
		{MessageDelta: dmessage.Message{Role: dmessage.RoleAssistant, Content: []dmessage.ContentBlock{{Type: dmessage.BlockText, Text: "lo"}}, ToolCalls: []dmessage.ToolCall{{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"q":"go"}`)}}}},
	}})
	service, err := NewNativeService(chat)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []StreamDelta
	response, err := service.Do(context.Background(), &Request{Messages: []Message{UserStringMessage("hello")}, OnStream: func(delta StreamDelta) {
		deltas = append(deltas, delta)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 3 || deltas[0].Text != "hel" || deltas[1].Text != "lo" || deltas[2].Type != "tool_input" {
		t.Fatalf("deltas = %#v", deltas)
	}
	if len(response.Content) != 2 || response.Content[0].Text != "hello" || response.Content[1].ToolName != "lookup" {
		t.Fatalf("response = %#v", response)
	}
}

func TestServiceConvertsToolResults(t *testing.T) {
	chat := modeltest.New(dmodel.Profile{Provider: "test", Model: "tools"}, modeltest.Step{
		Check: func(request dmodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Role != dmessage.RoleTool || last.ToolCallID != "call-1" || last.ToolStatus != dmessage.ToolStatusError || last.TextContent() != "failed" {
				t.Fatalf("tool result = %#v", last)
			}
			return nil
		},
		Response: dmodel.Response{Message: dmessage.Assistant("done")},
	})
	service, err := NewNativeService(chat)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Do(context.Background(), &Request{Messages: []Message{{Role: MessageRoleUser, Content: []Content{{
		Type: ContentTypeToolResult, ToolUseID: "call-1", ToolError: true, ToolResult: TextContent("failed"),
	}}}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativeServicePreservesReasoningControlAndState(t *testing.T) {
	state := OpenAIResponsesReasoningMetadata{
		ID: "rs_in", EncryptedContent: "opaque-in",
		Summary: []OpenAIResponsesReasoningSummary{{Type: "summary_text", Text: "input thought"}},
	}
	outputState, _ := json.Marshal(nativeOpenAIReasoningState{
		ID: "rs_out", EncryptedContent: "opaque-out",
		Summary: []OpenAIResponsesReasoningSummary{{Type: "summary_text", Text: "output thought"}},
	})
	chat := modeltest.New(dmodel.Profile{Provider: "test", Model: "reasoning", SupportsReasoning: true}, modeltest.Step{
		Check: func(request dmodel.Request) error {
			if request.Reasoning == nil || request.Reasoning.Effort != "high" || request.Reasoning.Summary != "auto" {
				t.Fatalf("reasoning control = %#v", request.Reasoning)
			}
			block := request.Messages[0].Content[0]
			if len(block.Extra[nativeOpenAIReasoningStateKey]) == 0 {
				t.Fatalf("reasoning input state = %#v", block)
			}
			return nil
		},
		Response: dmodel.Response{Message: dmessage.Message{Role: dmessage.RoleAssistant, Content: []dmessage.ContentBlock{{
			Type: dmessage.BlockReasoning, ID: "rs_out", Reasoning: "output thought",
			Extra: map[string]json.RawMessage{nativeOpenAIReasoningStateKey: outputState},
		}}}},
	})
	service, err := NewNativeService(chat)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Do(context.Background(), &Request{
		ThinkingLevel: ThinkingLevelHigh,
		Messages: []Message{{Role: MessageRoleAssistant, Content: []Content{{
			Type: ContentTypeThinking, Thinking: "input thought", OpenAIResponsesReasoning: &state,
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := response.Content[0].OpenAIResponsesReasoning
	if got == nil || got.ID != "rs_out" || got.EncryptedContent != "opaque-out" {
		t.Fatalf("reasoning output state = %#v", got)
	}
}

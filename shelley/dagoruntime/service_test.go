package dagoruntime

import (
	"context"
	"encoding/json"
	"testing"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"

	"shelley.exe.dev/llm"
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
	service, err := NewService(chat)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Do(context.Background(), &llm.Request{
		System:   []llm.SystemContent{{Type: "text", Text: "system"}},
		Messages: []llm.Message{llm.UserStringMessage("hello")},
		Tools:    []*llm.Tool{{Name: "lookup", Description: "look something up", InputSchema: llm.MustSchema(`{"type":"object","properties":{"q":{"type":"string"}}}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "response-1" || response.StopReason != llm.StopReasonToolUse || response.Usage.InputTokens != 3 || response.Usage.OutputTokens != 4 {
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
	service, err := NewService(chat)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []llm.StreamDelta
	response, err := service.Do(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserStringMessage("hello")}, OnStream: func(delta llm.StreamDelta) {
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
	service, err := NewService(chat)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Do(context.Background(), &llm.Request{Messages: []llm.Message{{Role: llm.MessageRoleUser, Content: []llm.Content{{
		Type: llm.ContentTypeToolResult, ToolUseID: "call-1", ToolError: true, ToolResult: llm.TextContent("failed"),
	}}}}})
	if err != nil {
		t.Fatal(err)
	}
}

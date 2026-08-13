package dago

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

func TestCompactConversationKeepsToolExchangeIntact(t *testing.T) {
	messages := []damessage.Message{
		damessage.Human(strings.Repeat("a", 80)),
		{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "call-1", Name: "read"}}},
		{Role: damessage.RoleTool, ToolCallID: "call-1", Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: strings.Repeat("b", 80)}}},
		damessage.Assistant(strings.Repeat("c", 80)),
		damessage.Human(strings.Repeat("d", 80)),
	}
	usage := &damessage.Usage{InputTokens: 10, OutputTokens: 2}
	model := modeltest.New(damodel.Profile{Provider: "test", Model: "summary"}, modeltest.Step{
		Check: func(request damodel.Request) error {
			if len(request.Messages) != 2 || request.Messages[0].Role != damessage.RoleSystem {
				return errors.New("compaction did not send system and history prompts")
			}
			if request.Reasoning == nil || request.Reasoning.Effort != "off" {
				return errors.New("compaction did not preserve reasoning options")
			}
			if !strings.Contains(request.Messages[1].TextContent(), "custom history\n\nSummarize\n\nretain paths") {
				return errors.New("compaction prompt did not include history, prompt, and instructions")
			}
			return nil
		},
		Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: " checkpoint "}}, Usage: usage}},
	})
	formatted := 0
	result, err := CompactConversation(t.Context(), model, messages, ConversationCompactionOptions{
		KeepTokens: 25, SystemPrompt: "system", Prompt: "Summarize", Instructions: "retain paths",
		Reasoning: &damodel.Reasoning{Effort: "off"},
		FormatHistory: func(older []damessage.Message) (string, error) {
			formatted = len(older)
			return "custom history", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cutoff != 4 || formatted != 4 {
		t.Fatalf("cutoff = %d, formatted = %d", result.Cutoff, formatted)
	}
	if result.Summary != "checkpoint" || len(result.Older) != 4 || len(result.Recent) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if result.Usage == nil || result.Usage.InputTokens != 10 || result.Started.IsZero() || result.Finished.IsZero() {
		t.Fatalf("usage/timestamps = %#v, %v, %v", result.Usage, result.Started, result.Finished)
	}
}

func TestCompactConversationSkipsModelWhenHistoryFits(t *testing.T) {
	model := modeltest.New(damodel.Profile{})
	result, err := CompactConversation(t.Context(), model, []damessage.Message{damessage.Human("hello")}, ConversationCompactionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cutoff != 0 || result.Summary != "" || len(result.Recent) != 1 || model.Remaining() != 0 {
		t.Fatalf("result = %#v, remaining model calls = %d", result, model.Remaining())
	}
}

func TestCompactConversationHonorsApplicationRecordBoundaries(t *testing.T) {
	messages := []damessage.Message{
		damessage.Human(strings.Repeat("a", 80)),
		damessage.Assistant(strings.Repeat("b", 80)),
		damessage.Human(strings.Repeat("c", 80)),
		damessage.Assistant(strings.Repeat("d", 80)),
		damessage.Human(strings.Repeat("e", 80)),
	}
	model := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if !strings.Contains(request.Messages[1].TextContent(), "5 records") {
			return errors.New("formatter did not receive constrained record range")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("summary")}})
	result, err := CompactConversation(t.Context(), model, messages, ConversationCompactionOptions{
		KeepTokens: 25, ValidCutoffs: []int{0, 3, 5},
		FormatHistory: func(older []damessage.Message) (string, error) {
			return fmt.Sprintf("%d records", len(older)), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cutoff != 5 || len(result.Recent) != 0 {
		t.Fatalf("constrained result = %#v", result)
	}
}

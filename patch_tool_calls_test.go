package dago

import (
	"testing"

	"github.com/semistrict/dago/message"
)

func TestRepairToolCallHistoryPreservesAdjacentResults(t *testing.T) {
	assistant := message.Assistant("working")
	assistant.ToolCalls = []message.ToolCall{{ID: "first", Name: "lookup"}, {ID: "second", Name: "lookup"}}
	result := message.Tool("first", "one")
	result.Name = "lookup"

	patched, changed := repairToolCallHistory([]message.Message{message.Human("go"), assistant, result, message.Human("continue")})
	if !changed {
		t.Fatal("missing result was not repaired")
	}
	if len(patched) != 5 {
		t.Fatalf("patched messages = %#v", patched)
	}
	if patched[2].Role != message.RoleTool || patched[2].ToolCallID != "first" || patched[2].TextContent() != "one" {
		t.Fatalf("existing result changed: %#v", patched[2])
	}
	if patched[3].Role != message.RoleTool || patched[3].ToolCallID != "second" || patched[3].ToolStatus != message.ToolStatusError {
		t.Fatalf("missing result = %#v", patched[3])
	}
	if patched[4].Role != message.RoleHuman {
		t.Fatalf("synthetic result was not adjacent to its call: %#v", patched)
	}
}

func TestRepairToolCallHistoryRemovesLateAndUnknownResults(t *testing.T) {
	assistant := message.Assistant("working")
	assistant.ToolCalls = []message.ToolCall{{ID: "call", Name: "lookup"}}
	late := message.Tool("call", "late")
	orphan := message.Tool("unknown", "orphan")

	patched, changed := repairToolCallHistory([]message.Message{
		message.Human("go"), assistant, message.Human("cancel"), late, orphan,
	})
	if !changed {
		t.Fatal("invalid results were not repaired")
	}
	if len(patched) != 4 {
		t.Fatalf("patched messages = %#v", patched)
	}
	if patched[2].Role != message.RoleTool || patched[2].ToolCallID != "call" || patched[2].ToolStatus != message.ToolStatusError {
		t.Fatalf("synthetic result = %#v", patched[2])
	}
	if patched[3].Role != message.RoleHuman || patched[3].TextContent() != "cancel" {
		t.Fatalf("late results survived: %#v", patched)
	}
}

func TestRepairToolCallHistoryDoesNotRewriteValidHistory(t *testing.T) {
	assistant := message.Assistant("working")
	assistant.ToolCalls = []message.ToolCall{{ID: "call", Name: "lookup"}}
	result := message.Tool("call", "done")
	messages := []message.Message{message.Human("go"), assistant, result, message.Assistant("finished")}

	patched, changed := repairToolCallHistory(messages)
	if changed {
		t.Fatalf("valid history was marked changed: %#v", patched)
	}
	if len(patched) != len(messages) {
		t.Fatalf("patched messages = %#v", patched)
	}
}

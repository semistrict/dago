package dago

import (
	"testing"

	"github.com/semistrict/dago/damessage"
)

func TestRepairToolCallHistoryPreservesAdjacentResults(t *testing.T) {
	assistant := damessage.Assistant("working")
	assistant.ToolCalls = []damessage.ToolCall{{ID: "first", Name: "lookup"}, {ID: "second", Name: "lookup"}}
	result := damessage.Tool("first", "one")
	result.Name = "lookup"

	patched, changed := repairToolCallHistory([]damessage.Message{damessage.Human("go"), assistant, result, damessage.Human("continue")})
	if !changed {
		t.Fatal("missing result was not repaired")
	}
	if len(patched) != 5 {
		t.Fatalf("patched messages = %#v", patched)
	}
	if patched[2].Role != damessage.RoleTool || patched[2].ToolCallID != "first" || patched[2].TextContent() != "one" {
		t.Fatalf("existing result changed: %#v", patched[2])
	}
	if patched[3].Role != damessage.RoleTool || patched[3].ToolCallID != "second" || patched[3].ToolStatus != damessage.ToolStatusError {
		t.Fatalf("missing result = %#v", patched[3])
	}
	if patched[4].Role != damessage.RoleHuman {
		t.Fatalf("synthetic result was not adjacent to its call: %#v", patched)
	}
}

func TestRepairToolCallHistoryRemovesLateAndUnknownResults(t *testing.T) {
	assistant := damessage.Assistant("working")
	assistant.ToolCalls = []damessage.ToolCall{{ID: "call", Name: "lookup"}}
	late := damessage.Tool("call", "late")
	orphan := damessage.Tool("unknown", "orphan")

	patched, changed := repairToolCallHistory([]damessage.Message{
		damessage.Human("go"), assistant, damessage.Human("cancel"), late, orphan,
	})
	if !changed {
		t.Fatal("invalid results were not repaired")
	}
	if len(patched) != 4 {
		t.Fatalf("patched messages = %#v", patched)
	}
	if patched[2].Role != damessage.RoleTool || patched[2].ToolCallID != "call" || patched[2].ToolStatus != damessage.ToolStatusError {
		t.Fatalf("synthetic result = %#v", patched[2])
	}
	if patched[3].Role != damessage.RoleHuman || patched[3].TextContent() != "cancel" {
		t.Fatalf("late results survived: %#v", patched)
	}
}

func TestRepairToolCallHistoryDoesNotRewriteValidHistory(t *testing.T) {
	assistant := damessage.Assistant("working")
	assistant.ToolCalls = []damessage.ToolCall{{ID: "call", Name: "lookup"}}
	result := damessage.Tool("call", "done")
	messages := []damessage.Message{damessage.Human("go"), assistant, result, damessage.Assistant("finished")}

	patched, changed := repairToolCallHistory(messages)
	if changed {
		t.Fatalf("valid history was marked changed: %#v", patched)
	}
	if len(patched) != len(messages) {
		t.Fatalf("patched messages = %#v", patched)
	}
}

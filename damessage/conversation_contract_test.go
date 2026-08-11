package damessage_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/semistrict/dago/damessage"
)

func overBudget(messages []damessage.Message, maximum float64) error {
	if maximum > 0 && damessage.AggregateUsage(messages).CostUSD >= maximum {
		return fmt.Errorf("usage exceeded $%.2f budget", maximum)
	}
	return nil
}

func resetBudget(maximum float64, messages []damessage.Message) float64 {
	if maximum <= 0 {
		return maximum
	}
	return maximum + damessage.AggregateUsage(messages).CostUSD
}

func TestCumulativeUsageMethods(t *testing.T) {
	first := damessage.Usage{InputTokens: 4, OutputTokens: 2, TotalTokens: 6, CostUSD: 0.1, InputDetails: map[string]int{"cached": 1}}
	second := damessage.Usage{InputTokens: 6, OutputTokens: 3, TotalTokens: 9, CostUSD: 0.2, InputDetails: map[string]int{"cached": 2}}
	messages := []damessage.Message{{Role: damessage.RoleAssistant, Usage: &first}, {Role: damessage.RoleAssistant, Usage: &second}}
	usage := damessage.AggregateUsage(messages)
	if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.TotalTokens != 15 || usage.CostUSD < 0.299 || usage.CostUSD > 0.301 || usage.InputDetails["cached"] != 3 {
		t.Fatalf("usage = %#v", usage)
	}
	usage.InputDetails["cached"] = 99
	if first.InputDetails["cached"] != 1 {
		t.Fatal("aggregate usage aliased source data")
	}
}

func TestLastUsage(t *testing.T) {
	first := damessage.Assistant("first")
	first.Usage = &damessage.Usage{TotalTokens: 2}
	last := damessage.Assistant("last")
	last.Usage = &damessage.Usage{TotalTokens: 9}
	usage, ok := damessage.LastUsage([]damessage.Message{damessage.Human("hi"), first, last})
	if !ok || usage.TotalTokens != 9 {
		t.Fatalf("last usage = %#v, %v", usage, ok)
	}
}

func TestOverBudget(t *testing.T) {
	response := damessage.Assistant("ok")
	response.Usage = &damessage.Usage{CostUSD: 0.25}
	messages := []damessage.Message{response}
	if err := overBudget(messages, 0); err != nil {
		t.Fatalf("unlimited budget = %v", err)
	}
	if err := overBudget(messages, 1); err != nil {
		t.Fatalf("remaining budget = %v", err)
	}
}

func TestResetBudget(t *testing.T) {
	response := damessage.Assistant("ok")
	response.Usage = &damessage.Usage{CostUSD: 1.25}
	if got := resetBudget(5, []damessage.Message{response}); got != 6.25 {
		t.Fatalf("reset budget = %v", got)
	}
}

func TestOverBudgetFunction(t *testing.T) {
	response := damessage.Assistant("ok")
	response.OtherUsage = []damessage.PurposedUsage{{Purpose: "subagent", Usage: damessage.Usage{CostUSD: 0.4}}}
	if err := overBudget([]damessage.Message{response}, 0.5); err != nil {
		t.Fatal(err)
	}
	if err := overBudget([]damessage.Message{response}, 0.4); err == nil {
		t.Fatal("nested model usage did not count toward budget")
	}
}

func TestIncrementToolUse(t *testing.T) {
	messages := []damessage.Message{{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{
		{ID: "1", Name: "bash", Arguments: json.RawMessage(`{}`)},
		{ID: "2", Name: "bash", Arguments: json.RawMessage(`{}`)},
		{ID: "3", Name: "read_file", Arguments: json.RawMessage(`{}`)},
	}}}
	counts := damessage.ToolUseCounts(messages)
	if counts["bash"] != 2 || counts["read_file"] != 1 {
		t.Fatalf("counts = %#v", counts)
	}
}

func TestToolResultCancelContents(t *testing.T) {
	result := damessage.Tool("call-1", "cancelled by user")
	result.Name = "bash"
	result.ToolStatus = damessage.ToolStatusError
	result.Artifact = json.RawMessage(`{"cause":"context canceled"}`)
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	if result.Role != damessage.RoleTool || result.ToolCallID != "call-1" || result.ToolStatus != damessage.ToolStatusError || !json.Valid(result.Artifact) {
		t.Fatalf("cancelled tool result = %#v", result)
	}
}

func TestOverBudgetWithExceeded(t *testing.T) {
	response := damessage.Assistant("expensive")
	response.Usage = &damessage.Usage{CostUSD: 0.75}
	if err := overBudget([]damessage.Message{response}, 0.5); err == nil {
		t.Fatal("expected exceeded budget error")
	}
}

func TestResetBudgetWithUsage(t *testing.T) {
	response := damessage.Assistant("used")
	response.Usage = &damessage.Usage{CostUSD: 2.5}
	response.OtherUsage = []damessage.PurposedUsage{{Purpose: "nested", Usage: damessage.Usage{CostUSD: 0.5}}}
	if got := resetBudget(10, []damessage.Message{response}); got != 13 {
		t.Fatalf("reset budget = %v", got)
	}
}

func TestDebugJSONAdditional(t *testing.T) {
	messages := []damessage.Message{damessage.Human("hello"), damessage.Assistant("world")}
	encoded, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var decoded []damessage.Message
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[0].Role != damessage.RoleHuman || decoded[1].TextContent() != "world" {
		t.Fatalf("decoded = %#v", decoded)
	}
}

package damessage

import "testing"

func TestAggregateUsageIncludesNestedCalls(t *testing.T) {
	direct := Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5, CostUSD: 0.2, InputDetails: map[string]int{"cached": 1}, Provider: "openai", Model: "gpt"}
	nested := PurposedUsage{Purpose: "summary", Usage: Usage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5, CostUSD: 0.3, InputDetails: map[string]int{"cached": 2}, Provider: "openai", Model: "gpt"}}
	messages := []Message{{Role: RoleAssistant, Usage: &direct, OtherUsage: []PurposedUsage{nested}}}

	got := AggregateUsage(messages)
	if got.InputTokens != 7 || got.OutputTokens != 3 || got.TotalTokens != 10 || got.CostUSD != 0.5 || got.InputDetails["cached"] != 3 {
		t.Fatalf("AggregateUsage() = %#v", got)
	}
	got.InputDetails["cached"] = 99
	if direct.InputDetails["cached"] != 1 || nested.InputDetails["cached"] != 2 {
		t.Fatal("AggregateUsage mutated source detail maps")
	}
}

func TestLastUsageAndToolUseCounts(t *testing.T) {
	first := Usage{TotalTokens: 2}
	last := Usage{TotalTokens: 7}
	messages := []Message{
		{Role: RoleAssistant, Usage: &first, ToolCalls: []ToolCall{{Name: "bash"}, {Name: "read_file"}}},
		{Role: RoleTool, ToolCallID: "one"},
		{Role: RoleAssistant, Usage: &last, ToolCalls: []ToolCall{{Name: "bash"}}},
	}
	got, ok := LastUsage(messages)
	if !ok || got.TotalTokens != 7 {
		t.Fatalf("LastUsage() = %#v, %v", got, ok)
	}
	counts := ToolUseCounts(messages)
	if counts["bash"] != 2 || counts["read_file"] != 1 {
		t.Fatalf("ToolUseCounts() = %#v", counts)
	}
}

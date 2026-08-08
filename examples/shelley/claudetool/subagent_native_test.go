package claudetool

import (
	"context"
	"encoding/json"
	"testing"

	dtool "github.com/semistrict/dago/tool"
)

func TestSubagentNativeToolUsesDagoContract(t *testing.T) {
	runner := &mockSubagentRunner{response: "native child result"}
	executable := (&SubagentTool{
		DB: newMockSubagentDB(), ParentConversationID: "parent-1",
		WorkingDir: NewMutableWorkingDir(t.TempDir()), Runner: runner,
		ModelID: "luna", ParentReasoning: "high",
	}).NativeTool()
	result, err := executable.Execute(context.Background(), json.RawMessage(`{
		"slug":"Native Child","prompt":"do the task"
	}`), dtool.Runtime{CallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "Subagent 'native-child' response:\nnative child result" {
		t.Fatalf("content = %#v", result.Content)
	}
	if runner.lastModelID != "luna" || runner.lastReasoning != "high" {
		t.Fatalf("runner model/reasoning = %q/%q", runner.lastModelID, runner.lastReasoning)
	}
	var display SubagentDisplayData
	if err := json.Unmarshal(result.Artifact, &display); err != nil {
		t.Fatal(err)
	}
	if display.Slug != "native-child" || display.ConversationID != "subagent-native-child" {
		t.Fatalf("display = %#v", display)
	}
}

package loop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/datool"

	"github.com/semistrict/dago/examples/shelley/llm"
)

func userStringMessage(text string) llm.Message {
	return llm.Message{Role: llm.MessageRoleUser, Content: llm.TextContent(text)}
}

func TestLoopUsesNativeChatWithoutLegacyRoundTrip(t *testing.T) {
	chat := &nativeRuntimeChat{}
	var recorded []llm.Message
	runtime := NewLoop(Config{
		Model: chat, ThinkingLevel: llm.ThinkingLevelHigh,
		RecordMessage: func(_ context.Context, item llm.Message, _ llm.Usage, _ []llm.PurposedUsage) error {
			recorded = append(recorded, item)
			return nil
		},
	})
	runtime.QueueUserMessage(userStringMessage("hello"))
	if err := runtime.ProcessOneTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if chat.calls.Load() != 1 {
		t.Fatalf("native model calls = %d, want 1", chat.calls.Load())
	}
	if len(recorded) != 1 || recorded[0].Content[0].Text != "native response" {
		t.Fatalf("recorded = %#v", recorded)
	}
}

func TestNativeImageProjectionUsesBase64AndPreservesDimensions(t *testing.T) {
	width, _ := json.Marshal(320)
	height, _ := json.Marshal(180)
	native := []dmessage.ContentBlock{{
		Type: dmessage.BlockImage, MIMEType: "image/png", Data: []byte{0, 1, 2, 255},
		Extra: map[string]json.RawMessage{displayWidthKey: width, displayHeightKey: height},
	}}

	projected := contentFromNative(native)
	if len(projected) != 1 {
		t.Fatalf("projected content = %#v", projected)
	}
	if projected[0].Data != base64.StdEncoding.EncodeToString(native[0].Data) {
		t.Fatalf("projected data = %q", projected[0].Data)
	}
	if projected[0].DisplayWidth != 320 || projected[0].DisplayHeight != 180 {
		t.Fatalf("projected dimensions = %dx%d", projected[0].DisplayWidth, projected[0].DisplayHeight)
	}

	roundTrip := contentToNative(projected)
	if len(roundTrip) != 1 || string(roundTrip[0].Data) != string(native[0].Data) {
		t.Fatalf("round-trip content = %#v", roundTrip)
	}
}

func TestNativeServerToolProjectionPreservesOpenAIOutputItem(t *testing.T) {
	raw := json.RawMessage(`{"type":"web_search_call","id":"search_1","status":"completed","action":{"type":"search","queries":["NYC weather"]}}`)
	native := []dmessage.ContentBlock{{
		Type: dmessage.BlockServerTool, ID: "search_1", Name: "web_search",
		Extra: map[string]json.RawMessage{
			"arguments":         json.RawMessage(`{"query":"NYC weather"}`),
			openAIOutputItemKey: raw,
		},
	}}

	projected := contentFromNative(native)
	if len(projected) != 1 || string(projected[0].OpenAIResponsesOutputItem) != string(raw) {
		t.Fatalf("projected content = %#v", projected)
	}
	roundTrip := contentToNative(projected)
	if len(roundTrip) != 1 || roundTrip[0].Type != dmessage.BlockServerTool || string(roundTrip[0].Extra[openAIOutputItemKey]) != string(raw) {
		t.Fatalf("round-trip content = %#v", roundTrip)
	}
}

func TestLoopNativeHarnessExposesCanonicalDeepAgentTools(t *testing.T) {
	chat := &harnessSurfaceChat{}
	runtime := NewLoop(Config{
		Model: chat, WorkingDir: t.TempDir(), FilesystemTools: []string{"ls", "read_file", "write_file", "edit_file", "delete", "glob", "grep", "execute"},
		RecordMessage: func(context.Context, llm.Message, llm.Usage, []llm.PurposedUsage) error { return nil },
	})
	runtime.QueueUserMessage(userStringMessage("inspect"))
	if err := runtime.ProcessOneTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLoopNativeHarnessAcceptsShelleyHostPaths(t *testing.T) {
	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "visible.txt")
	if err := os.WriteFile(filePath, []byte("visible"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: dmessage.Message{Role: dmessage.RoleAssistant, ToolCalls: []dmessage.ToolCall{{ID: "list", Name: "ls", Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, workingDir))}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Role != dmessage.RoleTool || !strings.Contains(last.TextContent(), filePath) {
				return fmt.Errorf("host-path ls result = %#v", last)
			}
			return nil
		}, Response: damodel.Response{Message: dmessage.Assistant("done")}},
	)
	runtime := NewLoop(Config{
		Model: script, WorkingDir: workingDir, FilesystemTools: []string{"ls"},
		RecordMessage: func(context.Context, llm.Message, llm.Usage, []llm.PurposedUsage) error { return nil },
	})
	runtime.QueueUserMessage(userStringMessage("list files"))
	if err := runtime.ProcessOneTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLoopNativeHarnessObservesWorkingDirectoryChangesWithinTurn(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	current := first
	type changeInput struct {
		Path string `json:"path"`
	}
	changeDir := datool.MustNew("change_dir", "change directory", func(_ context.Context, input changeInput) (string, error) {
		current = input.Path
		return "changed", nil
	})
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: dmessage.Message{Role: dmessage.RoleAssistant, ToolCalls: []dmessage.ToolCall{{ID: "change", Name: "change_dir", Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, second))}}}}},
		modeltest.Step{Response: damodel.Response{Message: dmessage.Message{Role: dmessage.RoleAssistant, ToolCalls: []dmessage.ToolCall{{ID: "execute", Name: "execute", Arguments: json.RawMessage(`{"command":"pwd"}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			resolvedSecond, _ := filepath.EvalSymlinks(second)
			if last.Role != dmessage.RoleTool || !strings.Contains(last.TextContent(), resolvedSecond) {
				return fmt.Errorf("execute after change_dir = %#v", last)
			}
			return nil
		}, Response: damodel.Response{Message: dmessage.Assistant("done")}},
	)
	runtime := NewLoop(Config{
		Model: script, WorkingDir: first, GetWorkingDir: func() string { return current },
		Tools: []datool.Tool{changeDir}, FilesystemTools: []string{"execute"},
		RecordMessage: func(context.Context, llm.Message, llm.Usage, []llm.PurposedUsage) error { return nil },
	})
	runtime.QueueUserMessage(userStringMessage("change directory and list files"))
	if err := runtime.ProcessOneTurn(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRuntimePreservesApplicationSystemPrompt(t *testing.T) {
	projected := []llm.SystemContent{{Text: " first "}, {Text: "second"}}
	if native := runtimeSystemPrompt(projected); native != "first \n\nsecond" {
		t.Fatalf("native system prompt = %q", native)
	}
}

func TestNativeRuntimeDelegatesDanglingToolRepairToNative(t *testing.T) {
	chat := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		for _, item := range request.Messages {
			if item.Role == dmessage.RoleTool && item.ToolCallID == "dangling" && strings.Contains(item.TextContent(), "was cancelled") {
				return nil
			}
		}
		return fmt.Errorf("dago did not patch dangling call: %#v", request.Messages)
	}, Response: damodel.Response{Message: dmessage.Assistant("continued")}})
	runtime := NewLoop(Config{
		Model: chat, WorkingDir: t.TempDir(),
		History: []llm.Message{
			{Role: llm.MessageRoleAssistant, Content: []llm.Content{{Type: llm.ContentTypeToolUse, ID: "dangling", ToolName: "lookup"}}},
			userStringMessage("continue"),
		},
		RecordMessage: func(context.Context, llm.Message, llm.Usage, []llm.PurposedUsage) error { return nil },
	})
	if err := runtime.ProcessOneTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type nativeRuntimeChat struct{ calls atomic.Int64 }

func (chat *nativeRuntimeChat) Profile() damodel.Profile {
	return damodel.Profile{Provider: "native-test", Model: "native-test", SupportsReasoning: true}
}

func (chat *nativeRuntimeChat) Invoke(_ context.Context, request damodel.Request) (damodel.Response, error) {
	chat.calls.Add(1)
	if request.Reasoning == nil || request.Reasoning.Effort != "high" || request.Reasoning.Summary != "auto" {
		return damodel.Response{}, fmt.Errorf("reasoning = %#v", request.Reasoning)
	}
	response := dmessage.Assistant("native response")
	response.ID = "native-response"
	response.Usage = &dmessage.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}
	return damodel.Response{Message: response}, nil
}

func (*nativeRuntimeChat) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return damodel.EmptyStream{}, nil
}

type harnessSurfaceChat struct{}

func (*harnessSurfaceChat) Profile() damodel.Profile {
	return damodel.Profile{Provider: "native-test", Model: "native-test", ToolCalling: true}
}

func (*harnessSurfaceChat) Invoke(_ context.Context, request damodel.Request) (damodel.Response, error) {
	names := make(map[string]bool, len(request.Tools))
	for _, definition := range request.Tools {
		names[definition.Name] = true
	}
	for _, required := range []string{"ls", "read_file", "write_file", "edit_file", "delete", "glob", "grep", "execute"} {
		if !names[required] {
			return damodel.Response{}, fmt.Errorf("missing dago harness tool %q", required)
		}
	}
	if names["task"] {
		return damodel.Response{}, fmt.Errorf("generic task tool overlaps Shelley's dago-owned conversation subagent")
	}
	return damodel.Response{Message: dmessage.Assistant("done")}, nil
}

func (*harnessSurfaceChat) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return damodel.EmptyStream{}, nil
}

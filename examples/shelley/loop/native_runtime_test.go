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

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"

	"shelley.exe.dev/llm"
)

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
	runtime.QueueUserMessage(llm.UserStringMessage("hello"))
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

	projected := contentFromDago(native)
	if len(projected) != 1 {
		t.Fatalf("projected content = %#v", projected)
	}
	if projected[0].Data != base64.StdEncoding.EncodeToString(native[0].Data) {
		t.Fatalf("projected data = %q", projected[0].Data)
	}
	if projected[0].DisplayWidth != 320 || projected[0].DisplayHeight != 180 {
		t.Fatalf("projected dimensions = %dx%d", projected[0].DisplayWidth, projected[0].DisplayHeight)
	}

	roundTrip := contentToDago(projected)
	if len(roundTrip) != 1 || string(roundTrip[0].Data) != string(native[0].Data) {
		t.Fatalf("round-trip content = %#v", roundTrip)
	}
}

func TestLoopDagoHarnessExposesCanonicalDeepAgentTools(t *testing.T) {
	chat := &harnessSurfaceChat{}
	runtime := NewLoop(Config{
		Model: chat, WorkingDir: t.TempDir(), FilesystemTools: []string{"ls", "read_file", "write_file", "edit_file", "delete", "glob", "grep", "execute"},
		RecordMessage: func(context.Context, llm.Message, llm.Usage, []llm.PurposedUsage) error { return nil },
	})
	runtime.QueueUserMessage(llm.UserStringMessage("inspect"))
	if err := runtime.ProcessOneTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLoopDagoHarnessAcceptsShelleyHostPaths(t *testing.T) {
	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "visible.txt")
	if err := os.WriteFile(filePath, []byte("visible"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := modeltest.New(dmodel.Profile{ToolCalling: true},
		modeltest.Step{Response: dmodel.Response{Message: dmessage.Message{Role: dmessage.RoleAssistant, ToolCalls: []dmessage.ToolCall{{ID: "list", Name: "ls", Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, workingDir))}}}}},
		modeltest.Step{Check: func(request dmodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Role != dmessage.RoleTool || !strings.Contains(last.TextContent(), filePath) {
				return fmt.Errorf("host-path ls result = %#v", last)
			}
			return nil
		}, Response: dmodel.Response{Message: dmessage.Assistant("done")}},
	)
	runtime := NewLoop(Config{
		Model: script, WorkingDir: workingDir, FilesystemTools: []string{"ls"},
		RecordMessage: func(context.Context, llm.Message, llm.Usage, []llm.PurposedUsage) error { return nil },
	})
	runtime.QueueUserMessage(llm.UserStringMessage("list files"))
	if err := runtime.ProcessOneTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRuntimeRemovesProjectedGuidance(t *testing.T) {
	projected := []llm.SystemContent{{Text: `base
<customization>projection note</customization>
<guidance><root_guidance file="AGENTS.md">project rules</root_guidance></guidance>
Subdirectory guidance files:
sub/AGENTS.md`}}
	native := runtimeSystemPrompt(projected)
	if strings.Contains(native, "projection note") || strings.Contains(native, "project rules") {
		t.Fatalf("projected guidance reached native runtime: %q", native)
	}
	if !strings.Contains(native, "base") || !strings.Contains(native, "sub/AGENTS.md") {
		t.Fatalf("application prompt content was removed: %q", native)
	}
}

func TestNativeRuntimeDelegatesDanglingToolRepairToDago(t *testing.T) {
	chat := modeltest.New(dmodel.Profile{}, modeltest.Step{Check: func(request dmodel.Request) error {
		for _, item := range request.Messages {
			if item.Role == dmessage.RoleTool && item.ToolCallID == "dangling" && strings.Contains(item.TextContent(), "was cancelled") {
				return nil
			}
		}
		return fmt.Errorf("Dago did not patch dangling call: %#v", request.Messages)
	}, Response: dmodel.Response{Message: dmessage.Assistant("continued")}})
	runtime := NewLoop(Config{
		Model: chat, WorkingDir: t.TempDir(),
		History: []llm.Message{
			{Role: llm.MessageRoleAssistant, Content: []llm.Content{{Type: llm.ContentTypeToolUse, ID: "dangling", ToolName: "lookup"}}},
			llm.UserStringMessage("continue"),
		},
		RecordMessage: func(context.Context, llm.Message, llm.Usage, []llm.PurposedUsage) error { return nil },
	})
	if err := runtime.ProcessOneTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type nativeRuntimeChat struct{ calls atomic.Int64 }

func (chat *nativeRuntimeChat) Profile() dmodel.Profile {
	return dmodel.Profile{Provider: "native-test", Model: "native-test", SupportsReasoning: true}
}

func (chat *nativeRuntimeChat) Invoke(_ context.Context, request dmodel.Request) (dmodel.Response, error) {
	chat.calls.Add(1)
	if request.Reasoning == nil || request.Reasoning.Effort != "high" || request.Reasoning.Summary != "auto" {
		return dmodel.Response{}, fmt.Errorf("reasoning = %#v", request.Reasoning)
	}
	response := dmessage.Assistant("native response")
	response.ID = "native-response"
	response.Usage = &dmessage.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}
	return dmodel.Response{Message: response}, nil
}

func (*nativeRuntimeChat) Stream(context.Context, dmodel.Request) (dmodel.Stream, error) {
	return dmodel.EmptyStream{}, nil
}

type harnessSurfaceChat struct{}

func (*harnessSurfaceChat) Profile() dmodel.Profile {
	return dmodel.Profile{Provider: "native-test", Model: "native-test", ToolCalling: true}
}

func (*harnessSurfaceChat) Invoke(_ context.Context, request dmodel.Request) (dmodel.Response, error) {
	names := make(map[string]bool, len(request.Tools))
	for _, definition := range request.Tools {
		names[definition.Name] = true
	}
	for _, required := range []string{"ls", "read_file", "write_file", "edit_file", "delete", "glob", "grep", "execute"} {
		if !names[required] {
			return dmodel.Response{}, fmt.Errorf("missing Dago harness tool %q", required)
		}
	}
	if names["task"] {
		return dmodel.Response{}, fmt.Errorf("generic task tool overlaps Shelley's Dago-owned conversation subagent")
	}
	return dmodel.Response{Message: dmessage.Assistant("done")}, nil
}

func (*harnessSurfaceChat) Stream(context.Context, dmodel.Request) (dmodel.Stream, error) {
	return dmodel.EmptyStream{}, nil
}

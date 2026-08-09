package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	dtool "github.com/semistrict/dago/tool"

	"shelley.exe.dev/llm"
)

func TestLoopPrefersNativeToolOverLegacyFacade(t *testing.T) {
	native := dtool.Func{
		Spec: dtool.Definition{Name: "lookup", Description: "lookup", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		Run: func(_ context.Context, _ json.RawMessage, runtime dtool.Runtime) (dtool.Result, error) {
			if runtime.CallID != "call-1" {
				return dtool.Result{}, fmt.Errorf("call id = %q", runtime.CallID)
			}
			return dtool.TextResult("native result"), nil
		},
	}
	chat := &nativeToolChat{}
	var recorded []llm.Message
	runtime := NewLoop(Config{
		Model: chat, Tools: []dtool.Tool{native},
		RecordMessage: func(_ context.Context, item llm.Message, _ llm.Usage, _ []llm.PurposedUsage) error {
			recorded = append(recorded, item)
			return nil
		},
	})
	runtime.QueueUserMessage(llm.UserStringMessage("lookup"))
	if err := runtime.ProcessOneTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if chat.calls.Load() != 2 {
		t.Fatalf("model calls = %d, want 2", chat.calls.Load())
	}
	if len(recorded) != 3 || recorded[1].Content[0].ToolResult[0].Text != "native result" ||
		recorded[1].Content[0].ToolUseStartTime == nil || recorded[1].Content[0].ToolUseEndTime == nil {
		t.Fatalf("recorded native tool projection = %#v", recorded)
	}
}

func TestResolveNativeToolsCanRequireCompleteNativeCoverage(t *testing.T) {
	resolved, err := validateTools(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved tools = %#v, want no implicit legacy fallback", resolved)
	}
}

type nativeToolChat struct{ calls atomic.Int64 }

func (*nativeToolChat) Profile() dmodel.Profile {
	return dmodel.Profile{Provider: "native-test", Model: "native-test", ToolCalling: true}
}

func (chat *nativeToolChat) Invoke(_ context.Context, request dmodel.Request) (dmodel.Response, error) {
	switch chat.calls.Add(1) {
	case 1:
		if len(request.Tools) != 1 || request.Tools[0].Name != "lookup" {
			return dmodel.Response{}, fmt.Errorf("tools = %#v", request.Tools)
		}
		return dmodel.Response{Message: dmessage.Message{
			Role:      dmessage.RoleAssistant,
			ToolCalls: []dmessage.ToolCall{{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
		}}, nil
	case 2:
		last := request.Messages[len(request.Messages)-1]
		if last.Role != dmessage.RoleTool || last.TextContent() != "native result" {
			return dmodel.Response{}, fmt.Errorf("tool result = %#v", last)
		}
		return dmodel.Response{Message: dmessage.Assistant("done")}, nil
	default:
		return dmodel.Response{}, fmt.Errorf("unexpected model call")
	}
}

func (*nativeToolChat) Stream(context.Context, dmodel.Request) (dmodel.Stream, error) {
	return dmodel.EmptyStream{}, nil
}

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
	var legacyCalls atomic.Int64
	legacy := &llm.Tool{
		Name: "lookup", Description: "lookup", InputSchema: llm.EmptySchema(),
		Run: func(context.Context, json.RawMessage) llm.ToolOut {
			legacyCalls.Add(1)
			return llm.ErrorfToolOut("legacy tool path called")
		},
	}
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
	service := &nativeToolService{chat: chat}
	var recorded []llm.Message
	runtime := NewLoop(Config{
		LLM: service, Tools: []*llm.Tool{legacy}, NativeTools: []dtool.Tool{native},
		RecordMessage: func(_ context.Context, item llm.Message, _ llm.Usage, _ []llm.PurposedUsage) error {
			recorded = append(recorded, item)
			return nil
		},
	})
	runtime.QueueUserMessage(llm.UserStringMessage("lookup"))
	if err := runtime.ProcessOneTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if legacyCalls.Load() != 0 {
		t.Fatalf("legacy tool calls = %d, want 0", legacyCalls.Load())
	}
	if chat.calls.Load() != 2 {
		t.Fatalf("model calls = %d, want 2", chat.calls.Load())
	}
	if len(recorded) != 3 || recorded[1].Content[0].ToolResult[0].Text != "native result" ||
		recorded[1].Content[0].ToolUseStartTime == nil || recorded[1].Content[0].ToolUseEndTime == nil {
		t.Fatalf("recorded native tool projection = %#v", recorded)
	}
}

type nativeToolService struct{ chat *nativeToolChat }

func (service *nativeToolService) DagoChat() dmodel.Chat { return service.chat }
func (*nativeToolService) Provider() string              { return "native-test" }
func (*nativeToolService) TokenContextWindow() int       { return 128000 }
func (*nativeToolService) MaxImageDimension() int        { return 0 }
func (*nativeToolService) MaxImageBytes() int            { return 0 }
func (*nativeToolService) SupportsImages() bool          { return false }
func (*nativeToolService) Do(context.Context, *llm.Request) (*llm.Response, error) {
	return nil, fmt.Errorf("legacy model path called")
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

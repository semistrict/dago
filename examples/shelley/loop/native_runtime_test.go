package loop

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"

	"shelley.exe.dev/llm"
)

func TestLoopUsesNativeChatWithoutLegacyRoundTrip(t *testing.T) {
	chat := &nativeRuntimeChat{}
	service := &nativeRuntimeService{chat: chat}
	var recorded []llm.Message
	runtime := NewLoop(Config{
		LLM: service, ThinkingLevel: llm.ThinkingLevelHigh,
		RecordMessage: func(_ context.Context, item llm.Message, _ llm.Usage, _ []llm.PurposedUsage) error {
			recorded = append(recorded, item)
			return nil
		},
	})
	runtime.QueueUserMessage(llm.UserStringMessage("hello"))
	if err := runtime.ProcessOneTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if service.legacyCalls.Load() != 0 {
		t.Fatalf("legacy Shelley model calls = %d, want 0", service.legacyCalls.Load())
	}
	if chat.calls.Load() != 1 {
		t.Fatalf("native model calls = %d, want 1", chat.calls.Load())
	}
	if len(recorded) != 1 || recorded[0].Content[0].Text != "native response" {
		t.Fatalf("recorded = %#v", recorded)
	}
}

type nativeRuntimeService struct {
	chat        *nativeRuntimeChat
	legacyCalls atomic.Int64
}

func (service *nativeRuntimeService) DagoChat() dmodel.Chat   { return service.chat }
func (service *nativeRuntimeService) ModelID() string         { return "native-test" }
func (service *nativeRuntimeService) Provider() string        { return "native-test" }
func (service *nativeRuntimeService) TokenContextWindow() int { return 128000 }
func (service *nativeRuntimeService) MaxImageDimension() int  { return 0 }
func (service *nativeRuntimeService) MaxImageBytes() int      { return 0 }
func (service *nativeRuntimeService) SupportsImages() bool    { return false }
func (service *nativeRuntimeService) Do(context.Context, *llm.Request) (*llm.Response, error) {
	service.legacyCalls.Add(1)
	return nil, fmt.Errorf("legacy model path called")
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

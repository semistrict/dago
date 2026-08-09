package loop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"

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

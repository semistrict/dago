package loop

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"

	"github.com/semistrict/dago/examples/shelley/llm"
)

// recordingService captures native model requests.
type recordingService struct {
	*customPredictableService
	mu   sync.Mutex
	reqs []damodel.Request
}

func (service *recordingService) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	service.mu.Lock()
	service.reqs = append(service.reqs, request)
	service.mu.Unlock()
	return service.customPredictableService.Invoke(ctx, request)
}
func (service *recordingService) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	return service.customPredictableService.Stream(ctx, request)
}

// TestInjectMessagesMidTurn verifies that messages returned by the
// InjectMessages callback are spliced into history between tool rounds, so
// the very next LLM request already carries them (e.g. a subagent completion
// notification the parent should react to immediately, mid-turn).
func TestInjectMessagesMidTurn(t *testing.T) {
	echoTool := datool.Func{
		Spec: datool.Definition{Name: "echo", Description: "echoes", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
			return datool.TextResult("echoed"), nil
		},
	}

	service := &recordingService{customPredictableService: &customPredictableService{
		responseFunc: func(request damodel.Request) (damodel.Response, error) {
			// Any request that already carries a tool result ends the turn;
			// the first request calls the tool.
			for _, item := range request.Messages {
				if item.Role == dmessage.RoleTool {
					return nativeTextResponse("done"), nil
				}
			}
			return nativeToolResponse("", "tool_1", "echo", json.RawMessage(`{}`)), nil
		},
	}}

	injectedPair := []llm.Message{
		{Role: llm.MessageRoleAssistant, Content: []llm.Content{{
			Type: llm.ContentTypeToolUse, ID: "sa_done_1", ToolName: "subagent",
			ToolInput: json.RawMessage(`{}`),
		}}},
		{Role: llm.MessageRoleUser, Content: []llm.Content{{
			Type: llm.ContentTypeToolResult, ToolUseID: "sa_done_1",
			ToolResult: []llm.Content{{Type: llm.ContentTypeText, Text: "subagent finished"}},
		}}},
	}

	var calls atomic.Int64
	loop := NewLoop(Config{
		Model: service,
		Tools: []datool.Tool{echoTool},
		RecordMessage: func(ctx context.Context, message llm.Message, usage llm.Usage, purposed []llm.PurposedUsage) error {
			return nil
		},
		InjectMessages: func(ctx context.Context) []llm.Message {
			// The first call happens before the first request; inject only on
			// the call after the tool round.
			if calls.Add(1) == 2 {
				return injectedPair
			}
			return nil
		},
	})

	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "go"}},
	})
	if err := loop.ProcessOneTurn(context.Background()); err != nil {
		t.Fatalf("ProcessOneTurn: %v", err)
	}

	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.reqs) != 2 {
		t.Fatalf("expected 2 LLM requests, got %d", len(service.reqs))
	}
	// Second request must carry: user, assistant(tool_use), user(tool_result),
	// assistant(injected tool_use), user(injected tool_result).
	msgs := service.reqs[1].Messages
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages in second request, got %d: %+v", len(msgs), msgs)
	}
	if len(msgs[3].ToolCalls) != 1 || msgs[3].ToolCalls[0].ID != "sa_done_1" {
		t.Errorf("expected injected tool_use at index 3, got %+v", msgs[3])
	}
	if msgs[4].ToolCallID != "sa_done_1" {
		t.Errorf("expected injected tool_result at index 4, got %+v", msgs[4])
	}
}

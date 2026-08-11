package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"

	"github.com/semistrict/dago/examples/shelley/llm"
)

func nativeToolResultCount(request damodel.Request) int {
	count := 0
	for _, item := range request.Messages {
		if item.Role == dmessage.RoleTool {
			count++
		}
	}
	return count
}

func nativeRequestHasText(request damodel.Request, text string) bool {
	for _, item := range request.Messages {
		if item.Role == dmessage.RoleHuman && item.TextContent() == text {
			return true
		}
	}
	return false
}

func nativeTextResponse(text string) damodel.Response {
	return damodel.Response{Message: dmessage.Assistant(text)}
}

func nativeToolResponse(text, id, name string, arguments json.RawMessage) damodel.Response {
	result := dmessage.Assistant(text)
	result.ToolCalls = []dmessage.ToolCall{{ID: id, Name: name, Arguments: arguments}}
	return damodel.Response{Message: result}
}

// TestInterruptionDuringToolExecution tests that user messages queued during
// tool execution are processed after the tool completes but before the next
// tool starts (not at the end of the entire turn).
func TestInterruptionDuringToolExecution(t *testing.T) {
	// Track when the tool is called and when it completes
	var toolStarted atomic.Bool
	var toolCompleted atomic.Bool
	var interruptionSeen atomic.Bool

	// Create a slow tool
	slowTool := datool.Func{
		Spec: datool.Definition{Name: "slow_tool", Description: "A tool that takes time to execute", InputSchema: json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}}}`)},
		Run: func(ctx context.Context, input json.RawMessage, _ datool.Runtime) (datool.Result, error) {
			toolStarted.Store(true)
			// Sleep to simulate slow tool execution
			time.Sleep(200 * time.Millisecond)
			toolCompleted.Store(true)
			return datool.TextResult("Tool completed"), nil
		},
	}

	recordMessage := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		return nil
	}

	// Create a service that detects the interruption
	service := &customPredictableService{
		responseFunc: func(request damodel.Request) (damodel.Response, error) {
			// Check if we've seen the interruption
			toolResults := nativeToolResultCount(request)
			if nativeRequestHasText(request, "INTERRUPTION") {
				interruptionSeen.Store(true)
				return nativeTextResponse("Acknowledged interruption"), nil
			}

			// First call: use the slow tool
			if toolResults == 0 {
				return nativeToolResponse("I'll use the slow tool", "tool_1", "slow_tool", json.RawMessage(`{"input":"test"}`)), nil
			}

			// After tool result, continue with more work
			return nativeTextResponse("Done with tool"), nil
		},
	}

	loop := NewLoop(Config{
		Model:         service,
		History:       []llm.Message{},
		Tools:         []datool.Tool{slowTool},
		RecordMessage: recordMessage,
	})

	// Queue initial user message that will trigger tool use
	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "use the tool"}},
	})

	// Run the loop in background
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var loopDone sync.WaitGroup
	loopDone.Go(func() {
		loop.Go(ctx)
	})

	// Wait for tool to start
	for !toolStarted.Load() {
		time.Sleep(10 * time.Millisecond)
	}

	// Queue an interruption message while tool is executing
	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "INTERRUPTION"}},
	})
	t.Log("Queued interruption message while tool is executing")

	// The message should remain in queue while tool is executing
	time.Sleep(50 * time.Millisecond)
	if !toolCompleted.Load() {
		loop.mu.Lock()
		queueLen := len(loop.messageQueue)
		loop.mu.Unlock()
		if queueLen > 0 {
			t.Log("Message is waiting in queue during tool execution (expected)")
		}
	}

	// Wait for loop to finish
	time.Sleep(500 * time.Millisecond)
	cancel()
	loopDone.Wait()

	// Verify the interruption was seen by the LLM
	if interruptionSeen.Load() {
		t.Log("SUCCESS: Interruption was seen by LLM after tool completed")
	} else {
		t.Error("Interruption was never seen by the LLM")
	}
}

// TestInterruptionDuringMultiToolChain tests interruption during a chain of tool calls.
// With the fix, the interruption should be visible to the LLM after the first tool completes.
func TestInterruptionDuringMultiToolChain(t *testing.T) {
	var toolCallCount atomic.Int32
	var interruptionSeenAtToolResult atomic.Int32 // -1 means not seen

	// Create a tool that's called multiple times
	multiTool := datool.Func{
		Spec: datool.Definition{Name: "multi_tool", Description: "A tool that might be called multiple times", InputSchema: json.RawMessage(`{"type":"object","properties":{"step":{"type":"integer"}}}`)},
		Run: func(ctx context.Context, input json.RawMessage, _ datool.Runtime) (datool.Result, error) {
			count := toolCallCount.Add(1)
			time.Sleep(100 * time.Millisecond) // Simulate some work
			_ = count
			return datool.TextResult("Tool step completed"), nil
		},
	}

	recordMessage := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		return nil
	}

	// Service that makes multiple tool calls but stops when it sees "STOP"
	interruptionSeenAtToolResult.Store(-1)
	service := &customPredictableService{
		responseFunc: func(request damodel.Request) (damodel.Response, error) {
			// Check if we've seen the STOP message
			toolResults := nativeToolResultCount(request)
			if nativeRequestHasText(request, "STOP") {
				interruptionSeenAtToolResult.CompareAndSwap(-1, int32(toolResults))
				return nativeTextResponse("Stopped due to user interruption"), nil
			}

			if toolResults < 5 {
				// Keep calling the tool (would do 5 if not interrupted)
				return nativeToolResponse("Calling tool again", fmt.Sprintf("tool_%d", toolResults+1), "multi_tool", json.RawMessage(fmt.Sprintf(`{"step":%d}`, toolResults+1))), nil
			}

			// Done with tools
			return nativeTextResponse("All tools completed"), nil
		},
	}

	loop := NewLoop(Config{
		Model:         service,
		History:       []llm.Message{},
		Tools:         []datool.Tool{multiTool},
		RecordMessage: recordMessage,
	})

	// Queue initial user message
	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "run the tool 5 times"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var loopDone sync.WaitGroup
	loopDone.Go(func() {
		loop.Go(ctx)
	})

	// Wait for first tool call to complete
	for toolCallCount.Load() < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	// Queue interruption after first tool
	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "STOP"}},
	})
	t.Logf("Queued STOP message after tool call %d", toolCallCount.Load())

	// Wait for loop to process and stop
	time.Sleep(500 * time.Millisecond)

	cancel()
	loopDone.Wait()

	finalToolCount := toolCallCount.Load()
	seenAt := interruptionSeenAtToolResult.Load()

	t.Logf("Final tool call count: %d (would be 5 without interruption)", finalToolCount)
	t.Logf("Interruption was seen by LLM after tool result %d", seenAt)

	// With the fix, the interruption should be seen after just 1 tool result
	// (the tool that was running when we queued the STOP message)
	if seenAt == 1 {
		t.Log("SUCCESS: Interruption was processed immediately after first tool completed")
	} else if seenAt > 1 {
		t.Errorf("Interruption was delayed: seen after %d tool results, expected 1", seenAt)
	} else if seenAt == -1 {
		t.Error("Interruption was never seen by the LLM")
	}

	// The tool should only be called a small number of times since we interrupted
	if finalToolCount > 2 {
		t.Errorf("Too many tool calls (%d): interruption should have stopped the chain earlier", finalToolCount)
	}
}

// customPredictableService allows custom response logic for testing
type customPredictableService struct {
	responses    []customResponse
	responseFunc func(request damodel.Request) (damodel.Response, error)
	callIndex    int
	mu           sync.Mutex
}

type customResponse struct {
	response damodel.Response
	err      error
}

func (s *customPredictableService) Invoke(_ context.Context, request damodel.Request) (damodel.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.responseFunc != nil {
		return s.responseFunc(request)
	}

	if s.callIndex >= len(s.responses) {
		// Default response
		return damodel.Response{Message: dmessage.Assistant("No more responses configured")}, nil
	}

	resp := s.responses[s.callIndex]
	s.callIndex++
	return resp.response, resp.err
}

func (*customPredictableService) Profile() damodel.Profile {
	return damodel.Profile{Provider: "builtin", Model: "custom-test", ContextWindow: 100000, ToolCalling: true, SupportsImages: true, MaxImageDimension: 8000}
}

func (*customPredictableService) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return nil, fmt.Errorf("custom test model does not stream")
}

// TestNoInterruptionNormalFlow verifies that normal tool chains work correctly
// when no interruption is queued.
func TestNoInterruptionNormalFlow(t *testing.T) {
	var toolCallCount atomic.Int32

	// Create a tool that tracks calls
	multiTool := datool.Func{
		Spec: datool.Definition{Name: "multi_tool", Description: "A tool", InputSchema: json.RawMessage(`{"type":"object","properties":{"step":{"type":"integer"}}}`)},
		Run: func(ctx context.Context, input json.RawMessage, _ datool.Runtime) (datool.Result, error) {
			toolCallCount.Add(1)
			return datool.TextResult("done"), nil
		},
	}

	recordMessage := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		return nil
	}

	// Service that makes 3 tool calls then finishes
	service := &customPredictableService{
		responseFunc: func(request damodel.Request) (damodel.Response, error) {
			toolResults := nativeToolResultCount(request)

			if toolResults < 3 {
				return nativeToolResponse("Calling tool", fmt.Sprintf("tool_%d", toolResults+1), "multi_tool", json.RawMessage(fmt.Sprintf(`{"step":%d}`, toolResults+1))), nil
			}

			return nativeTextResponse("All done"), nil
		},
	}

	loop := NewLoop(Config{
		Model:         service,
		History:       []llm.Message{},
		Tools:         []datool.Tool{multiTool},
		RecordMessage: recordMessage,
	})

	// Queue initial user message (no interruption)
	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "run tools"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var loopDone sync.WaitGroup
	loopDone.Go(func() {
		loop.Go(ctx)
	})

	// Wait for completion
	time.Sleep(500 * time.Millisecond)
	cancel()
	loopDone.Wait()

	finalCount := toolCallCount.Load()
	if finalCount != 3 {
		t.Errorf("Expected 3 tool calls, got %d", finalCount)
	} else {
		t.Log("SUCCESS: Normal flow completed 3 tool calls as expected")
	}
}

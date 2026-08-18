package loop_test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/semistrict/dago/datool"

	"github.com/semistrict/dago/examples/shelley/llm"
	"github.com/semistrict/dago/examples/shelley/loop"
)

func ExampleLoop() {
	type greetInput struct {
		Name string `json:"name"`
	}

	// Create a simple tool
	testTool := datool.Func{
		Spec: datool.Definition{Name: "greet", Description: "Greets the user with a friendly message", InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)},
		Run: func(_ context.Context, arguments json.RawMessage, _ datool.Runtime) (datool.Result, error) {
			var request greetInput
			if err := json.Unmarshal(arguments, &request); err != nil {
				return datool.Result{}, err
			}
			return datool.TextResult(fmt.Sprintf("Hello, %s! Nice to meet you.", request.Name)), nil
		},
	}

	// Message recording function (in real usage, this would save to database)
	recordMessage := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		roleStr := "user"
		if message.Role == llm.MessageRoleAssistant {
			roleStr = "assistant"
		}
		fmt.Printf("Recorded %s message with %d content items\n", roleStr, len(message.Content))
		return nil
	}

	// Create a loop with initial history
	initialHistory := []llm.Message{
		{
			Role: llm.MessageRoleUser,
			Content: []llm.Content{
				{Type: llm.ContentTypeText, Text: "Hello, I'm Alice"},
			},
		},
	}

	// Set up a predictable service for this example
	service := loop.NewPredictableService()
	myLoop := loop.NewLoop(service,

		recordMessage, loop.Config{
			History: initialHistory,
			Tools:   []datool.Tool{testTool},
		})

	// Queue a user message that triggers a simple response
	myLoop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hello"}},
	})

	// Run the loop for a short time
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	myLoop.Go(ctx)

	// Check usage
	usage := myLoop.GetUsage()
	fmt.Printf("Total usage: %s\n", usage.String())

	// Output:
	// Recorded assistant message with 1 content items
	// Total usage: in: 30, out: 3
}

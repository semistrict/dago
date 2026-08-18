package loop

import (
	"context"
	"testing"

	"github.com/semistrict/dago/examples/shelley/llm"
)

// TestLoopWithClaude preserves the upstream end-to-end loop contract while
// running it through the native deterministic model used by the dago port.
func TestLoopWithClaude(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conversation := NewLoop(NewPredictableService(),
		func(_ context.Context, message llm.Message, _ llm.Usage, _ []llm.PurposedUsage) error {
			if message.Role == llm.MessageRoleAssistant {
				cancel()
			}
			return nil
		}, Config{})
	conversation.QueueUserMessage(userStringMessage("Hello"))
	if err := conversation.Go(ctx); err != context.Canceled {
		t.Fatalf("Go error = %v, want context cancellation after assistant reply", err)
	}
	history := conversation.GetHistory()
	if len(history) < 2 || history[0].Role != llm.MessageRoleUser || history[1].Role != llm.MessageRoleAssistant {
		t.Fatalf("history = %#v, want user then assistant", history)
	}
}

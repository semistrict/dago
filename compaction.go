package dago

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
)

// ConversationCompactionOptions configures one explicit conversation
// compaction. Applications may customize prompts and history rendering while
// dago owns cut-point selection and the model invocation.
type ConversationCompactionOptions struct {
	KeepMessages int
	KeepTokens   int
	// ValidCutoffs optionally constrains the first recent-message index to
	// application record boundaries. Zero and len(messages) are implicit.
	ValidCutoffs  []int
	SystemPrompt  string
	Prompt        string
	Instructions  string
	Reasoning     *damodel.Reasoning
	FormatHistory func([]damessage.Message) (string, error)
}

// ConversationCompaction is the reusable result of one explicit compaction.
type ConversationCompaction struct {
	Summary  string
	Cutoff   int
	Older    []damessage.Message
	Recent   []damessage.Message
	Usage    *damessage.Usage
	Started  time.Time
	Finished time.Time
}

// CompactConversation summarizes the portion of messages before a safe cut
// point and returns the recent verbatim tail. It does not mutate checkpoints or
// application projections.
func CompactConversation(ctx context.Context, model damodel.Chat, messages []damessage.Message, options ConversationCompactionOptions) (ConversationCompaction, error) {
	if model == nil {
		panic("conversation compaction model is nil")
	}
	if options.KeepMessages <= 0 && options.KeepTokens <= 0 {
		options.KeepMessages = 6
	}
	cutoff := summaryCutoff(messages, Summarization{KeepMessages: options.KeepMessages, KeepTokens: options.KeepTokens})
	if len(options.ValidCutoffs) > 0 {
		cutoff = constrainedCompactionCutoff(messages, cutoff, options.ValidCutoffs)
	}
	result := ConversationCompaction{
		Cutoff: cutoff,
		Older:  cloneMessageSlice(messages[:cutoff]),
		Recent: cloneMessageSlice(messages[cutoff:]),
	}
	if cutoff == 0 {
		return result, nil
	}
	format := options.FormatHistory
	if format == nil {
		format = func(items []damessage.Message) (string, error) { return renderHistory(items), nil }
	}
	history, err := format(result.Older)
	if err != nil {
		return ConversationCompaction{}, fmt.Errorf("format conversation history: %w", err)
	}
	userPrompt := history
	if prompt := strings.TrimSpace(options.Prompt); prompt != "" {
		userPrompt += "\n\n" + prompt
	}
	if instructions := strings.TrimSpace(options.Instructions); instructions != "" {
		userPrompt += "\n\n" + instructions
	}
	systemPrompt := strings.TrimSpace(options.SystemPrompt)
	if systemPrompt == "" {
		systemPrompt = "Summarize the earlier conversation faithfully. Preserve decisions, constraints, unresolved tasks, file paths, errors, and important tool results."
	}
	reasoning := options.Reasoning
	if reasoning != nil {
		copy := *reasoning
		reasoning = &copy
	}
	result.Started = time.Now()
	response, err := model.Invoke(ctx, damodel.Request{
		Messages:  []damessage.Message{damessage.System(systemPrompt), damessage.Human(userPrompt)},
		Reasoning: reasoning,
	})
	result.Finished = time.Now()
	if err != nil {
		return ConversationCompaction{}, err
	}
	result.Summary = strings.TrimSpace(response.Message.TextContent())
	if result.Summary == "" {
		return ConversationCompaction{}, fmt.Errorf("conversation compaction returned an empty summary")
	}
	if response.Message.Usage != nil {
		usage := *response.Message.Usage
		result.Usage = &usage
	}
	return result, nil
}

func constrainedCompactionCutoff(messages []damessage.Message, desired int, candidates []int) int {
	best := len(messages)
	found := false
	for _, candidate := range append(append([]int(nil), candidates...), 0, len(messages)) {
		if candidate < desired || candidate < 0 || candidate > len(messages) {
			continue
		}
		if validCutoff(messages, candidate) != candidate {
			continue
		}
		if !found || candidate < best {
			best, found = candidate, true
		}
	}
	if found {
		return best
	}
	return 0
}

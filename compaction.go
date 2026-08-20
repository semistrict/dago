package dago

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
)

type conversationCompactionConfig struct {
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

// CompactOption configures one explicit conversation compaction.
type CompactOption interface {
	applyCompact(*conversationCompactionConfig)
}

type compactOptionFunc func(*conversationCompactionConfig)

func (option compactOptionFunc) applyCompact(config *conversationCompactionConfig) { option(config) }

func WithCompactKeepMessages(count int) CompactOption {
	return compactOptionFunc(func(config *conversationCompactionConfig) { config.KeepMessages = count })
}

func WithCompactKeepTokens(count int) CompactOption {
	return compactOptionFunc(func(config *conversationCompactionConfig) { config.KeepTokens = count })
}

func WithCompactCutoffs(cutoffs ...int) CompactOption {
	return compactOptionFunc(func(config *conversationCompactionConfig) { config.ValidCutoffs = append([]int(nil), cutoffs...) })
}

func WithCompactSystemPrompt(prompt string) CompactOption {
	return compactOptionFunc(func(config *conversationCompactionConfig) { config.SystemPrompt = prompt })
}

func WithCompactPrompt(prompt string) CompactOption {
	return compactOptionFunc(func(config *conversationCompactionConfig) { config.Prompt = prompt })
}

func WithCompactInstructions(instructions string) CompactOption {
	return compactOptionFunc(func(config *conversationCompactionConfig) { config.Instructions = instructions })
}

func WithCompactReasoning(reasoning damodel.Reasoning) CompactOption {
	return compactOptionFunc(func(config *conversationCompactionConfig) { config.Reasoning = &reasoning })
}

func WithCompactHistoryFormatter(format func([]damessage.Message) (string, error)) CompactOption {
	return compactOptionFunc(func(config *conversationCompactionConfig) { config.FormatHistory = format })
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
func CompactConversation(ctx context.Context, model damodel.Chat, messages []damessage.Message, options ...CompactOption) (ConversationCompaction, error) {
	if nilInterface(model) {
		panic("conversation compaction model is nil")
	}
	config := conversationCompactionConfig{}
	for index, option := range options {
		if option == nil {
			panic(fmt.Sprintf("conversation compaction option %d is nil", index))
		}
		option.applyCompact(&config)
	}
	if config.KeepMessages < 0 || config.KeepTokens < 0 {
		panic("conversation compaction limits cannot be negative")
	}
	if config.KeepMessages == 0 && config.KeepTokens == 0 {
		config.KeepMessages = 6
	}
	cutoff := summaryCutoff(messages, Summarization{KeepMessages: config.KeepMessages, KeepTokens: config.KeepTokens})
	if len(config.ValidCutoffs) > 0 {
		cutoff = constrainedCompactionCutoff(messages, cutoff, config.ValidCutoffs)
	}
	result := ConversationCompaction{
		Cutoff: cutoff,
		Older:  cloneMessageSlice(messages[:cutoff]),
		Recent: cloneMessageSlice(messages[cutoff:]),
	}
	if cutoff == 0 {
		return result, nil
	}
	format := config.FormatHistory
	if format == nil {
		format = func(items []damessage.Message) (string, error) { return renderHistory(items), nil }
	}
	history, err := format(result.Older)
	if err != nil {
		return ConversationCompaction{}, fmt.Errorf("format conversation history: %w", err)
	}
	userPrompt := history
	if prompt := strings.TrimSpace(config.Prompt); prompt != "" {
		userPrompt += "\n\n" + prompt
	}
	if instructions := strings.TrimSpace(config.Instructions); instructions != "" {
		userPrompt += "\n\n" + instructions
	}
	systemPrompt := strings.TrimSpace(config.SystemPrompt)
	if systemPrompt == "" {
		systemPrompt = "Summarize the earlier conversation faithfully. Preserve decisions, constraints, unresolved tasks, file paths, errors, and important tool results."
	}
	reasoning := config.Reasoning
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

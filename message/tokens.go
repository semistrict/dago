package message

import "unicode/utf8"

// ApproximateTokens provides a deterministic fallback when a model does not expose a
// tokenizer. It intentionally favors a stable estimate over provider-specific
// precision.
func ApproximateTokens(messages []Message) int {
	const messageOverhead = 3
	total := 0
	for _, message := range messages {
		total += messageOverhead
		total += approximateText(message.Name)
		total += approximateText(message.ToolCallID)
		for _, block := range message.Content {
			switch block.Type {
			case BlockText:
				total += approximateText(block.Text)
			case BlockReasoning:
				total += approximateText(block.Reasoning)
			case BlockImage, BlockFile, BlockAudio, BlockVideo:
				total += 85
			default:
				total += approximateText(string(block.NonStandard))
			}
		}
		for _, call := range message.ToolCalls {
			total += approximateText(call.Name)
			total += approximateText(string(call.Arguments))
		}
	}
	if len(messages) > 0 {
		total += 3
	}
	return total
}

func approximateText(value string) int {
	runes := utf8.RuneCountInString(value)
	if runes == 0 {
		return 0
	}
	return (runes + 3) / 4
}

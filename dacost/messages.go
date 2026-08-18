package dacost

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/semistrict/dago/damessage"
)

const maxMessageUsages = 65_536

// MessageOptions supplies configured-model fallbacks for top-level assistant
// messages. Nested usage must identify its own provider and model so a child
// request is never priced as though the parent served it.
type MessageOptions struct {
	FallbackProvider string
	FallbackModel    string
	MaxMessages      int
	MaxUsages        int
}

// ReportMessages reconstructs a deterministic report from durable normalized
// messages. It is useful after a process restart and deliberately does not
// trust message IDs for uniqueness: two persisted messages with the same
// provider ID still represent two requests.
func ReportMessages(messages []damessage.Message, estimator Estimator, options MessageOptions) (Report, error) {
	maxMessages := options.MaxMessages
	if maxMessages < 0 {
		panic("dacost: message limit cannot be negative")
	}
	if maxMessages == 0 {
		maxMessages = maxMessageUsages
	}
	if maxMessages > maxMessageUsages {
		panic("dacost: message limit exceeds hard safety maximum")
	}
	if len(messages) > maxMessages {
		return Report{}, fmt.Errorf("%w: report contains more than %d messages", ErrLimitExceeded, maxMessages)
	}
	maximum := options.MaxUsages
	if maximum < 0 {
		panic("dacost: message usage limit cannot be negative")
	}
	if maximum == 0 {
		maximum = maxMessageUsages
	}
	if maximum > maxMessageUsages {
		panic("dacost: message usage limit exceeds hard safety maximum")
	}
	tracker := NewTracker(estimator, Options{MaxRequests: maximum})
	count := 0
	for messageIndex, message := range messages {
		if message.Usage != nil {
			count++
			if count > maximum {
				return Report{}, fmt.Errorf("%w: messages contain more than %d usage records", ErrLimitExceeded, maximum)
			}
			usage := cloneUsage(*message.Usage)
			if _, err := tracker.Record(messageRequestID(messageIndex, 0), Observation{
				Usage: usage, FallbackProvider: options.FallbackProvider,
				FallbackModel: options.FallbackModel, Purpose: PurposeAssistant,
			}); err != nil {
				return Report{}, fmt.Errorf("record message %d usage: %w", messageIndex, err)
			}
		}
		for usageIndex, nested := range message.OtherUsage {
			count++
			if count > maximum {
				return Report{}, fmt.Errorf("%w: messages contain more than %d usage records", ErrLimitExceeded, maximum)
			}
			usage := cloneUsage(nested.Usage)
			if _, err := tracker.Record(messageRequestID(messageIndex, usageIndex+1), Observation{
				Usage: usage, Purpose: PurposeFromLabel(nested.Purpose),
			}); err != nil {
				return Report{}, fmt.Errorf("record message %d nested usage %d: %w", messageIndex, usageIndex, err)
			}
		}
	}
	return tracker.Report(), nil
}

// TransferUsage flattens the model usage owned by a completed nested run so
// its parent tool message can checkpoint it. Existing purpose labels are
// retained; direct assistant responses use purpose. A zero maximum selects the
// same bounded default as ReportMessages.
func TransferUsage(messages []damessage.Message, purpose Purpose, maximum int) ([]damessage.PurposedUsage, error) {
	if purpose == "" {
		purpose = PurposeSubagent
	}
	if !purpose.valid() {
		return nil, fmt.Errorf("%w: unsupported transfer purpose %q", ErrInvalidUsage, purpose)
	}
	if maximum < 0 {
		panic("dacost: transfer usage limit cannot be negative")
	}
	if maximum == 0 {
		maximum = maxMessageUsages
	}
	if maximum > maxMessageUsages {
		panic("dacost: transfer usage limit exceeds hard safety maximum")
	}
	if len(messages) > maxMessageUsages {
		return nil, fmt.Errorf("%w: nested run contains more than %d messages", ErrLimitExceeded, maxMessageUsages)
	}
	result := make([]damessage.PurposedUsage, 0)
	for messageIndex, message := range messages {
		if message.Usage != nil {
			if len(result) >= maximum {
				return nil, fmt.Errorf("%w: nested run contains more than %d usage records", ErrLimitExceeded, maximum)
			}
			usage := cloneUsage(*message.Usage)
			if err := validateTransferredUsage(usage); err != nil {
				return nil, fmt.Errorf("transfer message %d usage: %w", messageIndex, err)
			}
			result = append(result, damessage.PurposedUsage{Purpose: string(purpose), Usage: usage})
		}
		for usageIndex, nested := range message.OtherUsage {
			if len(result) >= maximum {
				return nil, fmt.Errorf("%w: nested run contains more than %d usage records", ErrLimitExceeded, maximum)
			}
			usage := cloneUsage(nested.Usage)
			if err := validateTransferredUsage(usage); err != nil {
				return nil, fmt.Errorf("transfer message %d nested usage %d: %w", messageIndex, usageIndex, err)
			}
			result = append(result, damessage.PurposedUsage{Purpose: string(PurposeFromLabel(nested.Purpose)), Usage: usage})
		}
	}
	return result, nil
}

// PurposeFromLabel normalizes durable and legacy source names into the four
// stable accounting buckets. Unknown side-model work is classified as offload
// rather than silently presented as an assistant response.
func PurposeFromLabel(label string) Purpose {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "assistant", "main":
		return PurposeAssistant
	case "subagent", "subagents", "child":
		return PurposeSubagent
	case "auto", "auto_mode", "auto_mode_classifier":
		return PurposeAuto
	case "offload", "summary", "summarization":
		return PurposeOffload
	default:
		return PurposeOffload
	}
}

func messageRequestID(messageIndex, usageIndex int) string {
	return "message:" + strconv.Itoa(messageIndex) + ":" + strconv.Itoa(usageIndex)
}

func validateTransferredUsage(usage damessage.Usage) error {
	return validateObservation("transfer", Observation{Usage: usage, Purpose: PurposeSubagent}, normalizeOptions(Options{}))
}

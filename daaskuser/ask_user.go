// Package daaskuser provides an opt-in tool for asking structured questions
// during an agent run. The tool pauses with an interrupt and completes after the
// caller resumes the run with an AnswerResponse.
package daaskuser

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

const (
	// ToolName is the model-visible name of the question tool.
	ToolName = "ask_user"
	// InterruptID identifies question interrupts to clients.
	InterruptID = "ask_user"
)

const defaultToolDescription = `Ask the user one or more questions when clarification or input is needed before proceeding.

Each question is either "text" for a free-form response or "multiple_choice" for predefined choices. A client should also allow a custom answer for multiple-choice questions. Questions are required by default; set required to false when the user may leave an answer blank. Group related questions into one call and use this tool sparingly.`

const defaultSystemPrompt = `## ask_user

You have access to the ask_user tool for information that cannot be determined from context. Use it sparingly. Ask concise, specific questions, prefer multiple choice when there are clear options, group related questions into one call, and do not ask questions you can answer yourself.`

const (
	// CancelledAnswer is recorded for every question when the interaction is cancelled.
	CancelledAnswer = "(cancelled)"
	// ErrorAnswerPrefix begins a model-visible answer produced for a failed interaction.
	ErrorAnswerPrefix = "(error: "
)

// QuestionType selects how a client should collect an answer.
type QuestionType string

const (
	QuestionText           QuestionType = "text"
	QuestionMultipleChoice QuestionType = "multiple_choice"
)

// Choice is one suggested answer to a multiple-choice question. Clients may
// additionally offer a free-form answer.
type Choice struct {
	Value string `json:"value" jsonschema:"minLength=1" description:"The display label for this choice."`
}

// Question is one structured prompt. Required defaults to true when omitted.
type Question struct {
	Question string       `json:"question" jsonschema:"minLength=1" description:"The question text to display."`
	Type     QuestionType `json:"type" jsonschema:"enum=text|multiple_choice" description:"Free-form text or a selection from predefined choices."`
	Choices  []Choice     `json:"choices,omitempty" description:"Options for a multiple-choice question. Clients should also offer a custom answer."`
	Required *bool        `json:"required,omitempty" description:"Whether the user must answer. Defaults to true when omitted."`
}

// IsRequired reports the effective required setting.
func (question Question) IsRequired() bool {
	return question.Required == nil || *question.Required
}

// Questions is the model-visible ask_user input.
type Questions struct {
	Questions []Question `json:"questions" jsonschema:"minItems=1" description:"Questions to present to the user."`
}

// AskRequest is the stable interrupt payload emitted by the tool.
type AskRequest struct {
	Type       string     `json:"type"`
	Questions  []Question `json:"questions"`
	ToolCallID string     `json:"tool_call_id"`
}

// AnswerStatus describes how an ask_user interaction ended.
type AnswerStatus string

const (
	AnswerAnswered  AnswerStatus = "answered"
	AnswerCancelled AnswerStatus = "cancelled"
	AnswerError     AnswerStatus = "error"
)

// AnswerResponse is supplied through dagent.Resume. Status defaults to
// AnswerAnswered when omitted. Answers remain positional, including an empty
// string for a skipped optional question.
type AnswerResponse struct {
	Status  AnswerStatus `json:"status,omitempty"`
	Answers []string     `json:"answers,omitempty"`
	Error   string       `json:"error,omitempty"`
}

// Options customizes the model-facing prompt and tool description. Empty fields
// select the package defaults.
type Options struct {
	SystemPrompt    string
	ToolDescription string
}

// Middleware adds the ask_user tool and its usage guidance. Callers must opt in
// by adding this middleware and provide a resume loop for InterruptID.
func Middleware() dagent.Middleware {
	return MiddlewareWithOptions(Options{})
}

// MiddlewareWithOptions adds a configured ask_user tool and usage guidance.
func MiddlewareWithOptions(options Options) dagent.Middleware {
	if options.SystemPrompt == "" {
		options.SystemPrompt = defaultSystemPrompt
	}
	if options.ToolDescription == "" {
		options.ToolDescription = defaultToolDescription
	}
	return dagent.Middleware{
		Name:           ToolName,
		SerializedName: "AskUserMiddleware",
		Tools:          []datool.Tool{NewTool(options.ToolDescription)},
		WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
			if options.SystemPrompt != "" {
				if request.SystemMessage == nil {
					message := damessage.System(options.SystemPrompt)
					request.SystemMessage = &message
				} else {
					message := request.SystemMessage.Clone()
					message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: "\n\n" + options.SystemPrompt})
					request.SystemMessage = &message
				}
			}
			return next(ctx, request)
		},
	}
}

// NewTool creates the interrupting ask_user tool. An empty description selects
// the package default.
func NewTool(description string) datool.Tool {
	if description == "" {
		description = defaultToolDescription
	}
	return datool.MustNew(ToolName, description, func(ctx context.Context, input Questions) (datool.Result, error) {
		if err := ValidateQuestions(input.Questions); err != nil {
			return datool.Result{}, err
		}
		runtime, ok := datool.RuntimeFromContext(ctx)
		if !ok {
			return datool.Result{}, errors.New("ask_user tool runtime is unavailable")
		}
		if runtime.Resume == nil {
			request := AskRequest{
				Type: ToolName, Questions: cloneQuestions(input.Questions), ToolCallID: runtime.CallID,
			}
			return datool.Result{Interrupt: &datool.Interrupt{ID: InterruptID, Value: request.checkpointValue()}}, nil
		}
		response, ok := datool.ResumeAs[AnswerResponse](runtime)
		if !ok {
			return failedResult(input.Questions, "invalid ask_user response payload"), nil
		}
		return resultForResponse(input.Questions, response), nil
	})
}

// checkpointValue uses only the portable scalar, list, and string-keyed-map
// subset accepted by durable checkpoint serializers. InterruptAs[AskRequest]
// reconstructs the exported wire type for clients.
func (request AskRequest) checkpointValue() map[string]any {
	questions := make([]any, len(request.Questions))
	for index, question := range request.Questions {
		record := map[string]any{
			"question": question.Question,
			"type":     string(question.Type),
		}
		if len(question.Choices) > 0 {
			choices := make([]any, len(question.Choices))
			for choiceIndex, choice := range question.Choices {
				choices[choiceIndex] = map[string]any{"value": choice.Value}
			}
			record["choices"] = choices
		}
		if question.Required != nil {
			record["required"] = *question.Required
		}
		questions[index] = record
	}
	return map[string]any{
		"type": request.Type, "questions": questions, "tool_call_id": request.ToolCallID,
	}
}

// ValidateQuestions checks relationships in the question schema that JSON
// Schema cannot express.
func ValidateQuestions(questions []Question) error {
	if len(questions) == 0 {
		return fmt.Errorf("%w: ask_user requires at least one question", datool.ErrInvalidArguments)
	}
	for index, question := range questions {
		if strings.TrimSpace(question.Question) == "" {
			return fmt.Errorf("%w: ask_user question %d requires non-empty text", datool.ErrInvalidArguments, index)
		}
		switch question.Type {
		case QuestionText:
			if len(question.Choices) > 0 {
				return fmt.Errorf("%w: text question %d must not define choices", datool.ErrInvalidArguments, index)
			}
		case QuestionMultipleChoice:
			if len(question.Choices) == 0 {
				return fmt.Errorf("%w: multiple-choice question %d requires choices", datool.ErrInvalidArguments, index)
			}
			for choiceIndex, choice := range question.Choices {
				if strings.TrimSpace(choice.Value) == "" {
					return fmt.Errorf("%w: multiple-choice question %d choice %d requires a value", datool.ErrInvalidArguments, index, choiceIndex)
				}
			}
		default:
			return fmt.Errorf("%w: ask_user question %d has unsupported type %q", datool.ErrInvalidArguments, index, question.Type)
		}
	}
	return nil
}

func resultForResponse(questions []Question, response AnswerResponse) datool.Result {
	status := response.Status
	if status == "" {
		status = AnswerAnswered
	}
	switch status {
	case AnswerCancelled:
		answers := make([]string, len(questions))
		for index := range answers {
			answers[index] = CancelledAnswer
		}
		return datool.TextResult(FormatTranscript(questions, answers))
	case AnswerError:
		detail := strings.TrimSpace(response.Error)
		if detail == "" {
			detail = "ask_user interaction failed"
		}
		return failedResult(questions, detail)
	case AnswerAnswered:
		if len(response.Answers) != len(questions) {
			return failedResult(questions, fmt.Sprintf("ask_user answer count mismatch (expected %d, got %d)", len(questions), len(response.Answers)))
		}
		for index, question := range questions {
			if question.IsRequired() && strings.TrimSpace(response.Answers[index]) == "" {
				return failedResult(questions, fmt.Sprintf("required answer %d is empty", index))
			}
		}
		return datool.TextResult(FormatTranscript(questions, response.Answers))
	default:
		return failedResult(questions, "invalid ask_user response status")
	}
}

func failedResult(questions []Question, detail string) datool.Result {
	answers := make([]string, len(questions))
	for index := range answers {
		answers[index] = ErrorAnswerPrefix + detail + ")"
	}
	result := datool.TextResult(FormatTranscript(questions, answers))
	result.Status = damessage.ToolStatusError
	return result
}

// FormatTranscript renders positional answers for the model as Q/A blocks.
func FormatTranscript(questions []Question, answers []string) string {
	blocks := make([]string, len(questions))
	for index, question := range questions {
		answer := "(no answer)"
		if index < len(answers) {
			answer = answers[index]
		}
		blocks[index] = "Q: " + question.Question + "\nA: " + answer
	}
	return strings.Join(blocks, "\n\n")
}

func cloneQuestions(questions []Question) []Question {
	cloned := make([]Question, len(questions))
	for index, question := range questions {
		cloned[index] = question
		cloned[index].Choices = append([]Choice(nil), question.Choices...)
		if question.Required != nil {
			cloned[index].Required = new(*question.Required)
		}
	}
	return cloned
}

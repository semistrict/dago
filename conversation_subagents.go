package dago

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

// ConversationSubagentRunner executes a turn in a persistent child conversation.
type ConversationSubagentRunner interface {
	RunSubagent(context.Context, ConversationSubagentRun) (ConversationSubagentReply, error)
}

// ConversationSubagentStore resolves a stable child conversation from its slug.
type ConversationSubagentStore interface {
	GetOrCreateSubagentConversation(context.Context, ConversationSubagentConversationRequest) (ConversationSubagentConversation, error)
}

// ConversationSubagentRun describes one turn in a persistent child conversation.
type ConversationSubagentRun struct {
	ConversationID string
	Prompt         string
	Wait           bool
	Timeout        time.Duration
	ModelID        string
	Reasoning      string
}

// ConversationSubagentReply is the visible response from a child conversation.
type ConversationSubagentReply struct {
	Content string
}

// ConversationSubagentConversationRequest identifies the child conversation to create or reuse.
type ConversationSubagentConversationRequest struct {
	Slug                 string
	ParentConversationID string
	WorkingDirectory     string
}

// ConversationSubagentConversation identifies a created or reused child conversation.
type ConversationSubagentConversation struct {
	ConversationID string
	Slug           string
}

// ConversationSubagentModel describes a model exposed to child conversations.
type ConversationSubagentModel struct {
	ID          string
	DisplayName string
}

// ConversationSubagentInput is the input accepted by ConversationSubagentTool.
type ConversationSubagentInput struct {
	Slug           string `json:"slug" description:"A short identifier for this subagent (e.g., 'research-api', 'test-runner')"`
	Prompt         string `json:"prompt" description:"The message to send to the subagent"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	Wait           *bool  `json:"wait,omitempty" description:"Whether to wait for completion. If false, returns immediately and completion is delivered asynchronously."`
	Model          string `json:"model,omitempty" description:"LLM model for the subagent. Defaults to the parent conversation's model."`
	Reasoning      string `json:"reasoning,omitempty" description:"Reasoning/thinking effort level for the subagent. If omitted, the subagent inherits the parent conversation's reasoning level."`
}

// ConversationSubagentDisplay identifies the persistent conversation created or reused by a call.
type ConversationSubagentDisplay struct {
	Slug           string `json:"slug"`
	ConversationID string `json:"conversation_id"`
}

// ConversationSubagentOptions configures a persistent conversation subagent tool.
type ConversationSubagentOptions struct {
	ParentConversationID string
	ModelID              string
	AvailableModels      []ConversationSubagentModel
	ParentReasoning      string
	ReasoningLevels      []string
	DefaultTimeout       time.Duration
	MaxTimeout           time.Duration
}

var defaultConversationSubagentReasoningLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh"}

// ConversationSubagentTool creates a tool that delegates work to named,
// persistent child conversations.
func ConversationSubagentTool(store ConversationSubagentStore, runner ConversationSubagentRunner, workingDirectory func() string, options ConversationSubagentOptions) datool.Tool {
	if store == nil || runner == nil || workingDirectory == nil {
		panic("conversation subagent store, runner, and working directory are required")
	}
	if len(options.ReasoningLevels) == 0 {
		options.ReasoningLevels = append([]string(nil), defaultConversationSubagentReasoningLevels...)
	}
	if options.DefaultTimeout <= 0 {
		options.DefaultTimeout = 15 * time.Minute
	}
	if options.MaxTimeout <= 0 {
		options.MaxTimeout = 60 * time.Minute
	}
	defaultSeconds := int(options.DefaultTimeout / time.Second)
	maxSeconds := int(options.MaxTimeout / time.Second)
	toolOptions := []datool.Option{
		datool.WithPropertyValue("timeout_seconds", "description", fmt.Sprintf("How long to wait for a synchronous response, in seconds (default: %d, max: %d). Only applies when wait=true; ignored otherwise. If the subagent hasn't finished by this deadline, the tool returns a progress summary and the subagent keeps running in the background; its eventual completion will then be delivered asynchronously.", defaultSeconds, maxSeconds)),
		datool.WithPropertyEnum("reasoning", options.ReasoningLevels...),
	}
	if len(options.AvailableModels) == 0 {
		toolOptions = append(toolOptions, datool.WithoutProperty("model"))
	} else {
		models := make([]string, 0, len(options.AvailableModels))
		for _, value := range options.AvailableModels {
			models = append(models, value.ID)
		}
		toolOptions = append(toolOptions, datool.WithPropertyEnum("model", models...))
	}
	return datool.MustNew(
		"subagent", conversationSubagentDescription(options),
		func(ctx context.Context, input ConversationSubagentInput) (datool.Result, error) {
			return executeConversationSubagent(ctx, store, runner, workingDirectory, options, input)
		},
		toolOptions...,
	)
}

func executeConversationSubagent(ctx context.Context, store ConversationSubagentStore, runner ConversationSubagentRunner, workingDirectory func() string, options ConversationSubagentOptions, input ConversationSubagentInput) (datool.Result, error) {
	if input.Slug == "" {
		return datool.Result{}, fmt.Errorf("slug is required")
	}
	input.Slug = SanitizeSubagentSlug(input.Slug)
	if input.Slug == "" {
		return datool.Result{}, fmt.Errorf("slug must contain alphanumeric characters")
	}
	if input.Prompt == "" {
		return datool.Result{}, fmt.Errorf("prompt is required")
	}
	timeout := options.DefaultTimeout
	if input.TimeoutSeconds > 0 {
		if int64(input.TimeoutSeconds) >= int64(options.MaxTimeout/time.Second) {
			timeout = options.MaxTimeout
		} else {
			timeout = time.Duration(input.TimeoutSeconds) * time.Second
		}
	}
	wait := true
	if input.Wait != nil {
		wait = *input.Wait
	}
	modelID := options.ModelID
	if input.Model != "" {
		if len(options.AvailableModels) > 0 && !conversationSubagentModelExists(options.AvailableModels, input.Model) {
			ids := make([]string, 0, len(options.AvailableModels))
			for _, value := range options.AvailableModels {
				ids = append(ids, value.ID)
			}
			return datool.Result{}, fmt.Errorf("unknown model %q; available: %s", input.Model, strings.Join(ids, ", "))
		}
		modelID = input.Model
	}
	reasoning := options.ParentReasoning
	if input.Reasoning != "" {
		if !stringIn(options.ReasoningLevels, input.Reasoning) {
			return datool.Result{}, fmt.Errorf("unknown reasoning level %q; available: %s", input.Reasoning, strings.Join(options.ReasoningLevels, ", "))
		}
		reasoning = input.Reasoning
	}
	conversation, err := store.GetOrCreateSubagentConversation(ctx, ConversationSubagentConversationRequest{
		Slug: input.Slug, ParentConversationID: options.ParentConversationID, WorkingDirectory: workingDirectory(),
	})
	if err != nil {
		return datool.Result{}, fmt.Errorf("failed to get/create subagent conversation: %w", err)
	}
	reply, err := runner.RunSubagent(ctx, ConversationSubagentRun{
		ConversationID: conversation.ConversationID, Prompt: input.Prompt, Wait: wait, Timeout: timeout, ModelID: modelID, Reasoning: reasoning,
	})
	if err != nil {
		return datool.Result{}, fmt.Errorf("subagent error: %w", err)
	}
	slugNote := ""
	if conversation.Slug != input.Slug {
		slugNote = fmt.Sprintf(" (Note: slug was changed to '%s' for uniqueness. Use '%s' for future messages to this subagent.)", conversation.Slug, conversation.Slug)
	}
	display, err := json.Marshal(ConversationSubagentDisplay{Slug: conversation.Slug, ConversationID: conversation.ConversationID})
	if err != nil {
		return datool.Result{}, fmt.Errorf("encode subagent display: %w", err)
	}
	return datool.Result{
		Content:  []damessage.ContentBlock{{Type: damessage.BlockText, Text: fmt.Sprintf("Subagent '%s' response:%s\n%s", conversation.Slug, slugNote, reply.Content)}},
		Artifact: display,
	}, nil
}

func conversationSubagentModelExists(values []ConversationSubagentModel, id string) bool {
	for _, value := range values {
		if value.ID == id {
			return true
		}
	}
	return false
}

func stringIn(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func conversationSubagentDescription(options ConversationSubagentOptions) string {
	description := `Spawn or interact with a subagent conversation.

Subagents are independent conversations that can work on subtasks in parallel.
Use subagents for:
- Long-running tasks that you want to delegate
- Token-intensive tasks that produce lots of output, little of which is needed
- Parallel exploration of different approaches
- Breaking down complex problems into independent pieces

Each subagent has its own slug identifier within this conversation.
You can send messages to existing subagents by using the same slug.
The tool returns the subagent's last response, or a status if the timeout is reached.

When writing prompts for subagents, convey intent, nuance, and operational
details — not just prescriptive instructions. The subagent has no context
beyond what you put in the prompt, so share the "why" alongside the "what".

Use the "reasoning" parameter to set the subagent's thinking effort (off,
minimal, low, medium, high, xhigh). If omitted, the subagent inherits the
parent conversation's reasoning level.`
	if len(options.AvailableModels) > 0 {
		description += "\n\nAvailable models (use the \"model\" parameter to override the default):"
		for _, value := range options.AvailableModels {
			if value.DisplayName != "" && value.DisplayName != value.ID {
				description += fmt.Sprintf("\n- %s (%s)", value.ID, value.DisplayName)
			} else {
				description += "\n- " + value.ID
			}
		}
	}
	return description
}

// SanitizeSubagentSlug converts a user label to a lowercase ASCII slug.
func SanitizeSubagentSlug(slug string) string {
	var result strings.Builder
	for _, r := range strings.ToLower(slug) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			result.WriteRune(r)
		} else if r == ' ' || r == '_' {
			result.WriteRune('-')
		}
	}
	value := result.String()
	for strings.Contains(value, "--") {
		value = strings.ReplaceAll(value, "--", "-")
	}
	return strings.Trim(value, "-")
}

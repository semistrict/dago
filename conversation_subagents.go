package dago

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/tool"
)

// ConversationSubagentRunner executes a turn in a persistent child conversation.
type ConversationSubagentRunner interface {
	RunSubagent(context.Context, string, string, bool, time.Duration, string, string) (string, error)
}

// ConversationSubagentStore resolves a stable child conversation from its slug.
type ConversationSubagentStore interface {
	GetOrCreateSubagentConversation(context.Context, string, string, string) (string, string, error)
}

// ConversationSubagentModel describes a model exposed to child conversations.
type ConversationSubagentModel struct {
	ID          string
	DisplayName string
}

// ConversationSubagentInput is the input accepted by ConversationSubagentTool.
type ConversationSubagentInput struct {
	Slug           string `json:"slug"`
	Prompt         string `json:"prompt"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	Wait           *bool  `json:"wait,omitempty"`
	Model          string `json:"model,omitempty"`
	Reasoning      string `json:"reasoning,omitempty"`
}

// ConversationSubagentDisplay identifies the persistent conversation created or reused by a call.
type ConversationSubagentDisplay struct {
	Slug           string `json:"slug"`
	ConversationID string `json:"conversation_id"`
}

// ConversationSubagentOptions configures a persistent conversation subagent tool.
type ConversationSubagentOptions struct {
	Store                ConversationSubagentStore
	Runner               ConversationSubagentRunner
	ParentConversationID string
	WorkingDirectory     func() string
	ModelID              string
	AvailableModels      []ConversationSubagentModel
	ParentReasoning      string
	ReasoningLevels      []string
	DefaultTimeout       time.Duration
	MaxTimeout           time.Duration
}

var defaultConversationSubagentReasoningLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh"}

// ConversationSubagentTool creates a tool that delegates work to named, persistent child conversations.
func ConversationSubagentTool(options ConversationSubagentOptions) tool.Tool {
	if len(options.ReasoningLevels) == 0 {
		options.ReasoningLevels = append([]string(nil), defaultConversationSubagentReasoningLevels...)
	}
	if options.DefaultTimeout <= 0 {
		options.DefaultTimeout = 15 * time.Minute
	}
	if options.MaxTimeout <= 0 {
		options.MaxTimeout = 60 * time.Minute
	}
	return tool.Func{
		Spec: tool.Definition{Name: "subagent", Description: conversationSubagentDescription(options), InputSchema: conversationSubagentSchema(options)},
		Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
			var input ConversationSubagentInput
			if err := json.Unmarshal(raw, &input); err != nil {
				return tool.Result{}, fmt.Errorf("%w: %v", tool.ErrInvalidArguments, err)
			}
			return executeConversationSubagent(ctx, options, input)
		},
	}
}

func executeConversationSubagent(ctx context.Context, options ConversationSubagentOptions, input ConversationSubagentInput) (tool.Result, error) {
	if input.Slug == "" {
		return tool.Result{}, fmt.Errorf("slug is required")
	}
	input.Slug = SanitizeSubagentSlug(input.Slug)
	if input.Slug == "" {
		return tool.Result{}, fmt.Errorf("slug must contain alphanumeric characters")
	}
	if input.Prompt == "" {
		return tool.Result{}, fmt.Errorf("prompt is required")
	}
	if options.Store == nil || options.Runner == nil || options.WorkingDirectory == nil {
		return tool.Result{}, fmt.Errorf("conversation subagent store, runner, and working directory are required")
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
			return tool.Result{}, fmt.Errorf("unknown model %q; available: %s", input.Model, strings.Join(ids, ", "))
		}
		modelID = input.Model
	}
	reasoning := options.ParentReasoning
	if input.Reasoning != "" {
		if !stringIn(options.ReasoningLevels, input.Reasoning) {
			return tool.Result{}, fmt.Errorf("unknown reasoning level %q; available: %s", input.Reasoning, strings.Join(options.ReasoningLevels, ", "))
		}
		reasoning = input.Reasoning
	}
	conversationID, actualSlug, err := options.Store.GetOrCreateSubagentConversation(ctx, input.Slug, options.ParentConversationID, options.WorkingDirectory())
	if err != nil {
		return tool.Result{}, fmt.Errorf("failed to get/create subagent conversation: %w", err)
	}
	response, err := options.Runner.RunSubagent(ctx, conversationID, input.Prompt, wait, timeout, modelID, reasoning)
	if err != nil {
		return tool.Result{}, fmt.Errorf("subagent error: %w", err)
	}
	slugNote := ""
	if actualSlug != input.Slug {
		slugNote = fmt.Sprintf(" (Note: slug was changed to '%s' for uniqueness. Use '%s' for future messages to this subagent.)", actualSlug, actualSlug)
	}
	display, err := json.Marshal(ConversationSubagentDisplay{Slug: actualSlug, ConversationID: conversationID})
	if err != nil {
		return tool.Result{}, fmt.Errorf("encode subagent display: %w", err)
	}
	return tool.Result{
		Content:  []message.ContentBlock{{Type: message.BlockText, Text: fmt.Sprintf("Subagent '%s' response:%s\n%s", actualSlug, slugNote, response)}},
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

func conversationSubagentSchema(options ConversationSubagentOptions) json.RawMessage {
	defaultSeconds := int(options.DefaultTimeout / time.Second)
	maxSeconds := int(options.MaxTimeout / time.Second)
	properties := map[string]any{
		"slug":            map[string]any{"type": "string", "description": "A short identifier for this subagent (e.g., 'research-api', 'test-runner')"},
		"prompt":          map[string]any{"type": "string", "description": "The message to send to the subagent"},
		"timeout_seconds": map[string]any{"type": "integer", "description": fmt.Sprintf("How long to wait for a synchronous response, in seconds (default: %d, max: %d). Only applies when wait=true; ignored otherwise. If the subagent hasn't finished by this deadline, the tool returns a progress summary and the subagent keeps running in the background; its eventual completion will then be delivered asynchronously.", defaultSeconds, maxSeconds)},
		"wait":            map[string]any{"type": "boolean", "description": "Whether to wait for completion (default: true). If false, returns immediately; when the subagent eventually finishes, its response is delivered asynchronously. If wait=true and the subagent completes before timeout, no later asynchronous duplicate is delivered. Sending a new message to a subagent that is still working does NOT interrupt it: the message is queued and delivered after the current turn finishes."},
		"reasoning":       map[string]any{"type": "string", "description": "Reasoning/thinking effort level for the subagent. If omitted, the subagent inherits the parent conversation's reasoning level.", "enum": options.ReasoningLevels},
	}
	if len(options.AvailableModels) > 0 {
		models := make([]string, 0, len(options.AvailableModels))
		for _, value := range options.AvailableModels {
			models = append(models, value.ID)
		}
		properties["model"] = map[string]any{"type": "string", "description": "LLM model for the subagent. Defaults to the parent conversation's model.", "enum": models}
	}
	schema, _ := json.Marshal(map[string]any{"type": "object", "required": []string{"slug", "prompt"}, "properties": properties})
	return schema
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

package slug

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"

	"shelley.exe.dev/db"
	"shelley.exe.dev/db/generated"
	"shelley.exe.dev/llm"
	"shelley.exe.dev/models"
)

// LLMServiceProvider supplies native chat models and their catalog metadata.
type LLMServiceProvider interface {
	GetChat(modelID string) (dmodel.Chat, error)
	GetAvailableModels() []string
	GetModelInfo(modelID string) *models.ModelInfo
}

// GenerateSlug generates a slug for a conversation and updates the database.
// If the conversation already has a slug, it is returned unchanged (no LLM call).
// If conversationModelID is provided, it will be used as a fallback if no model is tagged with "slug".
//
// Also returns the appended slug marker message (nil when no LLM call was made
// or no usage was reported). The caller MUST publish it: it carries a real
// sequence_id, so a client that never receives it sees a hole in the sequence
// and correctly discards its cached history.
func GenerateSlug(ctx context.Context, llmProvider LLMServiceProvider, database *db.DB, logger *slog.Logger, conversationID, userMessage, conversationModelID string) (string, *generated.Message, error) {
	// Don't regenerate if the conversation already has a slug. This matters
	// for flows that look like "first message" but are actually continuations,
	// e.g. starting a new generation after compaction.
	if conv, err := database.GetConversationByID(ctx, conversationID); err == nil && conv != nil && conv.Slug != nil && *conv.Slug != "" {
		return *conv.Slug, nil, nil
	}

	baseSlug, usage, err := generateSlugText(ctx, llmProvider, logger, userMessage, conversationModelID)
	if err != nil {
		return "", nil, err
	}

	// Record the slug call's cost by APPENDING a marker message rather than
	// editing an existing row. Message rows are an append-only log: the browser
	// caches them keyed by (conversation_id, sequence_id) and only ever fetches
	// the tail, forks copy them, and a sequence_id is streamed exactly once. The
	// marker renders as nothing and is never sent to the LLM; it exists purely so
	// the usage is accounted for through the ordinary message-usage path.
	var marker *generated.Message
	if usage != nil {
		marker, err = database.CreateSlugMessage(ctx, conversationID, []llm.PurposedUsage{{
			Purpose: "slug",
			Usage: llm.Usage{
				InputTokens:              uint64(max(0, usage.tokens.InputTokens)),
				CacheCreationInputTokens: uint64(max(0, usage.tokens.InputDetails["cache_creation"])),
				CacheReadInputTokens:     uint64(max(0, usage.tokens.InputDetails["cache_read"])),
				OutputTokens:             uint64(max(0, usage.tokens.OutputTokens)),
				Model:                    usage.model,
				StartTime:                &usage.started,
				EndTime:                  &usage.finished,
			},
		}})
		if err != nil {
			logger.Error("Failed to record slug usage", "conversationID", conversationID, "error", err)
		}
	}

	// Try to update with the base slug first, then with numeric suffixes if needed
	slug := baseSlug
	for attempt := 0; attempt < 100; attempt++ {
		_, err = database.UpdateConversationSlug(ctx, conversationID, slug)
		if err == nil {
			// Success!
			logger.Info("Generated slug for conversation", "conversationID", conversationID, "slug", slug)
			return slug, marker, nil
		}

		// Check if this is a unique constraint violation
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") ||
			strings.Contains(strings.ToLower(err.Error()), "unique constraint") ||
			strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			// Try with a numeric suffix
			slug = fmt.Sprintf("%s-%d", baseSlug, attempt+1)
			continue
		}

		// Some other error occurred. The marker still comes back so the caller
		// publishes it: the row exists and owns a sequence_id either way.
		return "", marker, fmt.Errorf("failed to update conversation slug: %w", err)
	}

	// If we've tried 100 times and still failed, give up
	return "", marker, fmt.Errorf("failed to generate unique slug after 100 attempts")
}

// preferredModelSubstrings is an ordered priority list of model-ID substrings
// used to pick a slug model when no tagged model produced a slug. Models
// discovered from a gateway integration carry no catalog tags, so without
// this list slug generation would fall through to the conversation's model.
// Cheap, fast models first.
var preferredModelSubstrings = []string{
	"gpt-oss-20b",
	"gpt-5.6-luna",
	"haiku",
	"gemini-3.6-flash",
	"gemini-3-flash",
	"-nano",
	"-mini",
}

// preferredModels returns available model IDs matching the substring priority
// list, in priority order, deduplicated and excluding already-tried models.
func preferredModels(available []string, tried map[string]bool) []string {
	var out []string
	seen := make(map[string]bool, len(available))
	for _, sub := range preferredModelSubstrings {
		for _, id := range available {
			if !seen[id] && !tried[id] && strings.Contains(id, sub) {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// generateSlugText generates a human-readable slug for a conversation based on the user message
// Priority order:
// 1. If conversationModelID is "predictable", use it
// 2. Try models tagged with "slug" (try the LLM call; if it fails, continue)
// 3. Try models tagged with "slug-backup"
// 4. Try models matching preferredModelSubstrings (covers untagged gateway models)
// 5. Fall back to the conversation's model (conversationModelID)
func generateSlugText(ctx context.Context, llmProvider LLMServiceProvider, logger *slog.Logger, userMessage, conversationModelID string) (string, *slugUsage, error) {
	// If conversation is using predictable model, use it for slug generation too
	if conversationModelID == "predictable" {
		chat, err := llmProvider.GetChat("predictable")
		if err == nil {
			logger.Debug("Using predictable model for slug generation")
			return callSlugLLM(ctx, chat, userMessage)
		}
		logger.Debug("Predictable model not available for slug generation", "error", err)
	}

	// Try models tagged with "slug", then "slug-backup"
	tried := map[string]bool{}
	for _, tag := range []string{"slug", "slug-backup"} {
		for _, modelID := range llmProvider.GetAvailableModels() {
			info := llmProvider.GetModelInfo(modelID)
			if info == nil || !hasTag(info.Tags, tag) {
				continue
			}
			if tried[modelID] {
				continue
			}
			tried[modelID] = true
			chat, err := llmProvider.GetChat(modelID)
			if err != nil {
				logger.Debug("Failed to get model for slug generation", "model", modelID, "tag", tag, "error", err)
				continue
			}
			logger.Debug("Trying model for slug generation", "model", modelID, "tag", tag)
			slug, usage, err := callSlugLLM(ctx, chat, userMessage)
			if err == nil {
				return slug, usage, nil
			}
			logger.Warn("Slug generation failed, trying next model", "model", modelID, "tag", tag, "error", err)
		}
	}

	// No tagged model produced a slug (typical for gateway integrations, which
	// don't carry tags at all). Try the substring preference list, skipping
	// models already tried above.
	for _, modelID := range preferredModels(llmProvider.GetAvailableModels(), tried) {
		if ctx.Err() != nil {
			break
		}
		tried[modelID] = true
		chat, err := llmProvider.GetChat(modelID)
		if err != nil {
			logger.Debug("Failed to get preferred model for slug generation", "model", modelID, "error", err)
			continue
		}
		logger.Debug("Trying preferred model for slug generation", "model", modelID)
		slug, usage, err := callSlugLLM(ctx, chat, userMessage)
		if err == nil {
			return slug, usage, nil
		}
		logger.Warn("Slug generation failed, trying next model", "model", modelID, "error", err)
	}

	// Fall back to the conversation's model
	if conversationModelID != "" && conversationModelID != "predictable" && !tried[conversationModelID] {
		chat, err := llmProvider.GetChat(conversationModelID)
		if err == nil {
			logger.Debug("Using conversation model for slug generation", "model", conversationModelID)
			return callSlugLLM(ctx, chat, userMessage)
		}
		logger.Debug("Conversation model not available for slug generation", "model", conversationModelID, "error", err)
	}

	return "", nil, fmt.Errorf("no suitable model available for slug generation")
}

// hasTag checks if a comma-separated tag list contains the exact given tag.
func hasTag(tags, tag string) bool {
	for _, t := range strings.Split(tags, ",") {
		if strings.TrimSpace(t) == tag {
			return true
		}
	}
	return false
}

// PromptPreamble is the fixed leading text of the slug-generation prompt. It is
// exported so tests (e.g. fake LLM services shared with the agent loop) can
// reliably distinguish a slug request from a real agent turn. Keep it in sync
// with the format string in callSlugLLM.
const PromptPreamble = "Generate a short, descriptive slug (2-6 words, lowercase, hyphen-separated) for a conversation that starts with this user message:"

// callSlugLLM calls a native chat model to generate a slug from a user message.
type slugUsage struct {
	tokens   dmessage.Usage
	model    string
	started  time.Time
	finished time.Time
}

func callSlugLLM(ctx context.Context, chat dmodel.Chat, userMessage string) (string, *slugUsage, error) {
	slugPrompt := fmt.Sprintf(PromptPreamble+`

%s

The slug should:
- Be concise and descriptive
- Use only lowercase letters, numbers, and hyphens
- Capture the main topic or intent
- Be suitable as a filename or URL path

Respond with only the slug, nothing else.`, userMessage)

	ctxWithTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	started := time.Now()
	response, err := chat.Invoke(ctxWithTimeout, dmodel.Request{Messages: []dmessage.Message{dmessage.Human(slugPrompt)}})
	finished := time.Now()
	if err != nil {
		return "", nil, fmt.Errorf("failed to generate slug: %w", err)
	}

	if len(response.Message.Content) == 0 {
		return "", nil, fmt.Errorf("empty response from LLM")
	}

	// Find the first text content block. Reasoning models (e.g.
	// gpt-oss-20b) return a leading Thinking block with the actual answer
	// in a later text block, so we can't just read Content[0].
	var text string
	for _, content := range response.Message.Content {
		if content.Type == dmessage.BlockText && strings.TrimSpace(content.Text) != "" {
			text = content.Text
			break
		}
	}

	slug := strings.TrimSpace(text)
	slug = Sanitize(slug)
	if slug == "" {
		return "", nil, fmt.Errorf("generated slug is empty after sanitization")
	}

	var usage *slugUsage
	if response.Message.Usage != nil {
		usage = &slugUsage{tokens: *response.Message.Usage, model: chat.Profile().Model, started: started, finished: finished}
	}
	return slug, usage, nil
}

// Sanitize cleans a string to be a valid slug
func Sanitize(input string) string {
	// Convert to lowercase
	slug := strings.ToLower(input)

	// Replace spaces and underscores with hyphens
	slug = regexp.MustCompile(`[\s_]+`).ReplaceAllString(slug, "-")

	// Remove non-alphanumeric characters except hyphens
	slug = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(slug, "")

	// Remove multiple consecutive hyphens
	slug = regexp.MustCompile(`-+`).ReplaceAllString(slug, "-")

	// Remove leading/trailing hyphens
	slug = strings.Trim(slug, "-")

	// Limit length
	if len(slug) > 60 {
		slug = slug[:60]
		slug = strings.Trim(slug, "-")
	}

	return slug
}

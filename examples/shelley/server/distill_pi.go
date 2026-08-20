package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	dago "github.com/semistrict/dago"
	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"

	"github.com/semistrict/dago/examples/shelley/db"
	"github.com/semistrict/dago/examples/shelley/db/generated"
	"github.com/semistrict/dago/examples/shelley/llm"
	loopapp "github.com/semistrict/dago/examples/shelley/loop"
)

// This file implements a second distillation strategy modeled on the
// open-source pi coding agent's compaction algorithm
// (github.com/badlogic/pi-mono, packages/agent/src/harness/compaction).
//
// Unlike the default Shelley distillation — which collapses the entire
// conversation into a single hand-written-style "briefing" message — the pi
// strategy splits the conversation at a cut point: older messages are
// summarized with a structured checkpoint prompt, while recent messages
// (≈ keepRecentTokens worth) are copied VERBATIM into the new generation so
// the agent retains exact recent tool calls, results, and edits. The summary
// is inserted as the first context message, wrapped so the LLM understands it
// replaces compacted history.

const defaultPiKeepRecentTokens = 20000

// piSummarizationSystemPrompt and the prompt bodies are ported verbatim from
// pi's compaction.ts so behavior matches the upstream implementation.
const piSummarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

const piSummarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// piCompactionSummaryPrefix/Suffix wrap the summary when it is presented to the
// LLM as the opening context message, matching pi's COMPACTION_SUMMARY_PREFIX.
const piCompactionSummaryPrefix = `The conversation history before this point was compacted into the following summary:

<summary>
`

const piCompactionSummarySuffix = `
</summary>`

// isToolResultMessage reports whether a message carries only tool_result
// content. Such messages are never valid cut points: they must stay paired
// with the assistant tool_use that produced them.
func isToolResultMessage(msg llm.Message) bool {
	hasToolResult := false
	for _, c := range msg.Content {
		if c.Type == llm.ContentTypeToolResult {
			hasToolResult = true
		} else if c.Type != llm.ContentTypeText {
			// Other content alongside tool_result is unusual; treat presence
			// of a non-tool-result, non-text block as making this not a pure
			// tool-result message.
			return false
		}
	}
	return hasToolResult
}

// serializePiConversation renders messages into the plain-text transcript pi
// feeds to the summarization model. Ported from pi's serializeConversation.
func serializePiConversation(messages []llm.Message) string {
	const toolResultMaxChars = 2000
	var parts []string

	for _, msg := range messages {
		switch msg.Role {
		case llm.MessageRoleUser:
			// A user message may carry tool results (Shelley stores tool
			// results as user-role messages) or ordinary text.
			if isToolResultMessage(msg) {
				var text string
				for _, c := range msg.Content {
					for _, r := range c.ToolResult {
						if r.Type == llm.ContentTypeText {
							text += r.Text
						}
					}
				}
				if text != "" {
					parts = append(parts, "[Tool result]: "+truncateForSummary(text, toolResultMaxChars))
				}
				continue
			}
			var text strings.Builder
			for _, c := range msg.Content {
				if c.Type == llm.ContentTypeText {
					text.WriteString(c.Text)
				}
			}
			if text.Len() > 0 {
				parts = append(parts, "[User]: "+text.String())
			}
		case llm.MessageRoleAssistant:
			var textParts, thinkingParts, toolCalls []string
			for _, c := range msg.Content {
				switch c.Type {
				case llm.ContentTypeText:
					textParts = append(textParts, c.Text)
				case llm.ContentTypeThinking:
					thinkingParts = append(thinkingParts, c.Thinking)
				case llm.ContentTypeToolUse:
					toolCalls = append(toolCalls, fmt.Sprintf("%s(%s)", c.ToolName, string(c.ToolInput)))
				}
			}
			if len(thinkingParts) > 0 {
				parts = append(parts, "[Assistant thinking]: "+strings.Join(thinkingParts, "\n"))
			}
			if len(textParts) > 0 {
				parts = append(parts, "[Assistant]: "+strings.Join(textParts, "\n"))
			}
			if len(toolCalls) > 0 {
				parts = append(parts, "[Assistant tool calls]: "+strings.Join(toolCalls, "; "))
			}
		}
	}

	return strings.Join(parts, "\n\n")
}

func truncateForSummary(text string, maxChars int) string {
	if len(text) <= maxChars {
		return text
	}
	truncated := truncateUTF8(text, maxChars)
	return fmt.Sprintf("%s\n\n[... %d more characters truncated]", truncated, len(text)-maxChars)
}

// extractPiFileOps walks summarized assistant messages and records file paths
// touched by read/patch tools, mirroring pi's file-operation tracking so the
// summary can list read vs. modified files.
func extractPiFileOps(messages []llm.Message) (readFiles, modifiedFiles []string) {
	read := map[string]bool{}
	modified := map[string]bool{}
	for _, msg := range messages {
		if msg.Role != llm.MessageRoleAssistant {
			continue
		}
		for _, c := range msg.Content {
			if c.Type != llm.ContentTypeToolUse || len(c.ToolInput) == 0 {
				continue
			}
			var args map[string]json.RawMessage
			if err := json.Unmarshal(c.ToolInput, &args); err != nil {
				continue
			}
			path := jsonStringField(args, "path")
			if path == "" {
				continue
			}
			// Shelley tool names that carry a "path" argument. There is no
			// plain "read" tool (file reads go through bash); "patch" is the
			// only file-mutating tool with a path.
			switch c.ToolName {
			case "read_image":
				read[path] = true
			case "patch":
				modified[path] = true
			}
		}
	}
	for f := range read {
		if !modified[f] {
			readFiles = append(readFiles, f)
		}
	}
	for f := range modified {
		modifiedFiles = append(modifiedFiles, f)
	}
	sort.Strings(readFiles)
	sort.Strings(modifiedFiles)
	return readFiles, modifiedFiles
}

func jsonStringField(args map[string]json.RawMessage, key string) string {
	raw, ok := args[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func formatPiFileOperations(readFiles, modifiedFiles []string) string {
	var sections []string
	if len(readFiles) > 0 {
		sections = append(sections, "<read-files>\n"+strings.Join(readFiles, "\n")+"\n</read-files>")
	}
	if len(modifiedFiles) > 0 {
		sections = append(sections, "<modified-files>\n"+strings.Join(modifiedFiles, "\n")+"\n</modified-files>")
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(sections, "\n\n")
}

// piContextMessage pairs the LLM form of a source message with the original DB
// row, so the pi flow can (a) resolve distillation-summary content for
// summarization and (b) preserve user_data when copying messages verbatim into
// the new generation.
type piContextMessage struct {
	llm    llm.Message
	source generated.Message
}

// piContextMessages converts the source generation's context-eligible messages
// into llm.Messages (preserving roles and tool structure), filtering out
// system/error/gitinfo/warning/slug messages and anything excluded from context.
// Each returned entry retains its source DB row.
func piContextMessages(sourceGeneration int64, messages []generated.Message) []piContextMessage {
	var out []piContextMessage
	for _, m := range messages {
		if m.Generation != sourceGeneration || m.ExcludedFromContext {
			continue
		}
		switch m.Type {
		case string(db.MessageTypeSystem), string(db.MessageTypeError),
			string(db.MessageTypeGitInfo), string(db.MessageTypeWarning),
			string(db.MessageTypeSlug):
			continue
		}
		llmMsg, err := convertToLLMMessage(m)
		if err != nil {
			continue
		}
		out = append(out, piContextMessage{llm: llmMsg, source: m})
	}
	return out
}

// resolveDistilledContent returns the real distillation summary text for a
// previously-distilled message. The message's llm_data only holds a
// placeholder ("Distillation written to ..."); the actual summary lives in
// user_data (or the editable temp file it points at). Mirrors
// ConversationManager.applyDistillationContentOverride. Returns ok=false when
// the message is not a distilled message.
func resolveDistilledContent(logger logWarner, m generated.Message) (string, bool) {
	if m.UserData == nil {
		return "", false
	}
	var userData map[string]string
	if err := json.Unmarshal([]byte(*m.UserData), &userData); err != nil {
		return "", false
	}
	if userData["distilled"] != "true" {
		return "", false
	}
	content := userData["distillation_content"]
	if filePath := userData["distillation_file"]; filePath != "" {
		if !isDistillationTempFile(filePath) {
			logger.Warn("Distillation file path validation failed", "messageID", m.MessageID, "path", filePath)
		} else if fileContent, err := os.ReadFile(filePath); err == nil {
			content = string(fileContent)
		} else {
			logger.Warn("Failed to read editable distillation file; using stored content", "messageID", m.MessageID, "path", filePath, "error", err)
		}
	}
	return content, true
}

// logWarner is the subset of *slog.Logger used by resolveDistilledContent.
type logWarner interface {
	Warn(msg string, args ...any)
}

// resolvePiSummarizationText returns the message text to feed the summarizer,
// substituting the real summary for any distilled-message placeholder.
func resolvePiSummarizationText(logger logWarner, entry piContextMessage) llm.Message {
	content, ok := resolveDistilledContent(logger, entry.source)
	if !ok {
		return entry.llm
	}
	msg := entry.llm
	// Copy the content slice so we don't mutate the shared message.
	newContent := make([]llm.Content, len(msg.Content))
	copy(newContent, msg.Content)
	replaced := false
	for i := range newContent {
		if newContent[i].Type == llm.ContentTypeText {
			newContent[i].Text = content
			replaced = true
			break
		}
	}
	if !replaced {
		newContent = append(newContent, llm.Content{Type: llm.ContentTypeText, Text: content})
	}
	msg.Content = newContent
	return msg
}

// userDataForCopy extracts the parsed user_data map from a source message so it
// can be preserved when copying the message into the new generation. Returns
// nil when there is none.
func userDataForCopy(m generated.Message) map[string]string {
	if m.UserData == nil {
		return nil
	}
	var userData map[string]string
	if err := json.Unmarshal([]byte(*m.UserData), &userData); err != nil {
		return nil
	}
	return userData
}

func piPurposedUsage(chat damodel.Chat, tokens *dmessage.Usage, started, finished time.Time) []llm.PurposedUsage {
	if tokens == nil {
		return nil
	}
	profile := chat.Profile()
	return []llm.PurposedUsage{{Purpose: "compaction", Usage: llm.Usage{
		InputTokens:              uint64(max(0, tokens.InputTokens)),
		CacheCreationInputTokens: uint64(max(0, tokens.InputDetails["cache_creation"])),
		CacheReadInputTokens:     uint64(max(0, tokens.InputDetails["cache_read"])),
		OutputTokens:             uint64(max(0, tokens.OutputTokens)),
		CostUSD:                  tokens.CostUSD,
		Model:                    profile.Model,
		URL:                      tokens.URL,
		StartTime:                &started,
		EndTime:                  &finished,
	}}}
}

// rollbackCompactionFailure restores the conversation to its pre-compaction
// generation and inserts a distill error message. The generation counter is
// bumped before summarization runs, so a failure would otherwise leave the
// conversation on an EMPTY new generation — silently wiping its working
// context and making forks of it empty. Rolling back keeps the old (intact)
// generation active; the error message (inserted after the rollback, so it
// lands in the restored generation) tells the user compaction failed. The
// already-written new-generation rows (the "Distilling…" status and a fresh
// system prompt) are left in place: messages are append-only, and they are
// invisible to context once current_generation points back at the old
// generation. The conversation manager's loop is reset so the next turn
// rehydrates from the restored generation.
//
// Two deliberate non-rollbacks: (1) a model/cwd change requested with the
// compaction stays (it was validated, and the user asked for it); (2) the
// abandoned generation's rows are not deleted — a later retry re-increments
// into the same generation number and Hydrate's hasSystemMessage guard
// prevents a duplicate system prompt.
func (s *Server) rollbackCompactionFailure(ctx context.Context, logger *slog.Logger, conversationID, errMsg string, sourceGeneration int64) {
	if err := s.db.QueriesTx(ctx, func(q *generated.Queries) error {
		_, err := q.SetConversationGeneration(ctx, generated.SetConversationGenerationParams{
			CurrentGeneration: sourceGeneration,
			ConversationID:    conversationID,
		})
		return err
	}); err != nil {
		logger.Error("Failed to roll back generation after compaction failure", "error", err)
		s.insertDistillError(ctx, conversationID, errMsg)
		return
	}
	s.mu.Lock()
	manager, ok := s.activeConversations[conversationID]
	s.mu.Unlock()
	if ok {
		manager.ResetLoop()
	}
	logger.Info("rolled back generation after compaction failure", "generation", sourceGeneration)
	s.insertDistillError(ctx, conversationID, errMsg+" — the conversation was left uncompacted.")
}

// performPiDistillation summarizes older history and copies recent messages
// verbatim into the conversation's (already-incremented) new generation. It is
// the pi-algorithm counterpart to performDistillation.
func (s *Server) performPiDistillation(ctx context.Context, conversationID, sourceSlug, modelID, instructions string, sourceGeneration int64, messages []generated.Message) string {
	logger := s.logger.With("conversationID", conversationID, "sourceSlug", sourceSlug, "method", "compact")

	chat, err := nativeChatFor(s.llmManager, modelID)
	if err != nil {
		logger.Error("Failed to get native model for pi distillation", "model", modelID, "error", err)
		// The generation was already incremented; roll back so the old
		// (intact) generation stays active (see rollbackCompactionFailure).
		s.rollbackCompactionFailure(ctx, logger, conversationID, fmt.Sprintf("Compaction failed: model %q unavailable: %v", modelID, err), sourceGeneration)
		return ""
	}

	ctxMsgs := piContextMessages(sourceGeneration, messages)
	if len(ctxMsgs) == 0 {
		logger.Warn("pi distillation found no context messages")
		s.insertDistillStatus(ctx, conversationID, "complete")
		return ""
	}

	keepRecentTokens := defaultPiKeepRecentTokens
	if s.piDistillKeepRecentTokens > 0 {
		keepRecentTokens = s.piDistillKeepRecentTokens
	}
	llmMsgs := make([]llm.Message, len(ctxMsgs))
	for i, entry := range ctxMsgs {
		llmMsgs[i] = entry.llm
	}
	var nativeMessages []dmessage.Message
	validCutoffs := []int{0}
	sourceCutoff := map[int]int{0: 0}
	for i, message := range llmMsgs {
		converted, convertErr := loopapp.MessagesToNative([]llm.Message{message})
		if convertErr != nil {
			logger.Error("failed to convert messages for compaction", "error", convertErr)
			s.rollbackCompactionFailure(ctx, logger, conversationID, fmt.Sprintf("Compaction failed: %v", convertErr), sourceGeneration)
			return ""
		}
		nativeMessages = append(nativeMessages, converted...)
		validCutoffs = append(validCutoffs, len(nativeMessages))
		sourceCutoff[len(nativeMessages)] = i + 1
	}
	steering := ""
	if strings.TrimSpace(instructions) != "" {
		steering = steeringSection(instructions)
	}
	var olderMsgs []llm.Message
	distillCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	compacted, err := dago.CompactConversation(distillCtx, chat, nativeMessages,
		dago.WithCompactKeepTokens(keepRecentTokens), dago.WithCompactCutoffs(validCutoffs...),
		dago.WithCompactSystemPrompt(piSummarizationSystemPrompt), dago.WithCompactPrompt(piSummarizationPrompt),
		dago.WithCompactInstructions(steering), dago.WithCompactReasoning(damodel.Reasoning{Effort: "off"}),
		dago.WithCompactHistoryFormatter(func(older []dmessage.Message) (string, error) {
			cut, ok := sourceCutoff[len(older)]
			if !ok {
				return "", fmt.Errorf("compaction selected non-record cutoff %d", len(older))
			}
			olderMsgs = make([]llm.Message, cut)
			for i := range olderMsgs {
				olderMsgs[i] = resolvePiSummarizationText(logger, ctxMsgs[i])
			}
			return fmt.Sprintf("<conversation>\n%s\n</conversation>", serializePiConversation(olderMsgs)), nil
		}),
	)
	cancel()
	if err != nil {
		logger.Error("pi summarization failed", "error", err)
		s.rollbackCompactionFailure(ctx, logger, conversationID, fmt.Sprintf("Compaction failed: %v", err), sourceGeneration)
		return ""
	}
	cut, ok := sourceCutoff[compacted.Cutoff]
	if !ok {
		logger.Error("compaction returned non-record cutoff", "cutoff", compacted.Cutoff)
		s.rollbackCompactionFailure(ctx, logger, conversationID, "Compaction failed: invalid message boundary", sourceGeneration)
		return ""
	}
	older := ctxMsgs[:cut]
	recent := ctxMsgs[cut:]
	logger.Info("pi cut point computed", "total", len(ctxMsgs), "summarized", len(older), "kept", len(recent))

	summary := compacted.Summary
	if summary != "" {
		readFiles, modifiedFiles := extractPiFileOps(olderMsgs)
		summary += formatPiFileOperations(readFiles, modifiedFiles)
	}
	otherUsage := piPurposedUsage(chat, compacted.Usage, compacted.Started, compacted.Finished)

	// Insert the summary as the opening context message. Unlike the default
	// distill flow, the compaction summary is NOT editable: it is a generated
	// checkpoint paired with a verbatim recent tail, so editing it in isolation
	// would be misleading. We therefore store the summary text inline in
	// user_data (no editable temp file) and put it directly in the message
	// body so it renders as-is.
	wrapped := piCompactionSummaryPrefix + summary + piCompactionSummarySuffix
	// Build the summary message (if any) plus the verbatim recent tail, then
	// write them all in ONE transaction. Doing each in its own Tx fired a
	// full conversation-list recompute per message (one per commit hook),
	// which made the stream load visibly slow — you could watch the carried
	// count tick up. A single batch is one commit, one recompute, one SSE frame.
	var batch []recordMessageInput
	if summary != "" {
		// The summary is a user-role message; the kept tail (recent[0]) may also
		// be a user message, producing two consecutive user messages. That is
		// fine: Shelley already emits consecutive user messages when a user
		// queues several turns (loop appends them without merging), and pi's own
		// compaction inserts its summary the same way.
		summaryMessage := llm.Message{
			Role: llm.MessageRoleUser,
			Content: []llm.Content{
				{Type: llm.ContentTypeText, Text: wrapped},
			},
		}
		userData := map[string]string{
			"distilled":            "true",
			"distillation_content": wrapped,
			"distill_method":       distillMethodCompact,
		}
		// Attach the summarization calls' usage (primary + fallback) to the
		// summary message so compaction cost is visible in cost reporting.
		batch = append(batch, recordMessageInput{message: summaryMessage, otherUsage: otherUsage, userData: []interface{}{userData}})
	}

	// Copy recent messages verbatim into the new generation so the agent keeps
	// exact recent tool calls and results. The copies are re-recorded with ZERO
	// usage (usage_data all zeros) and NULL other_usage_data: the original rows
	// in the previous generation keep the real numbers, so the cost was already
	// counted once. Preserve each message's user_data so
	// a previously-distilled message in the kept tail keeps its distilled=true
	// marker — otherwise applyDistillationContentOverride would never fire and
	// its real summary text would be lost. Stamp compaction_carried=true on every
	// copy so the UI can collapse the re-played tail behind a "messages carried
	// forward" band instead of re-rendering each one (slow, jarring scroll).
	for _, entry := range recent {
		ud := userDataForCopy(entry.source)
		if ud == nil {
			ud = map[string]string{}
		}
		ud["compaction_carried"] = "true"
		batch = append(batch, recordMessageInput{message: entry.llm, userData: []interface{}{ud}})
	}

	// Append the terminal "complete" status message as an additional INSERT in
	// the SAME batch that writes the summary + carried tail, so it rides along in
	// one commit (one conversation-list recompute, one SSE frame) rather than
	// paying a second commit. Messages are immutable, so instead of mutating the
	// "Compacting…" in_progress message we emit a second message and let the UI
	// collapse the pair. Fall back to a standalone insert if the batch is empty
	// (recordMessages is a no-op for an empty batch) or the in_progress message
	// can't be located.
	statusMsg, statusData, haveStatus := s.terminalDistillStatusMessage(ctx, conversationID, "complete")
	foldedStatus := haveStatus && len(batch) > 0
	if foldedStatus {
		batch = append(batch, recordMessageInput{
			message:  statusMsg,
			userData: []interface{}{statusData},
		})
	}
	if rerr := s.recordMessages(ctx, conversationID, batch); rerr != nil {
		logger.Error("Failed to record compaction messages", "error", rerr)
		// Same empty-new-generation hazard as a summarization failure.
		s.rollbackCompactionFailure(ctx, logger, conversationID, fmt.Sprintf("Compaction failed: could not record messages: %v", rerr), sourceGeneration)
		return ""
	}
	if !foldedStatus {
		s.insertDistillStatus(ctx, conversationID, "complete")
	}
	logger.Info("pi distillation complete", "summary_length", len(summary), "kept_messages", len(recent))
	return summary
}

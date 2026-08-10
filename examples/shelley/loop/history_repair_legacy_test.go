package loop

import "shelley.exe.dev/llm"

// This copied contract helper is test-only. Production history repair is
// performed by Dago's PatchToolCallsMiddleware.
// repairMessageHistory fixes tool_result issues in persisted conversation history:
//  1. Adds error results for tool_uses that were requested but not included in the next message.
//     This can happen when a request is cancelled or fails after the LLM responds with tool_use
//     blocks but before the tools execute.
//  2. Removes orphan tool_results that reference tool_use IDs not present in the immediately
//     preceding assistant message. This can happen when a tool execution completes after
//     CancelConversation has already written cancellation messages.
//
// This prevents API errors like:
//   - "tool_use ids were found without tool_result blocks"
//   - "unexpected tool_use_id found in tool_result blocks ... Each tool_result block must have
//     a corresponding tool_use block in the previous message"
func (l *Loop) repairMessageHistory(messages []llm.Message) []llm.Message {
	if len(messages) < 1 {
		return messages
	}

	// Scan through all messages looking for assistant messages with tool_use
	// that are not immediately followed by a user message with corresponding tool_results.
	// We may need to insert synthetic user messages with tool_results or filter orphans.
	var newMessages []llm.Message
	totalInserted := 0
	totalRemoved := 0

	// Track the tool_use IDs from the most recent assistant message
	var prevAssistantToolUseIDs map[string]bool

	for i := 0; i < len(messages); i++ {
		msg := messages[i]

		if msg.Role == llm.MessageRoleAssistant {
			// Handle empty assistant messages - add placeholder content if not the last message
			// The API requires all messages to have non-empty content except for the optional
			// final assistant message. Empty content can happen when the model ends its turn
			// without producing any output.
			if len(msg.Content) == 0 && i < len(messages)-1 {
				msg.Content = []llm.Content{{Type: llm.ContentTypeText, Text: "(no response)"}}
				l.logger.Debug("added placeholder content to empty assistant message", "index", i)
			}

			// Track all tool_use IDs in this assistant message
			prevAssistantToolUseIDs = make(map[string]bool)
			for _, c := range msg.Content {
				if c.Type == llm.ContentTypeToolUse {
					prevAssistantToolUseIDs[c.ID] = true
				}
			}
			newMessages = append(newMessages, msg)

			// Check if next message needs synthetic tool_results
			var toolUseContents []llm.Content
			for _, c := range msg.Content {
				if c.Type == llm.ContentTypeToolUse {
					toolUseContents = append(toolUseContents, c)
				}
			}

			if len(toolUseContents) == 0 {
				continue
			}

			// Check if next message is a user message with corresponding tool_results
			var nextMsg *llm.Message
			if i+1 < len(messages) {
				nextMsg = &messages[i+1]
			}

			if nextMsg == nil || nextMsg.Role != llm.MessageRoleUser {
				// Next message is not a user message (or there is no next message).
				// Insert a synthetic user message with tool_results for all tool_uses.
				var toolResultContent []llm.Content
				for _, tu := range toolUseContents {
					toolResultContent = append(toolResultContent, llm.Content{
						Type:      llm.ContentTypeToolResult,
						ToolUseID: tu.ID,
						ToolError: true,
						ToolResult: []llm.Content{{
							Type: llm.ContentTypeText,
							Text: "not executed; retry possible",
						}},
					})
				}
				syntheticMsg := llm.Message{
					Role:    llm.MessageRoleUser,
					Content: toolResultContent,
				}
				newMessages = append(newMessages, syntheticMsg)
				totalInserted += len(toolResultContent)
			}
		} else if msg.Role == llm.MessageRoleUser {
			// Filter out orphan tool_results and add missing ones
			var filteredContent []llm.Content
			existingResultIDs := make(map[string]bool)

			for _, c := range msg.Content {
				if c.Type == llm.ContentTypeToolResult {
					// Only keep tool_results that match a tool_use in the previous assistant message
					if prevAssistantToolUseIDs != nil && prevAssistantToolUseIDs[c.ToolUseID] {
						filteredContent = append(filteredContent, c)
						existingResultIDs[c.ToolUseID] = true
					} else {
						// Orphan tool_result - skip it
						totalRemoved++
						l.logger.Debug("removing orphan tool_result", "tool_use_id", c.ToolUseID)
					}
				} else {
					// Keep non-tool_result content
					filteredContent = append(filteredContent, c)
				}
			}

			// Check if we need to add missing tool_results for this user message
			if prevAssistantToolUseIDs != nil {
				var prefix []llm.Content
				for toolUseID := range prevAssistantToolUseIDs {
					if !existingResultIDs[toolUseID] {
						prefix = append(prefix, llm.Content{
							Type:      llm.ContentTypeToolResult,
							ToolUseID: toolUseID,
							ToolError: true,
							ToolResult: []llm.Content{{
								Type: llm.ContentTypeText,
								Text: "not executed; retry possible",
							}},
						})
						totalInserted++
					}
				}
				if len(prefix) > 0 {
					filteredContent = append(prefix, filteredContent...)
				}
			}

			// Only add the message if it has content
			if len(filteredContent) > 0 {
				msg.Content = filteredContent
				newMessages = append(newMessages, msg)
			} else {
				// Message is now empty after filtering - skip it entirely
				l.logger.Debug("removing empty user message after filtering orphan tool_results")
			}

			// Reset for next iteration - user message "consumes" the previous tool_uses
			prevAssistantToolUseIDs = nil
		} else {
			newMessages = append(newMessages, msg)
		}
	}

	if totalInserted > 0 || totalRemoved > 0 {
		if totalInserted > 0 {
			l.logger.Debug("inserted missing tool results", "count", totalInserted)
		}
		if totalRemoved > 0 {
			l.logger.Debug("removed orphan tool results", "count", totalRemoved)
		}
	}
	return newMessages
}

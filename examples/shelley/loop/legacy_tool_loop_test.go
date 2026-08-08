package loop

import (
	"context"
	"fmt"
	"time"

	"shelley.exe.dev/llm"
	"shelley.exe.dev/llm/llmhttp"
)

// executeToolCalls preserves the original package-private unit-test surface.
// Production tool execution is owned by the Dago agent graph.
func (l *Loop) executeToolCalls(ctx context.Context, content []llm.Content) error {
	var toolResults []llm.Content
	var otherUsage llmhttp.UsageAccumulator
	ctx = llmhttp.WithUsageCollector(ctx, otherUsage.Collect)

	for _, c := range content {
		if c.Type != llm.ContentTypeToolUse {
			continue
		}
		l.logger.Debug("executing tool", "name", c.ToolName, "id", c.ID)

		var selected *llm.Tool
		for _, candidate := range l.tools {
			if candidate.Name == c.ToolName {
				selected = candidate
				break
			}
		}
		if selected == nil {
			l.logger.Error("tool not found", "name", c.ToolName)
			toolResults = append(toolResults, llm.Content{
				Type: llm.ContentTypeToolResult, ToolUseID: c.ID, ToolError: true,
				ToolResult: []llm.Content{{Type: llm.ContentTypeText, Text: fmt.Sprintf("Tool '%s' not found", c.ToolName)}},
			})
			continue
		}

		toolCtx := ctx
		if l.workingDir != "" {
			toolCtx = llm.WithWorkingDir(toolCtx, l.workingDir)
		}
		if l.onToolProgress != nil {
			toolCtx = llm.WithToolProgress(toolCtx, l.onToolProgress)
		}
		toolCtx = llm.WithToolUseID(toolCtx, c.ID)
		toolCtx = llm.WithLLMService(toolCtx, l.llm)
		started := time.Now()
		output := selected.Run(toolCtx, c.ToolInput)
		finished := time.Now()
		content := output.LLMContent
		if output.Error != nil {
			l.logger.Error("tool execution failed", "name", c.ToolName, "error", output.Error)
			content = llm.TextContent(output.Error.Error())
		}
		toolResults = append(toolResults, llm.Content{
			Type: llm.ContentTypeToolResult, ToolUseID: c.ID, ToolError: output.Error != nil,
			ToolResult: content, ToolUseStartTime: &started, ToolUseEndTime: &finished, Display: output.Display,
		})
	}

	if len(toolResults) == 0 {
		return nil
	}
	toolMessage := llm.Message{Role: llm.MessageRoleUser, Content: toolResults}
	l.mu.Lock()
	l.history = append(l.history, toolMessage)
	if len(l.messageQueue) > 0 {
		l.history = append(l.history, l.messageQueue...)
		l.messageQueue = l.messageQueue[:0]
		l.logger.Info("processing user interruption during tool execution")
	}
	l.mu.Unlock()
	if err := l.recordMessage(ctx, toolMessage, llm.Usage{}, otherUsage.Take()); err != nil {
		l.logger.Error("failed to record tool result message", "error", err)
	}
	return nil
}

package dago

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
)

const (
	nemotronDefaultReadLimit = 500
	nemotronEmptyToolResult  = "(empty tool result)"
)

var (
	nemotronFunctionBlock    = regexp.MustCompile(`(?s)<function=([^>\s]+)\s*>(.*?)</function>`)
	nemotronParameter        = regexp.MustCompile(`(?s)<parameter\s+name=([^>\s]+)\s*>(.*?)</parameter>`)
	nemotronAltFunctionBlock = regexp.MustCompile(`(?is)<function>\s*(.*?)</function>\s*(?:</tool_call>)?`)
	nemotronAltName          = regexp.MustCompile(`(?is)<name\s*>(.*?)</name>|<name=([^<>\s]+)</name>`)
	nemotronAltParameter     = regexp.MustCompile(`(?is)<parameter(?:\s+name=([^>\s]+))?\s*>(.*?)</parameter>`)
	nemotronAltInlineArg     = regexp.MustCompile(`(?s)^\s*<?([A-Za-z_][\w-]*)>?\s*:\s*(.*?)\s*$`)
	nemotronThinkBlock       = regexp.MustCompile(`(?is)<think\b[^>]*>(.*?)</think>\s*`)
	nemotronToolID           atomic.Uint64
)

var nemotronFilePathTools = map[string]bool{
	"read_file": true, "write_file": true, "edit_file": true, "delete": true,
}

var nemotronFilesystemTools = map[string]bool{
	"ls": true, "read_file": true, "write_file": true, "edit_file": true,
	"delete": true, "glob": true, "grep": true,
}

// NemotronToolCallShim repairs common file-path arguments, expands its default
// read window, and gives otherwise empty ordinary tool results visible content.
func NemotronToolCallShim() agent.Middleware {
	return agent.Middleware{Name: "NemotronToolCallShim", WrapToolCall: func(ctx context.Context, request agent.ToolCallRequest, next agent.ToolHandler) (agent.ToolCallResponse, error) {
		if nemotronFilePathTools[request.Call.Name] {
			var arguments map[string]any
			if json.Unmarshal(request.Call.Arguments, &arguments) == nil {
				changed := false
				if value, exists := arguments["path"]; exists {
					if _, hasFilePath := arguments["file_path"]; !hasFilePath {
						arguments["file_path"] = value
						delete(arguments, "path")
						changed = true
					}
				}
				if request.Call.Name == "read_file" {
					if _, exists := arguments["limit"]; !exists {
						arguments["limit"] = nemotronDefaultReadLimit
						changed = true
					}
				}
				if changed {
					encoded, err := json.Marshal(arguments)
					if err != nil {
						return agent.ToolCallResponse{}, err
					}
					request.Call.Arguments = encoded
				}
			}
		}
		response, err := next(ctx, request)
		if err != nil {
			return response, err
		}
		if response.Result.Interrupt == nil && len(response.Result.Update) == 0 && toolResultContentEmpty(response.Result.Content) {
			response.Result.Content = []message.ContentBlock{{Type: message.BlockText, Text: nemotronEmptyToolResult}}
		}
		return response, nil
	}}
}

func toolResultContentEmpty(content []message.ContentBlock) bool {
	if len(content) == 0 {
		return true
	}
	for _, block := range content {
		if block.Type != message.BlockText || block.Text != "" {
			return false
		}
	}
	return true
}

// NemotronReadContinuationNotice marks exactly-full read windows so the model
// does not mistake pagination for end-of-file.
func NemotronReadContinuationNotice() agent.Middleware {
	return agent.Middleware{Name: "ReadFileContinuationNoticeMiddleware", WrapToolCall: func(ctx context.Context, request agent.ToolCallRequest, next agent.ToolHandler) (agent.ToolCallResponse, error) {
		response, err := next(ctx, request)
		if err != nil || request.Call.Name != "read_file" {
			return response, err
		}
		text := resultText(response.Result.Content)
		if text == "" || strings.HasPrefix(text, "Error") {
			return response, nil
		}
		offset, limit := 0, nemotronDefaultReadLimit
		var arguments map[string]any
		if json.Unmarshal(request.Call.Arguments, &arguments) == nil {
			offset = integerArgument(arguments["offset"], 0)
			limit = integerArgument(arguments["limit"], nemotronDefaultReadLimit)
		}
		if limit <= 0 {
			return response, nil
		}
		rows := 0
		for _, row := range strings.Split(text, "\n") {
			if isNumberedReadRow(row) {
				rows++
			}
		}
		if rows < limit {
			return response, nil
		}
		notice := fmt.Sprintf("\n\n[read_file returned %d lines starting at offset %d, the per-read limit. The file likely continues past this window. To read further, call read_file again with offset=%d. Do not assume you have seen the end of the file.]", limit, offset, offset+limit)
		response.Result.Content = append(response.Result.Content, message.ContentBlock{Type: message.BlockText, Text: notice})
		return response, nil
	}}
}

func resultText(content []message.ContentBlock) string {
	var result strings.Builder
	for _, block := range content {
		if block.Type == message.BlockText {
			result.WriteString(block.Text)
		}
	}
	return result.String()
}

func integerArgument(value any, fallback int) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case json.Number:
		if parsed, err := strconv.Atoi(typed.String()); err == nil {
			return parsed
		}
	case string:
		if parsed, err := strconv.Atoi(typed); err == nil {
			return parsed
		}
	}
	return fallback
}

func isNumberedReadRow(row string) bool {
	index := 0
	for index < len(row) && row[index] == ' ' {
		index++
	}
	start := index
	for index < len(row) && row[index] >= '0' && row[index] <= '9' {
		index++
	}
	return index > start && index < len(row) && (row[index] == '\t' || (index+1 < len(row) && row[index] == ' ' && row[index+1] == ' '))
}

// NemotronModelRateLimitRetry retries transient model throttling while honoring
// cancellation. Delays correspond to retries after the initial attempt.
func NemotronModelRateLimitRetry(delays ...time.Duration) agent.Middleware {
	if delays == nil {
		delays = []time.Duration{4 * time.Second, 12 * time.Second}
	}
	delays = append([]time.Duration(nil), delays...)
	return agent.Middleware{Name: "ModelRateLimitRetryMiddleware", WrapModelCall: func(ctx context.Context, request agent.ModelRequest, next agent.ModelHandler) (agent.ModelResponse, error) {
		for attempt := 0; ; attempt++ {
			response, err := next(ctx, request)
			if err == nil || attempt >= len(delays) || !nemotronRateLimitError(err) {
				return response, err
			}
			if delays[attempt] <= 0 {
				continue
			}
			timer := time.NewTimer(delays[attempt])
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return agent.ModelResponse{}, ctx.Err()
			}
		}
	}}
}

func nemotronRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	if reporter, ok := err.(model.RetryReporter); ok {
		if event := reporter.RetryEvent(1, 0); event.Status == 429 {
			return true
		}
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "rate limit") || strings.Contains(text, "rate-limit") || strings.Contains(text, "status 429")
}

// NemotronReasoningTagCleanup moves textual think blocks into provider-neutral
// reasoning content and keeps only visible text in the assistant answer.
func NemotronReasoningTagCleanup() agent.Middleware {
	return agent.Middleware{Name: "NemotronReasoningTagCleanupMiddleware", WrapModelCall: func(ctx context.Context, request agent.ModelRequest, next agent.ModelHandler) (agent.ModelResponse, error) {
		response, err := next(ctx, request)
		if err != nil {
			return response, err
		}
		for index := range response.Messages {
			response.Messages[index] = stripNemotronReasoning(response.Messages[index])
		}
		return response, nil
	}}
}

func stripNemotronReasoning(value message.Message) message.Message {
	if value.Role != message.RoleAssistant {
		return value
	}
	text := value.TextContent()
	if !strings.Contains(strings.ToLower(text), "</think>") {
		return value
	}
	matches := nemotronThinkBlock.FindAllStringSubmatch(text, -1)
	clean := strings.TrimSpace(nemotronThinkBlock.ReplaceAllString(text, ""))
	if clean == text {
		return value
	}
	blocks := make([]message.ContentBlock, 0, len(value.Content)+1)
	var reasoning []string
	for _, match := range matches {
		if part := strings.TrimSpace(match[1]); part != "" {
			reasoning = append(reasoning, part)
		}
	}
	if len(reasoning) > 0 && !hasReasoningBlock(value.Content) {
		blocks = append(blocks, message.ContentBlock{Type: message.BlockReasoning, Reasoning: strings.Join(reasoning, "\n\n")})
	}
	if clean != "" {
		blocks = append(blocks, message.ContentBlock{Type: message.BlockText, Text: clean})
	}
	for _, block := range value.Content {
		if block.Type != message.BlockText {
			blocks = append(blocks, block)
		}
	}
	value.Content = blocks
	return value
}

func hasReasoningBlock(content []message.ContentBlock) bool {
	for _, block := range content {
		if block.Type == message.BlockReasoning {
			return true
		}
	}
	return false
}

// NemotronTextToolCallParser turns supported text and JSON call formats into
// canonical tool calls, but only for tools visible on the current request.
func NemotronTextToolCallParser() agent.Middleware {
	return agent.Middleware{Name: "NemotronTextToolCallParser", WrapModelCall: func(ctx context.Context, request agent.ModelRequest, next agent.ModelHandler) (agent.ModelResponse, error) {
		response, err := next(ctx, request)
		if err != nil {
			return response, err
		}
		available := make(map[string]bool, len(request.Tools))
		for _, executable := range request.Tools {
			available[executable.Definition().Name] = true
		}
		for index := range response.Messages {
			response.Messages[index] = repairNemotronTextToolCalls(response.Messages[index], available)
		}
		return response, nil
	}}
}

func repairNemotronTextToolCalls(value message.Message, available map[string]bool) message.Message {
	if value.Role != message.RoleAssistant || len(value.ToolCalls) > 0 {
		return value
	}
	value = stripNemotronReasoning(value)
	text := value.TextContent()
	calls, leftover := parseNemotronFunctionCalls(text, available)
	if len(calls) == 0 {
		calls = parseNemotronJSONCall(text, available)
		if len(calls) > 0 {
			leftover = ""
		}
	}
	if len(calls) == 0 {
		return value
	}
	value.ToolCalls = calls
	value.Content = nil
	if leftover != "" {
		value.Content = []message.ContentBlock{{Type: message.BlockText, Text: leftover}}
	}
	return value
}

func parseNemotronFunctionCalls(content string, available map[string]bool) ([]message.ToolCall, string) {
	calls := parseNemotronBlocks(content, available, nemotronFunctionBlock, func(body string) (string, map[string]any) {
		return "", parseNemotronNamedParameters(body)
	})
	if len(calls) > 0 {
		return calls, strings.TrimSpace(strings.ReplaceAll(nemotronFunctionBlock.ReplaceAllString(content, ""), "</tool_call>", ""))
	}
	calls = parseNemotronBlocks(content, available, nemotronAltFunctionBlock, func(body string) (string, map[string]any) {
		return alternateNemotronName(body), alternateNemotronArguments(body)
	})
	if len(calls) == 0 {
		return nil, content
	}
	return calls, strings.TrimSpace(strings.ReplaceAll(nemotronAltFunctionBlock.ReplaceAllString(content, ""), "</tool_call>", ""))
}

func parseNemotronBlocks(content string, available map[string]bool, expression *regexp.Regexp, parse func(string) (string, map[string]any)) []message.ToolCall {
	var calls []message.ToolCall
	for _, match := range expression.FindAllStringSubmatch(content, -1) {
		name := ""
		body := ""
		if expression == nemotronFunctionBlock {
			name, body = trimNemotronToken(match[1]), match[2]
		} else {
			body = match[1]
		}
		parsedName, arguments := parse(body)
		if name == "" {
			name = parsedName
		}
		if name == "" || !available[name] {
			continue
		}
		encoded, err := json.Marshal(arguments)
		if err != nil {
			continue
		}
		calls = append(calls, message.ToolCall{ID: nextNemotronToolID(), Name: name, Arguments: encoded})
	}
	return calls
}

func parseNemotronNamedParameters(body string) map[string]any {
	arguments := map[string]any{}
	for _, match := range nemotronParameter.FindAllStringSubmatch(body, -1) {
		arguments[trimNemotronToken(match[1])] = strings.TrimSpace(match[2])
	}
	return arguments
}

func alternateNemotronName(body string) string {
	match := nemotronAltName.FindStringSubmatch(body)
	if len(match) == 0 {
		return ""
	}
	if match[1] != "" {
		return trimNemotronToken(match[1])
	}
	return trimNemotronToken(match[2])
}

func alternateNemotronArguments(body string) map[string]any {
	arguments := map[string]any{}
	for _, match := range nemotronAltParameter.FindAllStringSubmatch(body, -1) {
		name, raw := trimNemotronToken(match[1]), strings.TrimSpace(match[2])
		if name != "" {
			arguments[name] = raw
			continue
		}
		inline := nemotronAltInlineArg.FindStringSubmatch(raw)
		if len(inline) > 0 {
			arguments[inline[1]] = strings.TrimSpace(inline[2])
		}
	}
	return arguments
}

func parseNemotronJSONCall(content string, available map[string]bool) []message.ToolCall {
	start, end := strings.Index(content, "{"), strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return nil
	}
	var object map[string]any
	if json.Unmarshal([]byte(content[start:end+1]), &object) != nil {
		return nil
	}
	name, ok := object["tool"].(string)
	if !ok {
		return nil
	}
	name = strings.TrimSpace(name)
	switch strings.ToLower(name) {
	case "bash", "sh", "shell":
		name = "execute"
	}
	if name == "" || !available[name] {
		return nil
	}
	arguments, ok := object["args"].(map[string]any)
	if !ok {
		arguments = map[string]any{}
		if command, commandOK := object["cmd"].(string); commandOK {
			arguments["command"] = command
		} else if command, commandOK := object["command"].(string); commandOK {
			arguments["command"] = command
		}
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil
	}
	return []message.ToolCall{{ID: nextNemotronToolID(), Name: name, Arguments: encoded}}
}

func trimNemotronToken(value string) string {
	return strings.Trim(strings.TrimSpace(value), "\"'")
}

func nextNemotronToolID() string {
	return fmt.Sprintf("nemotron-tool-%d", nemotronToolID.Add(1))
}

func nemotronFilesystemRetry() agent.Middleware {
	retry := agent.ToolRetry("ToolRetryMiddleware", 2, 0, nil)
	return agent.Middleware{Name: "ToolRetryMiddleware", WrapToolCall: func(ctx context.Context, request agent.ToolCallRequest, next agent.ToolHandler) (agent.ToolCallResponse, error) {
		if !nemotronFilesystemTools[request.Call.Name] {
			return next(ctx, request)
		}
		return retry.WrapToolCall(ctx, request, next)
	}}
}

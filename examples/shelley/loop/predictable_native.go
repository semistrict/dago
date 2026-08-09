package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
)

func (s *PredictableService) invokeNative(ctx context.Context, request dmodel.Request) (dmodel.Response, error) {
	s.mu.Lock()
	delay := s.responseDelay
	s.recentRequests = append(s.recentRequests, request)
	if len(s.recentRequests) > 10 {
		s.recentRequests = s.recentRequests[len(s.recentRequests)-10:]
	}
	s.mu.Unlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return dmodel.Response{}, ctx.Err()
		}
	}

	inputTokens := countNativeRequestTokens(request)
	inputText, hasToolResult := nativeInput(request)
	if hasToolResult && inputText == "" {
		switch {
		case nativeRequestMentions(request, inlineImageSentinel):
			return nativeText("Here is the generated image:\n\n![demo image]("+inlineImagePath+")\n\nGenerated locally and served from the conversation working directory.", inputTokens), nil
		case nativeRequestMentions(request, screenshotImageSentinel):
			return nativeText("Verified against the real product:\n\n![demo screenshot]("+screenshotImagePath+")\n\nServed from the screenshot directory, outside the working directory.", inputTokens), nil
		default:
			return nativeText("Done.", inputTokens), nil
		}
	}

	switch inputText {
	case "hello":
		return nativeText("Well, hi there!", inputTokens), nil
	case "Hello":
		return nativeText("Hello! I'm Shelley, your AI assistant. How can I help you today?", inputTokens), nil
	case "Create an example":
		return nativeThinking("I'll create a simple example for you.", inputTokens), nil
	case "screenshot":
		return nativeTool("Taking a screenshot...", "browser", map[string]any{"action": "screenshot"}, inputTokens), nil
	case "wide tables":
		return nativeText(wideTablesMarkdown, inputTokens), nil
	case "web search", "citations":
		return nativeWebSearch(inputTokens), nil
	case "tool smorgasbord":
		return nativeToolSmorgasbord(inputTokens), nil
	case "echo: foo":
		return nativeText("foo", inputTokens), nil
	case "patch fail":
		return nativePatch("/nonexistent/file/that/does/not/exist.txt", false, inputTokens), nil
	case "patch success":
		return nativePatch("/tmp/test-patch-success.txt", true, inputTokens), nil
	case "big patch":
		return nativeBigPatch(inputTokens), nil
	case "patch bad json":
		return nativeMalformedPatch(inputTokens), nil
	case "maxTokens":
		return nativeTerminal("This is a truncated response that was cut off mid-sentence because the output token limit was", dmodel.FinishReasonMaxTokens, inputTokens), nil
	case "refusal":
		return nativeTerminal("", dmodel.FinishReasonRefusal, inputTokens), nil
	}

	if text, ok := strings.CutPrefix(inputText, "echo: "); ok {
		return nativeText(text, inputTokens), nil
	}
	if command, ok := strings.CutPrefix(inputText, "bash: "); ok {
		return nativeTool("I'll run the command: "+command, "bash", map[string]any{"command": command}, inputTokens), nil
	}
	if thoughts, ok := strings.CutPrefix(inputText, "think: "); ok {
		return nativeThinking(thoughts, inputTokens), nil
	}
	if path, ok := strings.CutPrefix(inputText, "patch: "); ok {
		return nativePatch(path, false, inputTokens), nil
	}
	if text, ok := strings.CutPrefix(inputText, "fail "); ok {
		message := strings.TrimSpace(text)
		return dmodel.Response{}, predictableRetryError{message: message}
	}
	if text, ok := strings.CutPrefix(inputText, "error: "); ok {
		return dmodel.Response{}, fmt.Errorf("predictable error: %s", text)
	}
	if selector, ok := strings.CutPrefix(inputText, "screenshot: "); ok {
		input := map[string]any{"action": "screenshot"}
		if selector = strings.TrimSpace(selector); selector != "" {
			input["selector"] = selector
		}
		return nativeTool("Taking a screenshot...", "browser", input, inputTokens), nil
	}
	if rest, ok := strings.CutPrefix(inputText, "subagent: "); ok {
		parts := strings.SplitN(rest, " ", 2)
		prompt := "do the task"
		if len(parts) > 1 {
			prompt = parts[1]
		}
		return nativeTool("Delegating to subagent '"+parts[0]+"'...", "subagent", map[string]any{"slug": parts[0], "prompt": prompt}, inputTokens), nil
	}
	if text, ok := strings.CutPrefix(inputText, "markdown: "); ok {
		return nativeText(text, inputTokens), nil
	}
	if inputText == "inline image" {
		command := fmt.Sprintf("printf %%s %q | base64 -d > %s && echo %s", inlineImagePNGBase64, inlineImagePath, inlineImageSentinel)
		return nativeTool("I'll run the command: "+command, "bash", map[string]any{"command": command}, inputTokens), nil
	}
	if inputText == "screenshot image" {
		command := fmt.Sprintf("mkdir -p %s && printf %%s %q | base64 -d > %s && echo %s", screenshotImageDir, inlineImagePNGBase64, screenshotImagePath, screenshotImageSentinel)
		return nativeTool("I'll run the command: "+command, "bash", map[string]any{"command": command}, inputTokens), nil
	}
	if path, ok := strings.CutPrefix(inputText, "change_dir: "); ok {
		return nativeTool("I'll change to directory: "+path, "change_dir", map[string]any{"path": path}, inputTokens), nil
	}
	if path, ok := strings.CutPrefix(inputText, "read_image: "); ok {
		path = strings.TrimSpace(path)
		return nativeTool("Reading "+path+"...", "read_image", map[string]any{"path": path}, inputTokens), nil
	}
	if seconds, ok := strings.CutPrefix(inputText, "delay: "); ok {
		if duration, err := time.ParseDuration(seconds + "s"); err == nil && duration > 0 {
			timer := time.NewTimer(duration)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return dmodel.Response{}, ctx.Err()
			}
		}
		return nativeText("Delayed for "+seconds+" seconds", inputTokens), nil
	}
	if strings.Contains(inputText, "PREDICTABLE_EMPTY_RESPONSE") {
		return nativeText("", inputTokens), nil
	}
	return nativeText("edit predictable.go to add a response for that one...", inputTokens), nil
}

type predictableRetryError struct{ message string }

func (err predictableRetryError) Error() string { return "predictable failure: " + err.message }
func (err predictableRetryError) RetryEvent(attempt int, delay time.Duration) dmodel.RetryEvent {
	return dmodel.RetryEvent{
		Attempt: attempt, Delay: delay, Retryable: true, Err: err.message,
		Provider: "predictable", Model: "predictable-v1",
	}
}

func nativeInput(request dmodel.Request) (string, bool) {
	if len(request.Messages) == 0 {
		return "", false
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role == dmessage.RoleTool {
		return "", true
	}
	if last.Role != dmessage.RoleHuman {
		return "", false
	}
	return strings.TrimSpace(last.TextContent()), false
}

func nativeRequestMentions(request dmodel.Request, needle string) bool {
	for _, item := range request.Messages {
		if strings.Contains(item.TextContent(), needle) {
			return true
		}
	}
	return false
}

func countNativeRequestTokens(request dmodel.Request) int {
	total := 0
	for _, item := range request.Messages {
		for _, block := range item.Content {
			total += len(block.Text) + len(block.Reasoning) + len(block.Data)
		}
		for _, call := range item.ToolCalls {
			total += len(call.Name) + len(call.Arguments)
		}
	}
	for _, definition := range request.Tools {
		total += len(definition.Name) + len(definition.Description) + len(definition.InputSchema)
	}
	return total / 4
}

func nativeText(text string, inputTokens int) dmodel.Response {
	message := dmessage.Assistant(text)
	message.ID = fmt.Sprintf("pred-%d", time.Now().UnixNano())
	message.Usage = nativeUsage(inputTokens, max(1, len(text)/4), 0.001)
	return dmodel.Response{Message: message}
}

func nativeThinking(thoughts string, inputTokens int) dmodel.Response {
	message := dmessage.Assistant("I've considered my approach.")
	message.ID = fmt.Sprintf("pred-thinking-%d", time.Now().UnixNano())
	message.Content = append([]dmessage.ContentBlock{{Type: dmessage.BlockReasoning, Reasoning: thoughts}}, message.Content...)
	message.Usage = nativeUsage(inputTokens, max(1, (len(thoughts)+len(message.TextContent()))/4), 0.002)
	return dmodel.Response{Message: message}
}

func nativeTool(text, name string, input any, inputTokens int) dmodel.Response {
	arguments, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	message := dmessage.Assistant(text)
	message.ID = fmt.Sprintf("pred-%s-%d", name, time.Now().UnixNano())
	message.ToolCalls = []dmessage.ToolCall{{ID: fmt.Sprintf("tool_%d", time.Now().UnixNano()%100000), Name: name, Arguments: arguments}}
	message.Usage = nativeUsage(inputTokens, max(1, (len(text)+len(arguments))/4), 0.002)
	return dmodel.Response{Message: message}
}

func nativePatch(path string, overwrite bool, inputTokens int) dmodel.Response {
	patch := map[string]string{"operation": "replace", "oldText": "example", "newText": "updated example"}
	text := "I'll patch the file: " + path
	if overwrite {
		patch = map[string]string{"operation": "overwrite", "newText": "This is the new content of the file.\nLine 2\nLine 3\n"}
		text = "I'll create/overwrite the file: " + path
	}
	return nativeTool(text, "patch", map[string]any{"path": path, "patches": []map[string]string{patch}}, inputTokens)
}

func nativeBigPatch(inputTokens int) dmodel.Response {
	var body strings.Builder
	body.WriteString("package big\n\n")
	for index := range 200 {
		fmt.Fprintf(&body, "// line %d of a deliberately tall generated file\nfunc Fn%d() int { return %d }\n\n", index, index, index)
	}
	path := fmt.Sprintf("/tmp/shelley-big-patch-%d.go", time.Now().UnixNano())
	return nativeTool("I'll write a large file: "+path, "patch", map[string]any{
		"path": path, "patches": []map[string]string{{"operation": "overwrite", "newText": body.String()}},
	}, inputTokens)
}

func nativeMalformedPatch(inputTokens int) dmodel.Response {
	message := dmessage.Assistant("I'll patch the file with the changes.")
	message.ID = fmt.Sprintf("pred-patch-malformed-%d", time.Now().UnixNano())
	message.ToolCalls = []dmessage.ToolCall{{
		ID: "tool_malformed", Name: "patch",
		Arguments: json.RawMessage(`{"path":"/home/agent/example.css","patch":"<parameter name=\"operation\">replace","oldText":".example {\n  color: red;\n}","newText":".example {\n  color: blue;\n}"}`),
	}}
	message.Usage = nativeUsage(inputTokens, 50, 0.003)
	return dmodel.Response{Message: message}
}

func nativeTerminal(text string, reason dmodel.FinishReason, inputTokens int) dmodel.Response {
	outputTokens := max(1, len(text)/4)
	message := dmessage.Assistant(text)
	message.ID = fmt.Sprintf("pred-%d", time.Now().UnixNano())
	var refusal *dmodel.Refusal
	if reason == dmodel.FinishReasonRefusal {
		message.Content = []dmessage.ContentBlock{{Type: dmessage.BlockReasoning}}
		outputTokens = 42
		refusal = &dmodel.Refusal{Category: "cyber", Explanation: "The model declined to continue with this request."}
	}
	dmodel.SetOutcome(&message, reason, refusal)
	message.Usage = nativeUsage(inputTokens, outputTokens, 0.001)
	return dmodel.Response{Message: message}
}

func nativeUsage(input, output int, cost float64) *dmessage.Usage {
	return &dmessage.Usage{
		InputTokens: input, OutputTokens: output, TotalTokens: input + output,
		Provider: "builtin", Model: "predictable-v1", CostUSD: cost,
	}
}

func nativeWebSearch(inputTokens int) dmodel.Response {
	message := dmessage.Assistant("")
	message.ID = fmt.Sprintf("pred-websearch-%d", time.Now().UnixNano())
	message.Content = []dmessage.ContentBlock{
		{Type: dmessage.BlockServerTool, ID: "search_1", Name: "web_search", Extra: map[string]json.RawMessage{"arguments": json.RawMessage(`{"query":"pi coding agent switch models"}`)}},
		{Type: dmessage.BlockSearchResult, ID: "result_1", Name: "earendil-works/pi: a tiny coding agent", URL: "https://github.com/earendil-works/pi"},
		{Type: dmessage.BlockSearchResult, ID: "result_2", Name: "Pi Docs — Switching models mid-session", URL: "https://pi.dev/docs/models"},
		{Type: dmessage.BlockSearchResult, ID: "result_3", Name: "Model switching workflows with Pi", URL: "https://pi.dev/blog/model-switching"},
		{Type: dmessage.BlockText, Text: "Pi makes mid-session model switching a core feature, so you can change models on an in-progress conversation without losing context.\n\n**Built-in ways to switch:**\n- "},
		{Type: dmessage.BlockText, Text: "Switch models mid-session with /model or Ctrl+L. Cycle through your favorites with Ctrl+P.", Citations: []dmessage.Citation{{URL: "https://pi.dev/docs/models", Title: "Pi Docs — Switching models mid-session", CitedText: "Switch models mid-session with /model or Ctrl+L"}}},
		{Type: dmessage.BlockText, Text: " The `/model` command is the discoverable way if you don't want to remember the shortcut.\n\n**Why this is seamless:** Pi sits on a unified multi-provider API layer, so "},
		{Type: dmessage.BlockText, Text: "mid-session model switching across 15+ providers lets you use Claude for exploration, GPT for a second opinion, Gemini for large context.", Citations: []dmessage.Citation{{URL: "https://github.com/earendil-works/pi", Title: "earendil-works/pi: a tiny coding agent", CitedText: "mid-session model switching across 15+ providers"}}},
		{Type: dmessage.BlockText, Text: "\n\n**Typical workflow people use:**\n"},
		{Type: dmessage.BlockText, Text: "Start with a small model for quick lookups and small edits, switch to a larger model for complex reasoning and multi-file changes, then switch back to the small one for running tests and fixing lint errors.", Citations: []dmessage.Citation{{URL: "https://pi.dev/blog/model-switching", Title: "Model switching workflows with Pi", CitedText: "Start with a small model for quick lookups"}}},
		{Type: dmessage.BlockText, Text: "\n\nOne nice bonus tied to switching: Pi keeps "},
		{Type: dmessage.BlockText, Text: "tree-structured sessions — every branch preserved; rewind 10 messages, try something else, never lose work", Citations: []dmessage.Citation{{URL: "https://github.com/earendil-works/pi", Title: "earendil-works/pi: a tiny coding agent", CitedText: "tree-structured sessions — every branch preserved"}}},
		{Type: dmessage.BlockText, Text: ", so model switching pairs well with rewinding to retry a step with a different model."},
	}
	message.Usage = nativeUsage(inputTokens, 80, 0.003)
	return dmodel.Response{Message: message}
}

func nativeToolSmorgasbord(inputTokens int) dmodel.Response {
	message := dmessage.Assistant("Here's a sample of all the tools:")
	message.ID = fmt.Sprintf("pred-smorgasbord-%d", time.Now().UnixNano())
	message.Content = append(message.Content, dmessage.ContentBlock{Type: dmessage.BlockReasoning, Reasoning: "I'm thinking about the best approach for this task."})
	calls := []struct {
		name string
		args any
	}{
		{"bash", map[string]any{"command": "echo 'hello from bash'"}},
		{"patch", map[string]any{"path": "/tmp/example.txt", "patches": []map[string]string{{"operation": "replace", "oldText": "foo", "newText": "bar"}}}},
		{"browser", map[string]any{"action": "screenshot"}},
		{"keyword_search", map[string]any{"query": "find all references", "search_terms": []string{"reference", "example"}}},
		{"browser", map[string]any{"action": "navigate", "url": "https://example.com"}},
		{"browser", map[string]any{"action": "eval", "expression": "document.title"}},
		{"read_image", map[string]any{"path": "/tmp/image.png"}},
		{"browser", map[string]any{"action": "console_logs"}},
		{"browser", map[string]any{"action": "emulate_device", "device": "iphone_14"}},
		{"browser", map[string]any{"action": "network_enable"}},
		{"browser", map[string]any{"action": "accessibility_tree"}},
		{"browser", map[string]any{"action": "profile_metrics"}},
		{"browser_emulate", map[string]any{"action": "device", "device": "ipad"}},
		{"browser_network", map[string]any{"action": "enable"}},
		{"browser_accessibility", map[string]any{"action": "tree"}},
		{"browser_profile", map[string]any{"action": "metrics"}},
		{"llm_one_shot", map[string]any{"prompt_files": []string{"/tmp/test-prompt.txt", "/tmp/test-image.png"}}},
		{"browser", map[string]any{"action": "screencast_stop"}},
		{"shell", map[string]any{"command": "echo 'hello from shell'"}},
	}
	for index, call := range calls {
		arguments, err := json.Marshal(call.args)
		if err != nil {
			panic(err)
		}
		message.ToolCalls = append(message.ToolCalls, dmessage.ToolCall{ID: fmt.Sprintf("tool_%d_%d", index, time.Now().UnixNano()%100000), Name: call.name, Arguments: arguments})
	}
	message.Usage = nativeUsage(inputTokens, 200, 0.01)
	return dmodel.Response{Message: message}
}

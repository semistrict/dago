package dago

import (
	"os"
	"strings"

	"github.com/semistrict/dago/agent"
)

const anthropicUniversalPrompt = `<use_parallel_tool_calls>
If you intend to call multiple tools and there are no dependencies between the tool calls, make all of the independent tool calls in parallel. Prioritize calling tools simultaneously whenever the actions can be done in parallel rather than sequentially. For example, when reading 3 files, run 3 tool calls in parallel to read all 3 files into context at the same time. Maximize use of parallel tool calls where possible to increase speed and efficiency. However, if some tool calls depend on previous calls to inform dependent values like the parameters, do NOT call these tools in parallel and instead call them sequentially. Never use placeholders or guess missing parameters in tool calls.
</use_parallel_tool_calls>

<investigate_before_answering>
Never speculate about code you have not opened. If the user references a specific file, you MUST read the file before answering. Make sure to investigate and read relevant files BEFORE answering questions about the codebase. Never make any claims about code before investigating unless you are certain of the correct answer - give grounded and hallucination-free answers.
</investigate_before_answering>

<tool_result_reflection>
After receiving tool results, carefully reflect on their quality and determine optimal next steps before proceeding. Use your thinking to plan and iterate based on this new information, and then take the best next action.
</tool_result_reflection>`

const anthropicOpusPrompt = anthropicUniversalPrompt + `

<tool_usage>
When a task depends on the state of files, tests, or system output, use tools to observe that state directly rather than reasoning from memory about what it probably contains. Read files before describing them. Run tests before claiming they pass. Search the codebase before asserting a symbol does or does not exist. Active investigation with tools is the default mode of working, not a fallback.
</tool_usage>

<subagent_usage>
Do not spawn a subagent for work you can complete directly in a single response (e.g. refactoring a function you can already see).

Spawn multiple subagents in the same turn when fanning out across items or reading multiple files.
</subagent_usage>`

const engineeringAgentPrompt = `## Engineering-Agent Behavior

- Act as an autonomous senior engineer. Once given a direction, proactively gather context, plan, implement, and verify without waiting for another prompt at each step.
- Persist until the task is handled end-to-end in the current turn whenever feasible. Carry work through implementation, verification, and a clear explanation of outcomes.
- Bias toward action with reasonable assumptions. Ask for clarification only when genuinely blocked.
- Begin the work directly without an upfront status preamble.

## Parallel Tool Use

- Before calling tools, identify the files and resources the task is likely to require.
- Batch independent reads, searches, and other independent operations.
- Use sequential calls only when a later action genuinely depends on an earlier result.

## Plan Hygiene

- Before finishing, reconcile every item created through write_todos. Mark each done, blocked with a concise reason, or cancelled; do not finish with pending items.`

const (
	openRouterAppURL   = "https://github.com/langchain-ai/deepagents"
	openRouterAppTitle = "Deep Agents"
)

func init() {
	registerBuiltinHarnessProfile("anthropic:claude-haiku-4-5", anthropicUniversalPrompt)
	registerBuiltinHarnessProfile("anthropic:claude-sonnet-4-6", anthropicUniversalPrompt)
	registerBuiltinHarnessProfile("anthropic:claude-opus-4-7", anthropicOpusPrompt)

	mustRegisterProviderProfile("openai", ProviderProfile{Options: map[string]any{"use_responses_api": true}})
	mustRegisterProviderProfile("nvidia", ProviderProfile{OptionsFactory: func() (map[string]any, error) {
		return map[string]any{"default_headers": map[string]string{"X-BILLING-INVOKE-ORIGIN": "DeepAgents"}}, nil
	}})
	mustRegisterProviderProfile("openrouter", ProviderProfile{OptionsFactory: openRouterDefaults})
}

func registerBuiltinHarnessProfile(name, suffix string) {
	if err := RegisterProfile(Profile{Name: name, Kind: ProfileHarness, SystemPromptSuffix: &suffix}); err != nil {
		panic(err)
	}
}

func builtinEngineeringHarnessProfile(provider, model string) (Profile, bool) {
	if provider != "openai" {
		return Profile{}, false
	}
	family := "co" + "dex"
	matched := false
	for _, version := range []string{"5.1", "5.2", "5.3"} {
		if model == "gpt-"+version+"-"+family {
			matched = true
			break
		}
	}
	if !matched {
		return Profile{}, false
	}
	suffix := engineeringAgentPrompt
	return Profile{
		Kind: ProfileHarness, SystemPromptSuffix: &suffix,
		Middleware: []agent.Middleware{agent.TodoList()},
	}, true
}

func mustRegisterProviderProfile(name string, profile ProviderProfile) {
	if err := RegisterProviderProfile(name, profile); err != nil {
		panic(err)
	}
}

func openRouterDefaults() (map[string]any, error) {
	result := map[string]any{}
	if _, exists := os.LookupEnv("OPENROUTER_APP_URL"); !exists {
		result["app_url"] = openRouterAppURL
	}
	if _, exists := os.LookupEnv("OPENROUTER_APP_TITLE"); !exists {
		result["app_title"] = openRouterAppTitle
	}
	if !environmentTruthy("DEEPAGENTS_OPENROUTER_ALLOW_AZURE") {
		result["openrouter_provider"] = map[string]any{"ignore": []string{"azure"}}
	}
	return result, nil
}

func environmentTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

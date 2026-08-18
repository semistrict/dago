package dacode

import (
	"sort"
	"strings"
)

type commandBypassTier string

const (
	commandAlways         commandBypassTier = "always"
	commandConnecting     commandBypassTier = "connecting"
	commandImmediateUI    commandBypassTier = "immediate_ui"
	commandSideEffectFree commandBypassTier = "side_effect_free"
	commandQueued         commandBypassTier = "queued"
)

type slashCommandDefinition struct {
	Name           string
	Description    string
	Tier           commandBypassTier
	HiddenKeywords string
	ArgumentHint   string
	Aliases        []string
}

var slashCommandDefinitions = []slashCommandDefinition{
	{Name: "/agents", Description: "Browse and switch between available agents", Tier: commandImmediateUI, HiddenKeywords: "switch profile persona"},
	{Name: "/auto", Description: "Switch to Auto approval mode or manage its classifier model", Tier: commandImmediateUI, HiddenKeywords: "approval mode classifier automatic shift tab model", ArgumentHint: "[model [<spec>|clear]]"},
	{Name: "/manual", Description: "Switch to Manual approval mode", Tier: commandSideEffectFree, HiddenKeywords: "approval mode prompt review shift tab"},
	{Name: "/yolo", Description: "Switch to unrestricted approval mode", Tier: commandSideEffectFree, HiddenKeywords: "approval mode unrestricted dangerous shift tab"},
	{Name: "/auth", Description: "Connect and manage provider and service credentials", Tier: commandImmediateUI, HiddenKeywords: "key credential login token api tracing langsmith", Aliases: []string{"/connect"}},
	{Name: "/clear", Description: "Clear the chat and start a new thread", Tier: commandQueued, HiddenKeywords: "reset"},
	{Name: "/copy", Description: "Copy the latest assistant message to clipboard", Tier: commandQueued},
	{Name: "/context", Description: "Show current context window usage", Tier: commandQueued, HiddenKeywords: "tokens window usage remaining offload compact"},
	{Name: "/cost", Description: "Show estimated thread cost", Tier: commandQueued, HiddenKeywords: "price spend usage tokens dollars usd"},
	{Name: "/force-clear", Description: "Stop active work, clear the chat, and start a new thread", Tier: commandAlways, HiddenKeywords: "reset interrupt"},
	{Name: "/goal", Description: "Set and manage a persistent objective with acceptance criteria", Tier: commandQueued, HiddenKeywords: "objective criteria acceptance amend pause resume rubric grader model iterations", ArgumentHint: "[<objective>|amend <feedback>|pause|resume|show|clear|model|max-iterations]"},
	{Name: "/workflow", Description: "Launch a deterministic background workflow", Tier: commandQueued, HiddenKeywords: "orchestration background script phases agents", ArgumentHint: "<saved-name-or-script-path>"},
	{Name: "/workflows", Description: "View and manage background workflow runs", Tier: commandImmediateUI, HiddenKeywords: "orchestration background runs cancel status"},
	{Name: "/editor", Description: "Open the prompt in an external editor", Tier: commandQueued},
	{Name: "/effort", Description: "Set reasoning effort for the current model", Tier: commandQueued, HiddenKeywords: "reasoning thinking level", ArgumentHint: "[<level>|clear]"},
	{Name: "/mcp", Description: "Manage MCP servers and authentication", Tier: commandSideEffectFree, HiddenKeywords: "servers oauth authenticate reconnect disable enable", ArgumentHint: "[login <server> | reconnect]"},
	{Name: "/plugins", Description: "Manage plugins", Tier: commandImmediateUI, HiddenKeywords: "plugin marketplace skills mcp enable disable install"},
	{Name: "/model", Description: "Switch models or edit model settings", Tier: commandImmediateUI},
	{Name: "/notifications", Description: "Configure warning notifications", Tier: commandImmediateUI, HiddenKeywords: "warnings alerts suppress startup yolo"},
	{Name: "/offload", Description: "Summarize and offload older messages to free context", Tier: commandQueued, HiddenKeywords: "compact", Aliases: []string{"/compact"}},
	{Name: "/remember", Description: "Save useful context to memory or skills", Tier: commandQueued, ArgumentHint: "[context]"},
	{Name: "/reload", Description: "Reload environment and config", Tier: commandQueued, HiddenKeywords: "refresh plugin plugins marketplace"},
	{Name: "/skill-creator", Description: "Create or refine agent skills", Tier: commandQueued, ArgumentHint: "[task]"},
	{Name: "/threads", Description: "Browse and resume past threads", Tier: commandImmediateUI, HiddenKeywords: "continue history sessions resume back previous", ArgumentHint: "[-r [ID]]"},
	{Name: "/trace", Description: "Open this thread in LangSmith", Tier: commandSideEffectFree},
	{Name: "/tokens", Description: "Show token usage", Tier: commandQueued, HiddenKeywords: "cost"},
	{Name: "/tools", Description: "List the tools available to the agent", Tier: commandQueued, HiddenKeywords: "mcp functions capabilities builtin"},
	{Name: "/rubric", Description: "Set explicit acceptance criteria for rubric grading", Tier: commandImmediateUI, HiddenKeywords: "criteria acceptance grader grading evaluation iterations", ArgumentHint: "[set|next|file|show|clear|model|max-iterations]", Aliases: []string{"/criteria"}},
	{Name: "/restart", Description: "Restart the agent server", Tier: commandAlways, HiddenKeywords: "respawn server reconnect connect"},
	{Name: "/theme", Description: "Change color theme", Tier: commandImmediateUI, HiddenKeywords: "dark light color appearance"},
	{Name: "/scrollbar", Description: "Show or hide the chat scrollbar", Tier: commandSideEffectFree, HiddenKeywords: "scroll scroller bar vertical"},
	{Name: "/timestamps", Description: "Show or hide message timestamps", Tier: commandSideEffectFree, HiddenKeywords: "time footer date"},
	{Name: "/line-numbers", Description: "Show or hide line numbers in file diffs", Tier: commandSideEffectFree, HiddenKeywords: "diff gutter numbers lines"},
	{Name: "/update", Description: "Check and install a signed update", Tier: commandQueued, HiddenKeywords: "upgrade signed release refresh"},
	{Name: "/install", Description: "Install an optional integration", Tier: commandQueued, HiddenKeywords: "extra extras add provider sandbox dependency", ArgumentHint: "<extra> [--force]"},
	{Name: "/auto-update", Description: "Turn automatic updates on or off", Tier: commandSideEffectFree},
	{Name: "/changelog", Description: "Open the changelog in a browser", Tier: commandSideEffectFree},
	{Name: "/version", Description: "Show version information", Tier: commandConnecting, Aliases: []string{"/about"}},
	{Name: "/feedback", Description: "Send feedback or report an issue", Tier: commandSideEffectFree},
	{Name: "/docs", Description: "Open the docs", Tier: commandSideEffectFree},
	{Name: "/help", Description: "Show help and available commands", Tier: commandQueued},
	{Name: "/quit", Description: "Exit app", Tier: commandAlways, HiddenKeywords: "close leave", Aliases: []string{"/q"}},
}

var (
	hiddenSlashCommands         = map[string]bool{"/debug": true, "/debug-error": true}
	immediateUIArgumentForms    = map[string]bool{"/auto model": true}
	startupRecoverySlashCommand = map[string]bool{"/install": true, "/reload": true, "/update": true}
	staticSkillCommandAliases   = map[string]bool{"remember": true, "skill-creator": true}
	slashCommandsByName         = buildSlashCommandIndex(slashCommandDefinitions)
)

func buildSlashCommandIndex(definitions []slashCommandDefinition) map[string]slashCommandDefinition {
	result := make(map[string]slashCommandDefinition, len(definitions)*2)
	for _, definition := range definitions {
		if !validSlashCommandName(definition.Name) || definition.Description == "" || !validCommandTier(definition.Tier) {
			panic("dacode: slash command definition is invalid")
		}
		definition.Aliases = append([]string(nil), definition.Aliases...)
		for _, name := range append([]string{definition.Name}, definition.Aliases...) {
			if !validSlashCommandName(name) {
				panic("dacode: slash command alias is invalid")
			}
			if _, exists := result[name]; exists {
				panic("dacode: slash command name is duplicated")
			}
			result[name] = definition
		}
	}
	for hidden := range hiddenSlashCommands {
		if _, exists := result[hidden]; exists || !validSlashCommandName(hidden) {
			panic("dacode: hidden slash command conflicts with the public registry")
		}
	}
	for recovery := range startupRecoverySlashCommand {
		definition, exists := result[recovery]
		if !exists || definition.Tier != commandQueued {
			panic("dacode: startup recovery command must be queue-bound")
		}
	}
	return result
}

func slashCommandDefinitionFor(value string) (slashCommandDefinition, bool) {
	command := firstSlashCommand(value)
	definition, exists := slashCommandsByName[command]
	definition.Aliases = append([]string(nil), definition.Aliases...)
	return definition, exists
}

func firstSlashCommand(value string) string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(value)))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// canonicalSlashInput resolves a public alias while preserving the original
// argument text. Hidden and dynamic skill commands intentionally bypass it.
func canonicalSlashInput(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	command := firstSlashCommand(trimmed)
	definition, exists := slashCommandsByName[command]
	if !exists {
		return trimmed, false
	}
	remainder := strings.TrimSpace(trimmed[len(command):])
	canonical := definition.Name
	if remainder != "" {
		canonical += " " + remainder
	}
	return canonical, true
}

type commandQueueState struct {
	Connecting    bool
	AgentRunning  bool
	ShellRunning  bool
	ModalRunning  bool
	StartupFailed bool
	Switching     bool
}

func canBypassCommandQueue(value string, state commandQueueState) bool {
	canonical := strings.ToLower(strings.TrimSpace(value))
	command := firstSlashCommand(canonical)
	if hiddenSlashCommands[command] {
		return canonical == command
	}
	definition, exists := slashCommandsByName[command]
	if !exists {
		return false
	}
	if definition.Tier == commandAlways {
		// Urgent commands bypass before the ordinary queue/switch guards, but
		// only in their registered bare form. Treating arbitrary argument
		// forms as urgent would let a future mutating subcommand race active
		// work merely because its parent command is urgent.
		return canonical == command
	}
	if state.Switching {
		return false
	}
	active := state.AgentRunning || state.ShellRunning || state.ModalRunning
	if startupRecoverySlashCommand[command] && state.StartupFailed && !active {
		if command == "/install" {
			return installStartupRecoveryBypassCapable(strings.TrimSpace(strings.TrimPrefix(canonical, command)))
		}
		return true
	}
	switch definition.Tier {
	case commandConnecting:
		return state.Connecting && !active
	case commandImmediateUI:
		return canonical == command || immediateUIArgumentForms[strings.Join(strings.Fields(canonical), " ")]
	case commandSideEffectFree:
		return true
	default:
		return false
	}
}

func publicSlashCommandDefinitions() []slashCommandDefinition {
	result := make([]slashCommandDefinition, len(slashCommandDefinitions))
	for index, definition := range slashCommandDefinitions {
		definition.Aliases = append([]string(nil), definition.Aliases...)
		result[index] = definition
	}
	return result
}

func classifiedSlashCommandNames() []string {
	result := make([]string, 0, len(slashCommandsByName)+len(hiddenSlashCommands))
	for name := range slashCommandsByName {
		result = append(result, name)
	}
	for name := range hiddenSlashCommands {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func validSlashCommandName(value string) bool {
	if len(value) < 2 || len(value) > 64 || value[0] != '/' {
		return false
	}
	for _, character := range value[1:] {
		if (character < 'a' || character > 'z') && character != '-' {
			return false
		}
	}
	return true
}

func validCommandTier(tier commandBypassTier) bool {
	switch tier {
	case commandAlways, commandConnecting, commandImmediateUI, commandSideEffectFree, commandQueued:
		return true
	default:
		return false
	}
}

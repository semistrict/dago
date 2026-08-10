package dago

import (
	"fmt"
	"strings"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	"github.com/semistrict/dago/cache"
	"github.com/semistrict/dago/checkpoint"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/state"
	"github.com/semistrict/dago/store"
	"github.com/semistrict/dago/tool"
)

// Options configures the complete local deep-agent stack.
type Options struct {
	Name                 string
	ProfileNames         []string
	Profiles             []Profile
	Model                model.Chat
	Tools                []tool.Tool
	SystemPrompt         string
	Middleware           []agent.Middleware
	Backend              backend.Backend
	FilesystemTools      []string
	Permissions          []FilesystemPermission
	Subagents            []Subagent
	DisableSubagents     bool
	Skills               []string
	Memory               []string
	EnableTodo           bool
	DisableTodo          bool
	DisableSummary       bool
	Summarization        SummarizationOptions
	InterruptOn          []agent.ApprovalRule
	PromptCacheRetention string
	StructuredOutput     *agent.StructuredOutput
	StateFields          map[string]agent.StateField
	Saver                checkpoint.Saver
	Store                store.Store
	Cache                cache.Cache
	Context              any
	RecursionLimit       int
	MaxConcurrency       int
	FailOnToolError      bool
}

// DeepAgent is a compiled agent with the standard filesystem, subagent,
// compaction, skill, memory, and optional planning layers selected by Options.
type DeepAgent struct{ *agent.Agent }

// New constructs a deep agent. A model is always explicit; no provider default or
// hidden credential lookup occurs in the core package.
func New(options Options) (*DeepAgent, error) {
	if options.Model == nil {
		return nil, fmt.Errorf("create deep agent: model is required")
	}
	if options.Backend == nil {
		memory, err := backend.NewState("", nil)
		if err != nil {
			return nil, err
		}
		options.Backend = memory
	}
	profile, err := resolveProfiles(options.Model, options.ProfileNames, options.Profiles)
	if err != nil {
		return nil, err
	}
	options.SystemPrompt = applyProfilePrompt(profile, options.SystemPrompt, "")
	options.Tools = applyToolProfile(options.Tools, profile.ToolDescriptions, stringSet(profile.ExcludeTools))
	filesystem, err := FilesystemMiddleware(FilesystemOptions{
		Backend: options.Backend, Permissions: options.Permissions, Tools: options.FilesystemTools,
	})
	if err != nil {
		return nil, err
	}
	filesystem.Tools = applyToolProfile(filesystem.Tools, profile.ToolDescriptions, stringSet(profile.ExcludeTools))
	core := []agent.Middleware{}
	if options.EnableTodo && !options.DisableTodo {
		core = append(core, agent.TodoList())
	}
	if len(options.Skills) > 0 {
		middleware, err := SkillsMiddleware(SkillsOptions{Backend: options.Backend, Sources: options.Skills})
		if err != nil {
			return nil, err
		}
		core = append(core, middleware)
	}
	core = append(core, filesystem)

	if !options.DisableSubagents {
		subagents := append([]Subagent(nil), options.Subagents...)
		generalEnabled := profile.GeneralPurpose == nil || profile.GeneralPurpose.Enabled == nil || *profile.GeneralPurpose.Enabled
		if generalEnabled && !hasSubagent(subagents, "general-purpose") {
			general, err := buildGeneralSubagent(options, filesystem, profile)
			if err != nil {
				return nil, err
			}
			description := defaultGeneralSubagentDescription
			if profile.GeneralPurpose != nil && profile.GeneralPurpose.Description != nil {
				description = *profile.GeneralPurpose.Description
			}
			subagents = append([]Subagent{{
				Name: "general-purpose", Description: description, Runnable: general,
				InheritedState: []string{"files"},
			}}, subagents...)
		}
		if len(subagents) > 0 {
			middleware, err := SubagentMiddleware(subagents)
			if err != nil {
				return nil, err
			}
			core = append(core, middleware)
		}
	}
	if !options.DisableSummary {
		summary := options.Summarization
		if summary.Model == nil {
			summary.Model = options.Model
		}
		if summary.Backend == nil {
			summary.Backend = options.Backend
		}
		middleware, err := SummarizationMiddleware(summary)
		if err != nil {
			return nil, err
		}
		core = append(core, middleware)
	}
	core = append(core, PatchToolCallsMiddleware())
	tail := append([]agent.Middleware(nil), profile.Middleware...)
	tail = append(tail, agent.PromptCaching("prompt_caching", options.PromptCacheRetention, func(request agent.ModelRequest) string {
		if request.Runtime.Config.ThreadID != "" {
			return request.Runtime.Config.ThreadID
		}
		return request.Runtime.TaskID
	}))
	if len(options.Memory) > 0 {
		middleware, err := MemoryMiddleware(MemoryOptions{Backend: options.Backend, Sources: options.Memory})
		if err != nil {
			return nil, err
		}
		tail = append(tail, middleware)
	}
	if len(options.InterruptOn) > 0 {
		tail = append(tail, agent.HumanApproval(options.InterruptOn))
	}
	middleware := mergeMiddleware(core, tail, options.Middleware)
	excludedMiddleware := map[string]bool{}
	for _, name := range profile.ExcludeMiddleware {
		if name == "filesystem" || name == "subagents" {
			return nil, fmt.Errorf("profile cannot exclude required middleware %q", name)
		}
		excludedMiddleware[name] = true
	}
	filteredMiddleware := make([]agent.Middleware, 0, len(middleware))
	excludedTools := map[string]bool{}
	for _, name := range profile.ExcludeTools {
		excludedTools[name] = true
	}
	for _, item := range middleware {
		if excludedMiddleware[item.Name] {
			delete(excludedMiddleware, item.Name)
			continue
		}
		item.Tools = applyToolProfile(item.Tools, profile.ToolDescriptions, excludedTools)
		filteredMiddleware = append(filteredMiddleware, item)
	}
	if len(excludedMiddleware) > 0 {
		for name := range excludedMiddleware {
			return nil, fmt.Errorf("profile middleware exclusion %q matched no assembled middleware", name)
		}
	}
	middleware = filteredMiddleware
	if len(profile.ExcludeTools) > 0 {
		middleware = append(middleware, ToolExclusionMiddleware(profile.ExcludeTools))
	}
	compiled, err := agent.New(agent.Options{
		Name: options.Name, Model: options.Model, Tools: options.Tools, SystemPrompt: options.SystemPrompt,
		Middleware: middleware, StateFields: options.StateFields, StructuredOutput: options.StructuredOutput,
		Saver: options.Saver, Store: options.Store, Cache: options.Cache, Context: options.Context,
		RecursionLimit: options.RecursionLimit, MaxConcurrency: options.MaxConcurrency,
		FailOnToolError: options.FailOnToolError,
	})
	if err != nil {
		return nil, err
	}
	return &DeepAgent{Agent: compiled}, nil
}

const defaultGeneralSubagentDescription = "General-purpose agent for researching complex questions, searching for files and content, and executing multi-step tasks. Use it when a search may require several attempts. It has the same capabilities as the main agent."

const defaultGeneralSubagentPrompt = "In order to complete the objective that the user asks of you, you have access to a number of standard tools.\n\nThe calling agent only sees your final assistant message, not your intermediate work, tool results, or status tracking. Ensure your final response contains the complete answer."

func buildGeneralSubagent(options Options, filesystem agent.Middleware, profile Profile) (*agent.Agent, error) {
	middleware := []agent.Middleware{}
	if options.EnableTodo && !options.DisableTodo {
		middleware = append(middleware, agent.TodoList())
	}
	if len(options.Skills) > 0 {
		skills, err := SkillsMiddleware(SkillsOptions{Backend: options.Backend, Sources: options.Skills})
		if err != nil {
			return nil, err
		}
		middleware = append(middleware, skills)
	}
	middleware = append(middleware, filesystem)
	if !options.DisableSummary {
		summary := options.Summarization
		if summary.Model == nil {
			summary.Model = options.Model
		}
		if summary.Backend == nil {
			summary.Backend = options.Backend
		}
		compact, err := SummarizationMiddleware(summary)
		if err != nil {
			return nil, err
		}
		middleware = append(middleware, compact)
	}
	middleware = append(middleware, PatchToolCallsMiddleware())
	middleware = append(middleware, profile.Middleware...)
	middleware = append(middleware, agent.PromptCaching("prompt_caching", options.PromptCacheRetention, func(request agent.ModelRequest) string {
		if request.Runtime.Config.ThreadID != "" {
			return request.Runtime.Config.ThreadID
		}
		return request.Runtime.TaskID
	}))
	if len(options.InterruptOn) > 0 {
		middleware = append(middleware, agent.HumanApproval(options.InterruptOn))
	}
	for _, custom := range options.Middleware {
		for index, current := range middleware {
			if current.Name == custom.Name {
				middleware[index] = custom
				break
			}
		}
	}
	filtered, err := filterProfileMiddleware(middleware, profile, false)
	if err != nil {
		return nil, err
	}
	middleware = filtered
	if len(profile.ExcludeTools) > 0 {
		middleware = append(middleware, ToolExclusionMiddleware(profile.ExcludeTools))
	}
	prompt := applyProfilePrompt(profile, "", defaultGeneralSubagentPrompt)
	if profile.GeneralPurpose != nil && profile.GeneralPurpose.SystemPrompt != nil {
		prompt = strings.TrimSpace(*profile.GeneralPurpose.SystemPrompt)
		if profile.SystemPrompt != "" {
			prompt += "\n\n" + strings.TrimSpace(profile.SystemPrompt)
		}
		if profile.SystemPromptSuffix != nil && strings.TrimSpace(*profile.SystemPromptSuffix) != "" {
			prompt += "\n\n" + strings.TrimSpace(*profile.SystemPromptSuffix)
		}
	}
	return agent.New(agent.Options{
		Name: "general-purpose", Model: options.Model, Tools: options.Tools,
		SystemPrompt: prompt, Middleware: middleware, StateFields: options.StateFields,
		StructuredOutput: options.StructuredOutput,
		Store:            options.Store, Cache: options.Cache, Context: options.Context,
		RecursionLimit: options.RecursionLimit, MaxConcurrency: options.MaxConcurrency,
		FailOnToolError: options.FailOnToolError,
	})
}

// Custom middleware replaces an existing entry with the same name in place.
// Brand-new middleware is inserted after required core scaffolding and before
// the profile, prompt-caching, memory, and approval tail.
func mergeMiddleware(core, tail, custom []agent.Middleware) []agent.Middleware {
	result := append(append([]agent.Middleware(nil), core...), tail...)
	positions := map[string]int{}
	for index, item := range result {
		positions[item.Name] = index
	}
	insertAt := len(core)
	var additions []agent.Middleware
	for _, item := range custom {
		if index, exists := positions[item.Name]; exists {
			result[index] = item
		} else {
			additions = append(additions, item)
		}
	}
	if len(additions) > 0 {
		result = append(result, make([]agent.Middleware, len(additions))...)
		copy(result[insertAt+len(additions):], result[insertAt:len(result)-len(additions)])
		copy(result[insertAt:], additions)
	}
	return result
}

func hasSubagent(values []Subagent, name string) bool {
	for _, value := range values {
		if value.Name == name {
			return true
		}
	}
	return false
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func filterProfileMiddleware(values []agent.Middleware, profile Profile, verify bool) ([]agent.Middleware, error) {
	excluded := stringSet(profile.ExcludeMiddleware)
	for name := range excluded {
		if name == "filesystem" || name == "subagents" {
			return nil, fmt.Errorf("profile cannot exclude required middleware %q", name)
		}
	}
	result := make([]agent.Middleware, 0, len(values))
	for _, value := range values {
		if excluded[value.Name] {
			delete(excluded, value.Name)
			continue
		}
		value.Tools = applyToolProfile(value.Tools, profile.ToolDescriptions, stringSet(profile.ExcludeTools))
		result = append(result, value)
	}
	if verify && len(excluded) > 0 {
		for name := range excluded {
			return nil, fmt.Errorf("profile middleware exclusion %q matched no assembled middleware", name)
		}
	}
	return result, nil
}

// State is an alias retained at the public boundary for custom middleware options.
type State = state.Values

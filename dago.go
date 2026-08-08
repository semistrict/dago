package dago

import (
	"fmt"

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
	Name             string
	ProfileNames     []string
	Profiles         []Profile
	Model            model.Chat
	Tools            []tool.Tool
	SystemPrompt     string
	Middleware       []agent.Middleware
	Backend          backend.Backend
	Permissions      []FilesystemPermission
	Subagents        []Subagent
	DisableSubagents bool
	Skills           []string
	Memory           []string
	DisableTodo      bool
	DisableSummary   bool
	Summarization    SummarizationOptions
	StructuredOutput *agent.StructuredOutput
	StateFields      map[string]agent.StateField
	Saver            checkpoint.Saver
	Store            store.Store
	Cache            cache.Cache
	Context          any
	RecursionLimit   int
	MaxConcurrency   int
	FailOnToolError  bool
}

// DeepAgent is a compiled agent with the standard planning, filesystem,
// subagent, compaction, skill, and memory layers selected by Options.
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
	profile, err := resolveProfiles(options.ProfileNames, options.Profiles)
	if err != nil {
		return nil, err
	}
	if profile.SystemPrompt != "" {
		if options.SystemPrompt != "" {
			options.SystemPrompt += "\n\n"
		}
		options.SystemPrompt += profile.SystemPrompt
	}
	options.Middleware = append(profile.Middleware, options.Middleware...)
	filesystem, err := FilesystemMiddleware(FilesystemOptions{Backend: options.Backend, Permissions: options.Permissions})
	if err != nil {
		return nil, err
	}
	core := []agent.Middleware{}
	if !options.DisableTodo {
		core = append(core, agent.TodoList())
	}
	core = append(core, filesystem)

	if !options.DisableSubagents {
		subagents := options.Subagents
		if subagents == nil {
			general, err := buildGeneralSubagent(options, filesystem)
			if err != nil {
				return nil, err
			}
			subagents = []Subagent{{
				Name: "general-purpose", Description: "General-purpose agent for complex, context-heavy multi-step tasks.", Runnable: general,
				InheritedState: []string{"files"},
			}}
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
	if len(options.Skills) > 0 {
		middleware, err := SkillsMiddleware(SkillsOptions{Backend: options.Backend, Sources: options.Skills})
		if err != nil {
			return nil, err
		}
		core = append(core, middleware)
	}
	if len(options.Memory) > 0 {
		middleware, err := MemoryMiddleware(MemoryOptions{Backend: options.Backend, Sources: options.Memory})
		if err != nil {
			return nil, err
		}
		core = append(core, middleware)
	}
	middleware := mergeMiddleware(core, options.Middleware)
	excludedMiddleware := map[string]bool{}
	for _, name := range profile.ExcludeMiddleware {
		if name == "filesystem" {
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
			continue
		}
		item.Tools = applyToolProfile(item.Tools, profile.ToolDescriptions, excludedTools)
		filteredMiddleware = append(filteredMiddleware, item)
	}
	middleware = filteredMiddleware
	options.Tools = applyToolProfile(options.Tools, profile.ToolDescriptions, excludedTools)
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

func buildGeneralSubagent(options Options, filesystem agent.Middleware) (*agent.Agent, error) {
	middleware := []agent.Middleware{}
	if !options.DisableTodo {
		middleware = append(middleware, agent.TodoList())
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
	return agent.New(agent.Options{
		Name: "general-purpose", Model: options.Model, Tools: options.Tools,
		SystemPrompt: options.SystemPrompt, Middleware: middleware,
		Store: options.Store, Cache: options.Cache, Context: options.Context,
		RecursionLimit: options.RecursionLimit, MaxConcurrency: options.MaxConcurrency,
		FailOnToolError: options.FailOnToolError,
	})
}

// Custom middleware replaces an existing entry with the same name in place.
// Brand-new middleware is inserted after required core scaffolding and before
// optional skill/memory tail entries.
func mergeMiddleware(base, custom []agent.Middleware) []agent.Middleware {
	result := append([]agent.Middleware(nil), base...)
	positions := map[string]int{}
	for index, item := range result {
		positions[item.Name] = index
	}
	insertAt := len(result)
	for index, item := range result {
		if item.Name == "skills" || item.Name == "memory" {
			insertAt = index
			break
		}
	}
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

// State is an alias retained at the public boundary for custom middleware options.
type State = state.Values

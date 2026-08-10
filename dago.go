package dago

import (
	"fmt"
	"sort"
	"strings"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	"github.com/semistrict/dago/cache"
	"github.com/semistrict/dago/checkpoint"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/state"
	"github.com/semistrict/dago/store"
	"github.com/semistrict/dago/tool"
)

// Options configures the complete local deep-agent stack.
type Options struct {
	Name                       string
	ProfileNames               []string
	Profiles                   []Profile
	Model                      model.Chat
	Tools                      []tool.Tool
	SystemPrompt               string
	SystemMessage              *message.Message
	Middleware                 []agent.Middleware
	Backend                    backend.Backend
	FilesystemTools            []string
	FilesystemToolDescriptions map[string]string
	MaxExecuteTimeout          int
	Permissions                []FilesystemPermission
	VideoExtractor             VideoExtractor
	MaxVideoBytes              int
	VideoSamplingRate          float64
	Subagents                  []Subagent
	AsyncSubagents             []AsyncSubagent
	AsyncSubagentPrompt        string
	DisableSubagents           bool
	Skills                     []string
	SkillCatalog               []Skill
	SkillActivation            func(Skill) string
	Memory                     []string
	MemoryContents             map[string]string
	MemorySystemPrompt         *string
	EnableTodo                 bool
	DisableTodo                bool
	DisableSummary             bool
	Summarization              SummarizationOptions
	InterruptOn                []agent.ApprovalRule
	PromptCacheRetention       string
	StructuredOutput           *agent.StructuredOutput
	StateFields                map[string]agent.StateField
	Saver                      checkpoint.Saver
	Store                      store.Store
	Cache                      cache.Cache
	Context                    any
	RecursionLimit             int
	MaxConcurrency             int
	FailOnToolError            bool
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
	if options.SystemPrompt != "" && options.SystemMessage != nil {
		return nil, fmt.Errorf("create deep agent: system prompt and system message are mutually exclusive")
	}
	if options.SystemMessage != nil && options.SystemMessage.Role != message.RoleSystem {
		return nil, fmt.Errorf("create deep agent: system message role must be system")
	}
	if options.Saver == nil && optionsNeedSaver(options) {
		return nil, fmt.Errorf("human approval requires a checkpointer")
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
	inheritedTools := append([]tool.Tool(nil), options.Tools...)
	if options.SystemMessage != nil {
		copy := options.SystemMessage.Clone()
		profilePrompt := applyProfilePrompt(profile, "", "")
		if profilePrompt != "" {
			copy.Content = append(copy.Content, message.ContentBlock{Type: message.BlockText, Text: "\n\n" + profilePrompt})
		}
		options.SystemMessage = &copy
	} else {
		options.SystemPrompt = applyProfilePrompt(profile, options.SystemPrompt, "")
	}
	options.Tools = applyToolProfile(options.Tools, profile.ToolDescriptions, nil)
	filesystem, err := FilesystemMiddleware(FilesystemOptions{
		Backend: options.Backend, Permissions: options.Permissions, Tools: options.FilesystemTools,
		ApprovalOverrides: options.InterruptOn, ToolDescriptions: options.FilesystemToolDescriptions,
		MaxExecuteTimeout: options.MaxExecuteTimeout, VideoExtractor: options.VideoExtractor,
		MaxVideoBytes: options.MaxVideoBytes, VideoSamplingRate: options.VideoSamplingRate,
	})
	if err != nil {
		return nil, err
	}
	filesystem.Tools = applyToolProfile(filesystem.Tools, profile.ToolDescriptions, nil)
	core := []agent.Middleware{}
	if options.EnableTodo && !options.DisableTodo {
		core = append(core, agent.TodoList())
	}
	if options.Skills != nil || options.SkillCatalog != nil {
		middleware, err := SkillsMiddleware(SkillsOptions{
			Backend: options.Backend, Sources: options.Skills,
			Catalog: options.SkillCatalog, Activate: options.SkillActivation,
		})
		if err != nil {
			return nil, err
		}
		core = append(core, middleware)
	}
	core = append(core, filesystem)

	subagentPrivateState := map[string]bool{}
	if !options.DisableSubagents {
		subagents, err := buildDeclarativeSubagents(options, inheritedTools)
		if err != nil {
			return nil, err
		}
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
				inheritAllState: true,
			}}, subagents...)
		}
		if len(subagents) > 0 {
			middleware, err := subagentMiddleware(subagents, subagentPrivateState)
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
	if len(options.AsyncSubagents) > 0 {
		middleware, err := AsyncSubagentMiddleware(AsyncSubagentOptions{Subagents: options.AsyncSubagents, SystemPrompt: options.AsyncSubagentPrompt})
		if err != nil {
			return nil, err
		}
		core = append(core, middleware)
	}
	tail := append([]agent.Middleware(nil), profile.Middleware...)
	tail = append(tail, agent.PromptCaching("prompt_caching", options.PromptCacheRetention, func(request agent.ModelRequest) string {
		if request.Runtime.Config.ThreadID != "" {
			return request.Runtime.Config.ThreadID
		}
		return request.Runtime.TaskID
	}))
	if options.Memory != nil || options.MemoryContents != nil {
		sources := append([]string(nil), options.Memory...)
		if sources == nil {
			for source := range options.MemoryContents {
				sources = append(sources, source)
			}
			sort.Strings(sources)
		}
		middleware, err := MemoryMiddleware(MemoryOptions{
			Backend: options.Backend, Sources: sources,
			Contents: options.MemoryContents, SystemPrompt: options.MemorySystemPrompt,
			AddCacheControl: true,
		})
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
	for _, item := range middleware {
		if excludedMiddleware[item.Name] {
			delete(excludedMiddleware, item.Name)
			continue
		}
		item.Tools = applyToolProfile(item.Tools, profile.ToolDescriptions, nil)
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
	for name, private := range privateStateFields(options.StateFields, middleware) {
		if private {
			subagentPrivateState[name] = true
		}
	}
	compiled, err := agent.New(agent.Options{
		Name: options.Name, Model: options.Model, Tools: options.Tools, SystemPrompt: options.SystemPrompt,
		SystemMessage: options.SystemMessage,
		Middleware:    middleware, StateFields: options.StateFields, StructuredOutput: options.StructuredOutput,
		Saver: options.Saver, Store: options.Store, Cache: options.Cache, Context: options.Context,
		RecursionLimit: options.RecursionLimit, MaxConcurrency: options.MaxConcurrency,
		FailOnToolError: options.FailOnToolError,
	})
	if err != nil {
		return nil, err
	}
	return &DeepAgent{Agent: compiled}, nil
}

func privateStateFields(base map[string]agent.StateField, middleware []agent.Middleware) map[string]bool {
	result := map[string]bool{}
	apply := func(name string, field agent.StateField) {
		if field.Private {
			result[name] = true
		}
	}
	for _, item := range middleware {
		for name, field := range item.Fields {
			apply(name, field)
		}
	}
	for name, field := range base {
		apply(name, field)
	}
	return result
}

func buildDeclarativeSubagents(options Options, inheritedTools []tool.Tool) ([]Subagent, error) {
	result := make([]Subagent, 0, len(options.Subagents))
	for _, spec := range options.Subagents {
		if spec.Runnable != nil {
			result = append(result, spec)
			continue
		}
		if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Description) == "" || strings.TrimSpace(spec.SystemPrompt) == "" {
			return nil, fmt.Errorf("declarative subagent name, description, and system prompt are required")
		}
		chat := spec.Model
		if chat == nil {
			chat = options.Model
		}
		profile, err := resolveProfiles(chat, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("subagent %q profile: %w", spec.Name, err)
		}
		permissions := spec.Permissions
		if permissions == nil {
			permissions = options.Permissions
		}
		interruptOn := spec.InterruptOn
		if interruptOn == nil {
			interruptOn = options.InterruptOn
		}
		filesystem, err := FilesystemMiddleware(FilesystemOptions{
			Backend: options.Backend, Permissions: permissions, Tools: options.FilesystemTools,
			ApprovalOverrides: interruptOn, ToolDescriptions: options.FilesystemToolDescriptions,
			MaxExecuteTimeout: options.MaxExecuteTimeout, VideoExtractor: options.VideoExtractor,
			MaxVideoBytes: options.MaxVideoBytes, VideoSamplingRate: options.VideoSamplingRate,
		})
		if err != nil {
			return nil, fmt.Errorf("subagent %q filesystem: %w", spec.Name, err)
		}
		filesystem.Tools = applyToolProfile(filesystem.Tools, profile.ToolDescriptions, nil)
		core := []agent.Middleware{filesystem}
		if !options.DisableSummary {
			summary := options.Summarization
			if summary.Model == nil {
				summary.Model = chat
			}
			if summary.Backend == nil {
				summary.Backend = options.Backend
			}
			compact, err := SummarizationMiddleware(summary)
			if err != nil {
				return nil, fmt.Errorf("subagent %q summary: %w", spec.Name, err)
			}
			core = append(core, compact)
		}
		core = append(core, PatchToolCallsMiddleware())
		if len(spec.Skills) > 0 {
			skills, err := SkillsMiddleware(SkillsOptions{Backend: options.Backend, Sources: spec.Skills})
			if err != nil {
				return nil, fmt.Errorf("subagent %q skills: %w", spec.Name, err)
			}
			core = append(core, skills)
		}
		tail := append([]agent.Middleware(nil), profile.Middleware...)
		tail = append(tail, agent.PromptCaching("prompt_caching", options.PromptCacheRetention, func(request agent.ModelRequest) string {
			if request.Runtime.Config.ThreadID != "" {
				return request.Runtime.Config.ThreadID
			}
			return request.Runtime.TaskID
		}))
		if len(interruptOn) > 0 {
			tail = append(tail, agent.HumanApproval(interruptOn))
		}
		middleware := mergeMiddleware(core, tail, spec.Middleware)
		middleware, err = filterProfileMiddleware(middleware, profile, true)
		if err != nil {
			return nil, fmt.Errorf("subagent %q middleware: %w", spec.Name, err)
		}
		if len(profile.ExcludeTools) > 0 {
			middleware = append(middleware, ToolExclusionMiddleware(profile.ExcludeTools))
		}
		tools := spec.Tools
		if tools == nil {
			tools = inheritedTools
		}
		tools = applyToolProfile(tools, profile.ToolDescriptions, nil)
		compiled, err := agent.New(agent.Options{
			Name: spec.Name, Model: chat, Tools: tools,
			SystemPrompt: applyProfilePrompt(profile, "", spec.SystemPrompt), Middleware: middleware,
			StateFields: options.StateFields, StructuredOutput: spec.StructuredOutput, Saver: options.Saver,
			Store: options.Store, Cache: options.Cache, Context: options.Context,
			RecursionLimit: options.RecursionLimit, MaxConcurrency: options.MaxConcurrency,
			FailOnToolError: options.FailOnToolError,
		})
		if err != nil {
			return nil, fmt.Errorf("subagent %q: %w", spec.Name, err)
		}
		spec.Runnable = compiled
		spec.inheritAllState = spec.InheritedState == nil
		result = append(result, spec)
	}
	return result, nil
}

func optionsNeedSaver(options Options) bool {
	if len(options.InterruptOn) > 0 || permissionsNeedSaver(options.Permissions) {
		return true
	}
	if options.DisableSubagents {
		return false
	}
	for _, spec := range options.Subagents {
		if spec.Runnable != nil {
			continue
		}
		interruptOn := spec.InterruptOn
		if interruptOn == nil {
			interruptOn = options.InterruptOn
		}
		permissions := spec.Permissions
		if permissions == nil {
			permissions = options.Permissions
		}
		if len(interruptOn) > 0 || permissionsNeedSaver(permissions) {
			return true
		}
	}
	return false
}

func permissionsNeedSaver(values []FilesystemPermission) bool {
	for _, value := range values {
		if normalizedMode(value.Mode) == PermissionInterrupt {
			return true
		}
	}
	return false
}

const defaultGeneralSubagentDescription = "General-purpose agent for researching complex questions, searching for files and content, and executing multi-step tasks. Use it when a search may require several attempts. It has the same capabilities as the main agent."

const defaultGeneralSubagentPrompt = "In order to complete the objective that the user asks of you, you have access to a number of standard tools.\n\nThe calling agent only sees your final assistant message, not your intermediate work, tool results, or status tracking. Ensure your final response contains the complete answer."

func buildGeneralSubagent(options Options, filesystem agent.Middleware, profile Profile) (*agent.Agent, error) {
	middleware := []agent.Middleware{}
	if options.EnableTodo && !options.DisableTodo {
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
	middleware = append(middleware, PatchToolCallsMiddleware())
	if options.Skills != nil || options.SkillCatalog != nil {
		skills, err := SkillsMiddleware(SkillsOptions{
			Backend: options.Backend, Sources: options.Skills,
			Catalog: options.SkillCatalog, Activate: options.SkillActivation,
		})
		if err != nil {
			return nil, err
		}
		middleware = append(middleware, skills)
	}
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
		Saver: options.Saver, Store: options.Store, Cache: options.Cache, Context: options.Context,
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
		value.Tools = applyToolProfile(value.Tools, profile.ToolDescriptions, nil)
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

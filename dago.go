package dago

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacache"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastore"
	"github.com/semistrict/dago/datool"
)

// Options configures the complete local deep-agent stack.
type Options struct {
	Name                 string
	Profiles             []Profile
	Tools                []datool.Tool
	SystemMessage        damessage.Message
	Middleware           []dagent.Middleware
	Backend              dabackend.Backend
	Filesystem           Filesystem
	Interpreter          Interpreter
	Subagents            []Subagent
	AsyncSubagents       []AsyncSubagent
	AsyncSubagentPrompt  string
	DisableSubagents     bool
	Skills               Skills
	Memory               Memory
	EnableTodo           bool
	DisableSummary       bool
	Summarization        Summarization
	InterruptOn          []dagent.ApprovalRule
	PromptCacheRetention string
	StructuredOutput     *dagent.StructuredOutput
	StateFields          map[string]dagent.StateField
	Saver                dacheckpoint.Saver
	// RetainThreadState keeps active thread state in memory while checkpoints
	// remain durable. It requires one live owner per thread.
	RetainThreadState bool
	Store             dastore.Store
	Cache             dacache.Cache
	Deps              any
	RecursionLimit    int
	MaxConcurrency    int
	FailOnToolError   bool
	Metadata          map[string]json.RawMessage
	Tags              []string
	Debug             bool
}

// New constructs a deep agent. It panics when static construction options
// violate an invariant; invocation and dependency failures remain errors on Agent methods.
func New(model damodel.Chat, options Options) *dagent.Agent {
	agent, err := newAgent(model, options)
	if err != nil {
		panic(err)
	}
	return agent
}

func newAgent(model damodel.Chat, options Options) (*dagent.Agent, error) {
	if model == nil {
		return nil, fmt.Errorf("create deep agent: model is nil")
	}
	if options.SystemMessage.Role != "" && options.SystemMessage.Role != damessage.RoleSystem {
		return nil, fmt.Errorf("create deep agent: system message role must be system")
	}
	if options.Saver == nil && optionsNeedSaver(options) {
		return nil, fmt.Errorf("human approval requires a checkpointer")
	}
	if options.Backend == nil {
		memory, err := dabackend.NewState("", nil)
		if err != nil {
			return nil, err
		}
		options.Backend = memory
	}
	profile, err := resolveProfiles(model, options.Profiles)
	if err != nil {
		return nil, err
	}
	profileExclusionMatches := map[string]bool{}
	inheritedTools := append([]datool.Tool(nil), options.Tools...)
	if options.SystemMessage.Role != "" {
		copy := options.SystemMessage.Clone()
		profilePrompt := applyProfilePrompt(profile, "", "")
		if profilePrompt != "" {
			copy.Content = append(copy.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: "\n\n" + profilePrompt})
		}
		options.SystemMessage = copy
	} else {
		prompt := applyProfilePrompt(profile, "", "")
		if prompt != "" {
			options.SystemMessage = damessage.System(prompt)
		}
	}
	options.Tools = applyToolProfile(options.Tools, profile.ToolDescriptions, nil)
	filesystem, err := newFilesystem(options.Backend, configuredFilesystem(options, nil), options.InterruptOn)
	if err != nil {
		return nil, err
	}
	filesystem.Tools = applyToolProfile(filesystem.Tools, profile.ToolDescriptions, nil)
	core := []dagent.Middleware{}
	if options.EnableTodo {
		core = append(core, dagent.TodoList())
	}
	if options.Skills.Sources != nil || options.Skills.LabeledSources != nil || options.Skills.Catalog != nil {
		middleware, err := newSkills(options.Backend, options.Skills)
		if err != nil {
			return nil, err
		}
		core = append(core, middleware)
	}
	core = append(core, filesystem)
	if options.Interpreter.Enabled {
		interpreter, err := newInterpreter(options.Interpreter)
		if err != nil {
			return nil, err
		}
		core = append(core, interpreter)
	}

	subagentPrivateState := map[string]bool{}
	if !options.DisableSubagents {
		subagents, err := buildDeclarativeSubagents(model, options, inheritedTools)
		if err != nil {
			return nil, err
		}
		generalEnabled := profile.GeneralPurpose == nil || profile.GeneralPurpose.Mode != GeneralPurposeSubagentDisabled
		if generalEnabled && !hasSubagent(subagents, "general-purpose") {
			general, err := buildGeneralSubagent(model, options, filesystem, profile, profileExclusionMatches, nil)
			if err != nil {
				return nil, err
			}
			description := defaultGeneralSubagentDescription
			if profile.GeneralPurpose != nil && profile.GeneralPurpose.Description != nil {
				description = *profile.GeneralPurpose.Description
			}
			generalFactory := func(output *dagent.StructuredOutput) (Runnable, error) {
				return buildGeneralSubagent(model, options, filesystem, profile, profileExclusionMatches, output)
			}
			subagents = append([]Subagent{{
				Name: "general-purpose", Description: description, Runnable: general,
				inheritAllState: true, responseFactory: generalFactory,
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
		summaryModel := options.Summarization.modelFor(model)
		middleware, err := newSummarization(summaryModel, options.Backend, options.Summarization)
		if err != nil {
			return nil, err
		}
		core = append(core, middleware)
	}
	core = append(core, PatchToolCalls())
	if len(options.AsyncSubagents) > 0 {
		middleware := AsyncSubagentsWithOptions(AsyncSubagentsOptions{SystemPrompt: options.AsyncSubagentPrompt}, options.AsyncSubagents[0], options.AsyncSubagents[1:]...)
		core = append(core, middleware)
	}
	tail := append([]dagent.Middleware(nil), profile.Middleware...)
	tail = append(tail, dagent.PromptCaching(options.PromptCacheRetention, func(request dagent.ModelRequest) string {
		if request.Runtime.Config.ThreadID != "" {
			return request.Runtime.Config.ThreadID
		}
		return request.Runtime.TaskID
	}))
	if options.Memory.Sources != nil || options.Memory.Contents != nil {
		memory := options.Memory
		sources := append([]string(nil), memory.Sources...)
		if sources == nil {
			for source := range memory.Contents {
				sources = append(sources, source)
			}
			sort.Strings(sources)
		}
		memory.Sources = sources
		middleware, err := newMemory(options.Backend, memory, true)
		if err != nil {
			return nil, err
		}
		tail = append(tail, middleware)
	}
	if len(options.InterruptOn) > 0 {
		tail = append(tail, dagent.HumanApproval(options.InterruptOn))
	}
	middleware := mergeMiddleware(core, tail, options.Middleware)
	middleware, err = filterProfileMiddleware(middleware, profile, profileExclusionMatches, true)
	if err != nil {
		return nil, err
	}
	if len(profile.ExcludeTools) > 0 {
		middleware = append(middleware, ToolExclusion(profile.ExcludeTools))
	}
	middleware = moveMiddlewareLast(middleware, "code_interpreter")
	for name, private := range privateStateFields(options.StateFields, middleware) {
		if private {
			subagentPrivateState[name] = true
		}
	}
	compiled := dagent.New(model, dagent.Options{
		Name: options.Name, Tools: options.Tools,
		SystemMessage: options.SystemMessage,
		Middleware:    middleware, StateFields: options.StateFields, StructuredOutput: options.StructuredOutput,
		Saver: options.Saver, Store: options.Store, Cache: options.Cache, Deps: options.Deps,
		RetainThreadState: options.RetainThreadState,
		RecursionLimit:    options.RecursionLimit, MaxConcurrency: options.MaxConcurrency,
		FailOnToolError: options.FailOnToolError,
		Metadata:        options.Metadata, Tags: options.Tags, Debug: options.Debug,
	})
	return compiled, nil
}

func privateStateFields(base map[string]dagent.StateField, middleware []dagent.Middleware) map[string]bool {
	result := map[string]bool{}
	apply := func(name string, field dagent.StateField) {
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

func buildDeclarativeSubagents(parentModel damodel.Chat, options Options, inheritedTools []datool.Tool) ([]Subagent, error) {
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
			chat = parentModel
		}
		profile, err := resolveProfiles(chat, nil)
		if err != nil {
			return nil, fmt.Errorf("subagent %q profile: %w", spec.Name, err)
		}
		permissions := spec.Permissions
		if permissions == nil {
			permissions = options.Filesystem.Permissions
		}
		interruptOn := spec.InterruptOn
		if interruptOn == nil {
			interruptOn = options.InterruptOn
		}
		filesystemOptions := configuredFilesystem(options, permissions)
		filesystem, err := newFilesystem(options.Backend, filesystemOptions, interruptOn)
		if err != nil {
			return nil, fmt.Errorf("subagent %q filesystem: %w", spec.Name, err)
		}
		filesystem.Tools = applyToolProfile(filesystem.Tools, profile.ToolDescriptions, nil)
		core := []dagent.Middleware{filesystem}
		if options.Interpreter.Enabled {
			interpreter, err := newInterpreter(options.Interpreter)
			if err != nil {
				return nil, fmt.Errorf("subagent %q interpreter: %w", spec.Name, err)
			}
			core = append(core, interpreter)
		}
		if !options.DisableSummary {
			compact, err := newSummarization(options.Summarization.modelFor(chat), options.Backend, options.Summarization)
			if err != nil {
				return nil, fmt.Errorf("subagent %q summarization: %w", spec.Name, err)
			}
			core = append(core, compact)
		}
		core = append(core, PatchToolCalls())
		if spec.Skills.Sources != nil || spec.Skills.LabeledSources != nil || spec.Skills.Catalog != nil {
			skills, err := newSkills(options.Backend, spec.Skills)
			if err != nil {
				return nil, fmt.Errorf("subagent %q skills: %w", spec.Name, err)
			}
			core = append(core, skills)
		}
		tail := append([]dagent.Middleware(nil), profile.Middleware...)
		tail = append(tail, dagent.PromptCaching(options.PromptCacheRetention, func(request dagent.ModelRequest) string {
			if request.Runtime.Config.ThreadID != "" {
				return request.Runtime.Config.ThreadID
			}
			return request.Runtime.TaskID
		}))
		if len(interruptOn) > 0 {
			tail = append(tail, dagent.HumanApproval(interruptOn))
		}
		middleware := mergeMiddleware(core, tail, spec.Middleware)
		middleware, err = filterProfileMiddleware(middleware, profile, map[string]bool{}, true)
		if err != nil {
			return nil, fmt.Errorf("subagent %q middleware: %w", spec.Name, err)
		}
		if len(profile.ExcludeTools) > 0 {
			middleware = append(middleware, ToolExclusion(profile.ExcludeTools))
		}
		middleware = moveMiddlewareLast(middleware, "code_interpreter")
		tools := spec.Tools
		if tools == nil {
			tools = inheritedTools
		}
		tools = applyToolProfile(tools, profile.ToolDescriptions, nil)
		name := spec.Name
		prompt := applyProfilePrompt(profile, "", spec.SystemPrompt)
		compile := func(output *dagent.StructuredOutput) (Runnable, error) {
			return dagent.New(chat, dagent.Options{
				Name: name, Tools: tools,
				SystemMessage: damessage.System(prompt), Middleware: middleware,
				StateFields: options.StateFields, StructuredOutput: output, Saver: options.Saver,
				Store: options.Store, Cache: options.Cache, Deps: options.Deps,
				RecursionLimit: options.RecursionLimit, MaxConcurrency: options.MaxConcurrency,
				FailOnToolError: options.FailOnToolError,
				Metadata:        options.Metadata, Tags: options.Tags, Debug: options.Debug,
			}), nil
		}
		compiled, err := compile(spec.StructuredOutput)
		if err != nil {
			return nil, fmt.Errorf("subagent %q: %w", spec.Name, err)
		}
		spec.Runnable = compiled
		spec.inheritAllState = spec.InheritedState == nil
		spec.responseFactory = compile
		result = append(result, spec)
	}
	return result, nil
}

func optionsNeedSaver(options Options) bool {
	if len(options.InterruptOn) > 0 || permissionsNeedSaver(options.Filesystem.Permissions) {
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
			permissions = options.Filesystem.Permissions
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

func buildGeneralSubagent(model damodel.Chat, options Options, filesystem dagent.Middleware, profile Profile, exclusionMatches map[string]bool, structuredOutput *dagent.StructuredOutput) (*dagent.Agent, error) {
	middleware := []dagent.Middleware{}
	middleware = append(middleware, filesystem)
	if options.Interpreter.Enabled {
		interpreter, err := newInterpreter(options.Interpreter)
		if err != nil {
			return nil, err
		}
		middleware = append(middleware, interpreter)
	}
	if !options.DisableSummary {
		compact, err := newSummarization(options.Summarization.modelFor(model), options.Backend, options.Summarization)
		if err != nil {
			return nil, err
		}
		middleware = append(middleware, compact)
	}
	middleware = append(middleware, PatchToolCalls())
	if options.Skills.Sources != nil || options.Skills.LabeledSources != nil || options.Skills.Catalog != nil {
		skills, err := newSkills(options.Backend, options.Skills)
		if err != nil {
			return nil, err
		}
		middleware = append(middleware, skills)
	}
	middleware = append(middleware, profile.Middleware...)
	middleware = append(middleware, dagent.PromptCaching(options.PromptCacheRetention, func(request dagent.ModelRequest) string {
		if request.Runtime.Config.ThreadID != "" {
			return request.Runtime.Config.ThreadID
		}
		return request.Runtime.TaskID
	}))
	if len(options.InterruptOn) > 0 {
		middleware = append(middleware, dagent.HumanApproval(options.InterruptOn))
	}
	for _, custom := range options.Middleware {
		for index, current := range middleware {
			if current.Name == custom.Name {
				if custom.SerializedName == "" {
					custom.SerializedName = current.SerializedName
				}
				middleware[index] = custom
				break
			}
		}
	}
	filtered, err := filterProfileMiddleware(middleware, profile, exclusionMatches, false)
	if err != nil {
		return nil, err
	}
	middleware = filtered
	if len(profile.ExcludeTools) > 0 {
		middleware = append(middleware, ToolExclusion(profile.ExcludeTools))
	}
	middleware = moveMiddlewareLast(middleware, "code_interpreter")
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
	return dagent.New(model, dagent.Options{
		Name: "general-purpose", Tools: options.Tools,
		SystemMessage: damessage.System(prompt), Middleware: middleware, StateFields: options.StateFields,
		StructuredOutput: structuredOutput,
		Saver:            options.Saver, Store: options.Store, Cache: options.Cache, Deps: options.Deps,
		RecursionLimit: options.RecursionLimit, MaxConcurrency: options.MaxConcurrency,
		FailOnToolError: options.FailOnToolError,
		Metadata:        options.Metadata, Tags: options.Tags, Debug: options.Debug,
	}), nil
}

func configuredFilesystem(options Options, permissions []FilesystemPermission) Filesystem {
	configured := options.Filesystem
	if permissions != nil {
		configured.Permissions = permissions
	}
	return configured
}

// Custom middleware replaces an existing entry with the same name in place.
// Brand-new middleware is inserted after required core scaffolding and before
// the profile, prompt-caching, memory, and approval tail.
func mergeMiddleware(core, tail, custom []dagent.Middleware) []dagent.Middleware {
	result := append(append([]dagent.Middleware(nil), core...), tail...)
	positions := map[string]int{}
	for index, item := range result {
		positions[item.Name] = index
	}
	insertAt := len(core)
	var additions []dagent.Middleware
	for _, item := range custom {
		if index, exists := positions[item.Name]; exists {
			if item.SerializedName == "" {
				item.SerializedName = result[index].SerializedName
			}
			result[index] = item
		} else {
			additions = append(additions, item)
		}
	}
	if len(additions) > 0 {
		result = append(result, make([]dagent.Middleware, len(additions))...)
		copy(result[insertAt+len(additions):], result[insertAt:len(result)-len(additions)])
		copy(result[insertAt:], additions)
	}
	return result
}

func moveMiddlewareLast(values []dagent.Middleware, name string) []dagent.Middleware {
	for index, value := range values {
		if value.Name != name || index == len(values)-1 {
			continue
		}
		copy(values[index:], values[index+1:])
		values[len(values)-1] = value
		break
	}
	return values
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

func filterProfileMiddleware(values []dagent.Middleware, profile Profile, matched map[string]bool, verify bool) ([]dagent.Middleware, error) {
	if matched == nil {
		matched = map[string]bool{}
	}
	for _, name := range profile.ExcludeMiddleware {
		if isRequiredMiddlewareExclusion(name) {
			return nil, fmt.Errorf("profile cannot exclude required middleware %q", name)
		}
	}
	result := make([]dagent.Middleware, 0, len(values))
	for _, value := range values {
		matching := ""
		for _, excluded := range profile.ExcludeMiddleware {
			if excluded == value.Name || (value.SerializedName != "" && excluded == value.SerializedName) {
				if matching != "" && matching != excluded {
					return nil, fmt.Errorf("middleware %q matched multiple profile exclusions %q and %q", value.Name, matching, excluded)
				}
				matching = excluded
			}
		}
		if matching != "" {
			matched[matching] = true
			continue
		}
		value.Tools = applyToolProfile(value.Tools, profile.ToolDescriptions, nil)
		result = append(result, value)
	}
	if verify {
		for _, name := range profile.ExcludeMiddleware {
			if !matched[name] {
				return nil, fmt.Errorf("profile middleware exclusion %q matched no assembled middleware", name)
			}
		}
	}
	return result, nil
}

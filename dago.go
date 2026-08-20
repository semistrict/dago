package dago

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
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

type agentConfig struct {
	Name             string
	Profiles         []Profile
	Tools            []datool.Tool
	SystemMessage    damessage.Message
	Backend          dabackend.Backend
	middleware       []middlewareBuilder
	StructuredOutput *dagent.StructuredOutput
	StateFields      map[string]dagent.StateField
	Saver            dacheckpoint.Saver
	// RetainThreadState keeps active thread state in memory while checkpoints
	// remain durable. It requires one live owner per thread.
	RetainThreadState  bool
	Store              dastore.Store
	Cache              dacache.Cache
	Deps               any
	RecursionLimit     int
	MaxConcurrency     int
	FailOnToolError    bool
	Metadata           map[string]json.RawMessage
	Tags               []string
	Debug              bool
	Logger             *slog.Logger
	managedMemoryPaths []string
	approvalRules      []dagent.ApprovalRule
	needsSaver         bool
	removeMiddleware   map[string]bool
	resolvedProfile    *Profile
	profilePromptReady bool
	verifyExclusions   bool
}

type buildContext struct {
	Model        damodel.Chat
	Backend      dabackend.Backend
	Config       *agentConfig
	Profile      Profile
	RawTools     []datool.Tool
	PrivateState map[string]bool
}

// New constructs a deep agent. It panics when static construction options
// violate an invariant; invocation and dependency failures remain errors on Agent methods.
func New(model damodel.Chat, options ...Option) *dagent.Agent {
	config := agentConfig{verifyExclusions: true}
	for index, option := range options {
		if option == nil {
			panic(fmt.Sprintf("create deep agent: option %d is nil", index))
		}
		option.apply(&config)
	}
	agent, err := newAgent(model, config)
	if err != nil {
		panic(err)
	}
	return agent
}

func newAgent(model damodel.Chat, options agentConfig) (*dagent.Agent, error) {
	if nilInterface(model) {
		return nil, fmt.Errorf("create deep agent: model is nil")
	}
	options.Metadata = withVersionMetadata(options.Metadata)
	if options.SystemMessage.Role != "" && options.SystemMessage.Role != damessage.RoleSystem {
		return nil, fmt.Errorf("create deep agent: system message role must be system")
	}
	if nilInterface(options.Saver) && options.needsSaver {
		return nil, fmt.Errorf("human approval requires a checkpointer")
	}
	if nilInterface(options.Backend) {
		options.Backend = dabackend.NewState("", nil)
	}
	var profile Profile
	if options.resolvedProfile != nil {
		profile = cloneProfile(*options.resolvedProfile)
	} else {
		var err error
		profile, err = resolveProfiles(model, options.Profiles)
		if err != nil {
			return nil, err
		}
	}
	profileExclusionMatches := map[string]bool{}
	rawTools := append([]datool.Tool(nil), options.Tools...)
	if !options.profilePromptReady && options.SystemMessage.Role != "" {
		copy := options.SystemMessage.Clone()
		profilePrompt := applyProfilePrompt(profile, "", "")
		if profilePrompt != "" {
			copy.Content = append(copy.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: "\n\n" + profilePrompt})
		}
		options.SystemMessage = copy
	} else if !options.profilePromptReady {
		prompt := applyProfilePrompt(profile, "", "")
		if prompt != "" {
			options.SystemMessage = damessage.System(prompt)
		}
	}
	options.Tools = applyToolProfile(options.Tools, profile.ToolDescriptions, nil)
	subagentPrivateState := map[string]bool{}
	context := buildContext{
		Model: model, Backend: options.Backend, Config: &options, Profile: profile,
		RawTools: rawTools, PrivateState: subagentPrivateState,
	}
	core := make([]dagent.Middleware, 0, len(options.middleware)+1)
	custom := []dagent.Middleware{}
	for _, builder := range options.middleware {
		middleware, err := builder.build(context)
		if err != nil {
			return nil, err
		}
		if builder.optional && middleware.Name == "" {
			continue
		}
		if builder.custom {
			custom = append(custom, middleware)
		} else {
			core = append(core, middleware)
		}
	}
	core = mergeMiddlewareByName(core, []dagent.Middleware{PatchToolCalls()})
	middleware := mergeMiddleware(core, profile.Middleware, custom)
	if len(options.removeMiddleware) > 0 {
		filtered := middleware[:0]
		for _, item := range middleware {
			if !options.removeMiddleware[item.Name] && !options.removeMiddleware[item.SerializedName] {
				filtered = append(filtered, item)
			}
		}
		middleware = filtered
	}
	var err error
	middleware, err = filterProfileMiddleware(middleware, profile, profileExclusionMatches, options.verifyExclusions)
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
		Metadata:        options.Metadata, Tags: options.Tags, Debug: options.Debug, Logger: options.Logger,
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

func buildDeclarativeSubagents(context buildContext, subagents []Subagent) ([]Subagent, error) {
	result := make([]Subagent, 0, len(subagents))
	for _, spec := range subagents {
		if spec.runnable != nil {
			result = append(result, spec)
			continue
		}
		chat := spec.model
		if chat == nil {
			chat = context.Model
		}
		configured := inheritedSubagentConfig(*context.Config, context.RawTools, false)
		for index, option := range spec.options {
			if option == nil {
				return nil, fmt.Errorf("subagent %q option %d is nil", spec.name, index)
			}
			option.apply(&configured)
		}
		configured.Name = spec.name
		if configured.SystemMessage.Role == "" {
			return nil, fmt.Errorf("declarative subagent %q requires a system message", spec.name)
		}
		compiledConfig := configured
		compile := func(output *dagent.StructuredOutput) (Runnable, error) {
			current := compiledConfig
			current.StructuredOutput = output
			return newAgent(chat, current)
		}
		compiled, err := compile(compiledConfig.StructuredOutput)
		if err != nil {
			return nil, fmt.Errorf("subagent %q: %w", spec.name, err)
		}
		spec.runnable = compiled
		spec.responseFactory = compile
		result = append(result, spec)
	}
	return result, nil
}

func inheritedSubagentConfig(parent agentConfig, tools []datool.Tool, general bool) agentConfig {
	middleware := make([]middlewareBuilder, 0, len(parent.middleware))
	for _, builder := range parent.middleware {
		if (!general && builder.inheritDeclarative) || (general && builder.inheritGeneral) {
			middleware = append(middleware, builder)
		}
	}
	return agentConfig{
		Tools: append([]datool.Tool{}, tools...), Backend: parent.Backend, middleware: middleware,
		StateFields: parent.StateFields,
		Saver:       parent.Saver, Store: parent.Store, Cache: parent.Cache, Deps: parent.Deps,
		RecursionLimit: parent.RecursionLimit, MaxConcurrency: parent.MaxConcurrency,
		FailOnToolError: parent.FailOnToolError,
		Metadata:        parent.Metadata, Tags: append([]string(nil), parent.Tags...), Debug: parent.Debug, Logger: parent.Logger,
		approvalRules:      append([]dagent.ApprovalRule(nil), parent.approvalRules...),
		managedMemoryPaths: append([]string(nil), parent.managedMemoryPaths...),
		needsSaver:         parent.needsSaver, removeMiddleware: cloneBoolMap(parent.removeMiddleware), verifyExclusions: true,
	}
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func cloneBoolMap(values map[string]bool) map[string]bool {
	if values == nil {
		return nil
	}
	result := make(map[string]bool, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
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

func buildSubagentsMiddleware(context buildContext, declared []Subagent) (dagent.Middleware, error) {
	subagents, err := buildDeclarativeSubagents(context, declared)
	if err != nil {
		return dagent.Middleware{}, err
	}
	generalEnabled := context.Profile.GeneralPurpose == nil || context.Profile.GeneralPurpose.Mode != GeneralPurposeSubagentDisabled
	if generalEnabled && !hasSubagent(subagents, "general-purpose") {
		general, err := buildGeneralSubagent(context, nil)
		if err != nil {
			return dagent.Middleware{}, err
		}
		description := defaultGeneralSubagentDescription
		if context.Profile.GeneralPurpose != nil && context.Profile.GeneralPurpose.Description != nil {
			description = *context.Profile.GeneralPurpose.Description
		}
		generalFactory := func(output *dagent.StructuredOutput) (Runnable, error) {
			return buildGeneralSubagent(context, output)
		}
		subagents = append([]Subagent{{
			name: "general-purpose", description: description, runnable: general,
			inheritAllState: true, responseFactory: generalFactory,
		}}, subagents...)
	}
	if len(subagents) == 0 {
		return dagent.Middleware{}, nil
	}
	return subagentMiddleware(subagents, context.PrivateState)
}

func buildGeneralSubagent(context buildContext, structuredOutput *dagent.StructuredOutput) (*dagent.Agent, error) {
	config := inheritedSubagentConfig(*context.Config, context.Config.Tools, true)
	config.Name = "general-purpose"
	config.StructuredOutput = structuredOutput
	config.resolvedProfile = &context.Profile
	config.profilePromptReady = true
	config.verifyExclusions = false
	prompt := applyProfilePrompt(context.Profile, "", defaultGeneralSubagentPrompt)
	if context.Profile.GeneralPurpose != nil && context.Profile.GeneralPurpose.SystemPrompt != nil {
		prompt = strings.TrimSpace(*context.Profile.GeneralPurpose.SystemPrompt)
		if context.Profile.SystemPrompt != "" {
			prompt += "\n\n" + strings.TrimSpace(context.Profile.SystemPrompt)
		}
		if context.Profile.SystemPromptSuffix != nil && strings.TrimSpace(*context.Profile.SystemPromptSuffix) != "" {
			prompt += "\n\n" + strings.TrimSpace(*context.Profile.SystemPromptSuffix)
		}
	}
	config.SystemMessage = damessage.System(prompt)
	return newAgent(context.Model, config)
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
	additionPositions := map[string]int{}
	for _, item := range custom {
		if index, exists := positions[item.Name]; exists {
			if item.SerializedName == "" {
				item.SerializedName = result[index].SerializedName
			}
			result[index] = item
		} else if index, exists := additionPositions[item.Name]; exists {
			if item.SerializedName == "" {
				item.SerializedName = additions[index].SerializedName
			}
			additions[index] = item
		} else {
			additionPositions[item.Name] = len(additions)
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
		if value.name == name {
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

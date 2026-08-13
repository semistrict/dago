package dago

import (
	"encoding/json"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacache"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastore"
	"github.com/semistrict/dago/datool"
)

// Option configures an agent constructed by NewAgent.
type Option interface {
	apply(*agentConfig)
}

type optionFunc func(*agentConfig)

func (configure optionFunc) apply(options *agentConfig) { configure(options) }

// WithName sets the name stamped on model responses.
func WithName(name string) Option {
	return optionFunc(func(options *agentConfig) { options.Name = name })
}

// WithProfiles sets the ordered harness profiles applied to the agent.
func WithProfiles(profiles ...Profile) Option {
	return optionFunc(func(options *agentConfig) { options.Profiles = append([]Profile(nil), profiles...) })
}

// WithTools sets the caller-provided model tools.
func WithTools(tools ...datool.Tool) Option {
	return optionFunc(func(options *agentConfig) { options.Tools = append([]datool.Tool{}, tools...) })
}

// WithSystemMessage sets the agent's system message.
func WithSystemMessage(message damessage.Message) Option {
	return optionFunc(func(options *agentConfig) { options.SystemMessage = message })
}

// WithMiddleware sets the caller-provided middleware.
func WithMiddleware(middleware ...dagent.Middleware) Option {
	return optionFunc(func(options *agentConfig) { options.Middleware = append([]dagent.Middleware(nil), middleware...) })
}

// WithBackend sets the filesystem and memory backend.
func WithBackend(backend dabackend.Backend) Option {
	return optionFunc(func(options *agentConfig) { options.Backend = backend })
}

// WithFilesystem sets filesystem tool and permission configuration.
func WithFilesystem(filesystem Filesystem) Option {
	return optionFunc(func(options *agentConfig) { options.Filesystem = filesystem })
}

// WithFilesystemPermissions sets filesystem permissions while preserving the
// rest of the filesystem configuration inherited by a subagent.
func WithFilesystemPermissions(permissions ...FilesystemPermission) Option {
	return optionFunc(func(options *agentConfig) {
		options.Filesystem.Permissions = append([]FilesystemPermission{}, permissions...)
	})
}

// WithInterpreter enables the persistent JavaScript interpreter with the given configuration.
func WithInterpreter(interpreter Interpreter) Option {
	return optionFunc(func(options *agentConfig) {
		copy := interpreter
		options.Interpreter = &copy
	})
}

// WithSubagents sets the inline subagents available through the task tool.
func WithSubagents(subagents ...Subagent) Option {
	return optionFunc(func(options *agentConfig) {
		options.Subagents = append([]Subagent(nil), subagents...)
		options.DisableSubagents = false
	})
}

// WithAsyncSubagents sets the hosted asynchronous subagents.
func WithAsyncSubagents(subagents ...AsyncSubagent) Option {
	return optionFunc(func(options *agentConfig) { options.AsyncSubagents = append([]AsyncSubagent(nil), subagents...) })
}

// WithAsyncSubagentPrompt sets the additional async-subagent system prompt.
func WithAsyncSubagentPrompt(prompt string) Option {
	return optionFunc(func(options *agentConfig) { options.AsyncSubagentPrompt = prompt })
}

// WithoutSubagents disables the task tool and default general-purpose subagent.
func WithoutSubagents() Option {
	return optionFunc(func(options *agentConfig) { options.DisableSubagents = true })
}

// WithSkills sets skill discovery and prompt configuration.
func WithSkills(skills Skills) Option {
	return optionFunc(func(options *agentConfig) { options.Skills = skills })
}

// WithMemory sets persistent memory prompt configuration.
func WithMemory(memory Memory) Option {
	return optionFunc(func(options *agentConfig) { options.Memory = memory })
}

// WithTodo enables the todo-list middleware.
func WithTodo() Option {
	return optionFunc(func(options *agentConfig) { options.EnableTodo = true })
}

// WithoutSummary disables automatic conversation summarization.
func WithoutSummary() Option {
	return optionFunc(func(options *agentConfig) { options.DisableSummary = true })
}

// WithSummarization sets automatic conversation summarization configuration.
func WithSummarization(summarization Summarization) Option {
	return optionFunc(func(options *agentConfig) {
		options.Summarization = summarization
		options.DisableSummary = false
	})
}

// WithApprovalRules sets tool calls that require human approval.
func WithApprovalRules(rules ...dagent.ApprovalRule) Option {
	return optionFunc(func(options *agentConfig) { options.InterruptOn = append([]dagent.ApprovalRule(nil), rules...) })
}

// WithPromptCacheRetention sets the provider prompt-cache retention policy.
func WithPromptCacheRetention(retention string) Option {
	return optionFunc(func(options *agentConfig) { options.PromptCacheRetention = retention })
}

// WithStructuredOutput sets the agent's structured response contract.
func WithStructuredOutput(output *dagent.StructuredOutput) Option {
	return optionFunc(func(options *agentConfig) { options.StructuredOutput = output })
}

// WithStateFields sets application-owned state fields and reducers.
func WithStateFields(fields map[string]dagent.StateField) Option {
	return optionFunc(func(options *agentConfig) { options.StateFields = fields })
}

// WithSaver sets the thread checkpoint saver.
func WithSaver(saver dacheckpoint.Saver) Option {
	return optionFunc(func(options *agentConfig) { options.Saver = saver })
}

// WithRetainedThreadState keeps active thread state in memory between invocations.
func WithRetainedThreadState() Option {
	return optionFunc(func(options *agentConfig) { options.RetainThreadState = true })
}

// WithStore sets the durable cross-thread store.
func WithStore(store dastore.Store) Option {
	return optionFunc(func(options *agentConfig) { options.Store = store })
}

// WithCache sets the runtime cache.
func WithCache(cache dacache.Cache) Option {
	return optionFunc(func(options *agentConfig) { options.Cache = cache })
}

// WithDependencies sets construction-time runtime dependencies.
func WithDependencies(dependencies any) Option {
	return optionFunc(func(options *agentConfig) { options.Deps = dependencies })
}

// WithRecursionLimit sets the maximum graph steps for one invocation.
func WithRecursionLimit(limit int) Option {
	return optionFunc(func(options *agentConfig) { options.RecursionLimit = limit })
}

// WithMaxConcurrency sets the maximum concurrent graph tasks.
func WithMaxConcurrency(limit int) Option {
	return optionFunc(func(options *agentConfig) { options.MaxConcurrency = limit })
}

// WithFatalToolErrors makes operational tool failures terminate the invocation.
func WithFatalToolErrors() Option {
	return optionFunc(func(options *agentConfig) { options.FailOnToolError = true })
}

// WithMetadata sets invocation metadata used by tracing and evaluation adapters.
func WithMetadata(metadata map[string]json.RawMessage) Option {
	return optionFunc(func(options *agentConfig) { options.Metadata = metadata })
}

// WithTags sets invocation tags used by tracing and evaluation adapters.
func WithTags(tags ...string) Option {
	return optionFunc(func(options *agentConfig) { options.Tags = append([]string(nil), tags...) })
}

// WithDebug enables graph debug logging.
func WithDebug() Option {
	return optionFunc(func(options *agentConfig) { options.Debug = true })
}

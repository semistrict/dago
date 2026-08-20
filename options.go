package dago

import (
	"encoding/json"
	"log/slog"
	"sort"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacache"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastore"
	"github.com/semistrict/dago/datool"
)

// Option configures an agent constructed by New.
type Option interface {
	apply(*agentConfig)
}

type optionFunc func(*agentConfig)

func (configure optionFunc) apply(config *agentConfig) { configure(config) }

type middlewareBuilder struct {
	build              func(buildContext) (dagent.Middleware, error)
	custom             bool
	optional           bool
	inheritDeclarative bool
	inheritGeneral     bool
}

// WithName sets the name stamped on model responses.
func WithName(name string) Option {
	return optionFunc(func(config *agentConfig) { config.Name = name })
}

// WithProfiles sets the ordered harness profiles applied to the agent.
func WithProfiles(profiles ...Profile) Option {
	return optionFunc(func(config *agentConfig) { config.Profiles = append([]Profile(nil), profiles...) })
}

// WithTools sets the caller-provided model tools.
func WithTools(tools ...datool.Tool) Option {
	return optionFunc(func(config *agentConfig) { config.Tools = append([]datool.Tool{}, tools...) })
}

// WithSystemMessage sets the agent's system message.
func WithSystemMessage(message damessage.Message) Option {
	return optionFunc(func(config *agentConfig) { config.SystemMessage = message })
}

// WithSystemPrompt sets a plain-text system message.
func WithSystemPrompt(prompt string) Option {
	return WithSystemMessage(damessage.System(prompt))
}

// WithMiddleware appends caller-provided middleware. A later middleware with
// the same name replaces the earlier contribution.
func WithMiddleware(middleware ...dagent.Middleware) Option {
	values := append([]dagent.Middleware(nil), middleware...)
	return optionFunc(func(config *agentConfig) {
		for _, current := range values {
			item := current
			config.middleware = append(config.middleware, middlewareBuilder{
				custom: true, inheritGeneral: true,
				build: func(buildContext) (dagent.Middleware, error) { return item, nil },
			})
		}
	})
}

// WithoutMiddleware removes inherited or profile-provided middleware by public
// or serialized name. It is primarily useful for declarative child agents.
func WithoutMiddleware(names ...string) Option {
	values := append([]string(nil), names...)
	return optionFunc(func(config *agentConfig) {
		if config.removeMiddleware == nil {
			config.removeMiddleware = map[string]bool{}
		}
		for _, name := range values {
			config.removeMiddleware[name] = true
		}
	})
}

// WithBackend sets the backend used by backend-backed middleware.
func WithBackend(backend dabackend.Backend) Option {
	return optionFunc(func(config *agentConfig) { config.Backend = backend })
}

// WithFilesystem adds filesystem middleware with the given configuration.
func WithFilesystem(filesystem Filesystem) Option {
	return optionFunc(func(config *agentConfig) {
		config.middleware = append(config.middleware, middlewareBuilder{
			inheritDeclarative: true, inheritGeneral: true,
			build: func(context buildContext) (dagent.Middleware, error) {
				configured := filesystem
				configured.managedMemoryPaths = appendUniqueStrings(configured.managedMemoryPaths, context.Config.managedMemoryPaths...)
				return newFilesystem(context.Backend, configured, context.Config.approvalRules)
			},
		})
		config.needsSaver = config.needsSaver || permissionsNeedSaver(filesystem.Permissions)
	})
}

// WithInterpreter adds persistent JavaScript interpreter middleware.
func WithInterpreter(interpreter Interpreter) Option {
	return optionFunc(func(config *agentConfig) {
		config.middleware = append(config.middleware, middlewareBuilder{
			inheritDeclarative: true, inheritGeneral: true,
			build: func(buildContext) (dagent.Middleware, error) { return compileInterpreter(interpreter) },
		})
	})
}

// WithSubagents adds task delegation middleware. Calling it without explicit
// subagents enables only the default general-purpose worker.
func WithSubagents(subagents ...Subagent) Option {
	values := append([]Subagent(nil), subagents...)
	return optionFunc(func(config *agentConfig) {
		config.middleware = append(config.middleware, middlewareBuilder{
			optional: true,
			build: func(context buildContext) (dagent.Middleware, error) {
				return buildSubagentsMiddleware(context, values)
			},
		})
	})
}

// WithAsyncSubagents adds hosted asynchronous-subagent middleware.
func WithAsyncSubagents(subagents []AsyncSubagent, options ...AsyncSubagentsOption) Option {
	values := append([]AsyncSubagent(nil), subagents...)
	configured := append([]AsyncSubagentsOption(nil), options...)
	return optionFunc(func(config *agentConfig) {
		config.middleware = append(config.middleware, middlewareBuilder{
			build: func(buildContext) (dagent.Middleware, error) {
				return AsyncSubagents(values, configured...), nil
			},
		})
	})
}

// WithSkills adds skill-discovery middleware.
func WithSkills(skills Skills) Option {
	return optionFunc(func(config *agentConfig) {
		config.middleware = append(config.middleware, middlewareBuilder{
			inheritGeneral: true,
			build: func(context buildContext) (dagent.Middleware, error) {
				return newSkills(context.Backend, skills)
			},
		})
	})
}

// WithMemory adds persistent-memory prompt middleware.
func WithMemory(memory Memory) Option {
	configured := memory
	configured.Sources = append([]string(nil), memory.Sources...)
	if configured.Sources == nil {
		for source := range configured.Contents {
			configured.Sources = append(configured.Sources, source)
		}
		sort.Strings(configured.Sources)
	}
	return optionFunc(func(config *agentConfig) {
		config.managedMemoryPaths = appendUniqueStrings(config.managedMemoryPaths, configured.Sources...)
		config.middleware = append(config.middleware, middlewareBuilder{
			inheritDeclarative: true, inheritGeneral: true,
			build: func(context buildContext) (dagent.Middleware, error) {
				return newMemory(context.Backend, configured, true)
			},
		})
	})
}

// WithTodo adds todo-list middleware.
func WithTodo(options ...dagent.TodoOption) Option {
	configured := append([]dagent.TodoOption(nil), options...)
	return optionFunc(func(config *agentConfig) {
		config.middleware = append(config.middleware, middlewareBuilder{
			build: func(buildContext) (dagent.Middleware, error) {
				return dagent.TodoList(configured...), nil
			},
		})
	})
}

// WithSummarization adds automatic conversation-summarization middleware.
func WithSummarization(summarization Summarization) Option {
	return optionFunc(func(config *agentConfig) {
		config.middleware = append(config.middleware, middlewareBuilder{
			inheritDeclarative: true, inheritGeneral: true,
			build: func(context buildContext) (dagent.Middleware, error) {
				return newSummarization(summarization.modelFor(context.Model), context.Backend, summarization)
			},
		})
	})
}

// WithApprovalRules adds human-approval middleware.
func WithApprovalRules(rules ...dagent.ApprovalRule) Option {
	configured := append([]dagent.ApprovalRule(nil), rules...)
	return optionFunc(func(config *agentConfig) {
		config.approvalRules = configured
		config.needsSaver = config.needsSaver || len(configured) > 0
		config.middleware = append(config.middleware, middlewareBuilder{
			inheritDeclarative: true, inheritGeneral: true,
			build: func(buildContext) (dagent.Middleware, error) {
				return dagent.HumanApproval(configured), nil
			},
		})
	})
}

// WithPromptCacheRetention adds provider prompt-caching middleware.
func WithPromptCacheRetention(retention string) Option {
	return optionFunc(func(config *agentConfig) {
		config.middleware = append(config.middleware, middlewareBuilder{
			inheritDeclarative: true, inheritGeneral: true,
			build: func(buildContext) (dagent.Middleware, error) {
				return dagent.PromptCaching(retention, func(request dagent.ModelRequest) string {
					if request.Runtime.Config.ThreadID != "" {
						return request.Runtime.Config.ThreadID
					}
					return request.Runtime.TaskID
				}), nil
			},
		})
	})
}

// WithStructuredOutput sets the agent's structured response contract.
func WithStructuredOutput(output *dagent.StructuredOutput) Option {
	return optionFunc(func(config *agentConfig) { config.StructuredOutput = output })
}

// WithStateFields sets application-owned state fields and reducers.
func WithStateFields(fields map[string]dagent.StateField) Option {
	return optionFunc(func(config *agentConfig) { config.StateFields = fields })
}

// WithSaver sets the thread checkpoint saver.
func WithSaver(saver dacheckpoint.Saver) Option {
	return optionFunc(func(config *agentConfig) { config.Saver = saver })
}

// WithRetainedThreadState keeps active thread state in memory between invocations.
func WithRetainedThreadState() Option {
	return optionFunc(func(config *agentConfig) { config.RetainThreadState = true })
}

// WithStore sets the durable cross-thread store.
func WithStore(store dastore.Store) Option {
	return optionFunc(func(config *agentConfig) { config.Store = store })
}

// WithCache sets the runtime cache.
func WithCache(cache dacache.Cache) Option {
	return optionFunc(func(config *agentConfig) { config.Cache = cache })
}

// WithDependencies sets construction-time runtime dependencies.
func WithDependencies(dependencies any) Option {
	return optionFunc(func(config *agentConfig) { config.Deps = dependencies })
}

// WithRecursionLimit sets the maximum graph steps for one invocation.
func WithRecursionLimit(limit int) Option {
	return optionFunc(func(config *agentConfig) { config.RecursionLimit = limit })
}

// WithMaxConcurrency sets the maximum concurrent graph tasks.
func WithMaxConcurrency(limit int) Option {
	return optionFunc(func(config *agentConfig) { config.MaxConcurrency = limit })
}

// WithFatalToolErrors makes operational tool failures terminate the invocation.
func WithFatalToolErrors() Option {
	return optionFunc(func(config *agentConfig) { config.FailOnToolError = true })
}

// WithMetadata sets invocation metadata used by tracing and evaluation adapters.
func WithMetadata(metadata map[string]json.RawMessage) Option {
	return optionFunc(func(config *agentConfig) { config.Metadata = metadata })
}

// WithTags sets invocation tags used by tracing and evaluation adapters.
func WithTags(tags ...string) Option {
	return optionFunc(func(config *agentConfig) { config.Tags = append([]string(nil), tags...) })
}

// WithDebug enables graph debug logging.
func WithDebug() Option {
	return optionFunc(func(config *agentConfig) { config.Debug = true })
}

// WithLogger routes enabled graph debug events to logger. An omitted logger
// preserves slog.Default behavior; a nil explicit logger is a static error.
func WithLogger(logger *slog.Logger) Option {
	if logger == nil {
		panic("create deep agent: logger is nil")
	}
	return optionFunc(func(config *agentConfig) { config.Logger = logger })
}

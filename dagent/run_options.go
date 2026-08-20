package dagent

import (
	"fmt"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
)

type runInput struct {
	config             dacheckpoint.Config
	messages           []damessage.Message
	state              dastate.Values
	resume             any
	deps               any
	configurable       map[string]any
	skipValueEvents    bool
	discardResultState bool
}

// RunOption configures one invocation, stream, or cancellation.
type RunOption interface {
	applyRun(*runInput)
}

type runOptionFunc func(*runInput)

func (option runOptionFunc) applyRun(input *runInput) { option(input) }

// Prompt adds one human message containing text.
func Prompt(text string) RunOption {
	return Messages([]damessage.Message{damessage.Human(text)})
}

// Messages adds model-visible conversation messages.
func Messages(messages []damessage.Message) RunOption {
	cloned := make([]damessage.Message, len(messages))
	for index := range messages {
		cloned[index] = messages[index].Clone()
	}
	return runOptionFunc(func(input *runInput) {
		input.messages = append(input.messages, cloned...)
	})
}

// Resume supplies the value that resumes an interrupted invocation.
func Resume(value any) RunOption {
	return runOptionFunc(func(input *runInput) { input.resume = value })
}

// OnThread selects the durable thread. An omitted thread uses "default".
func OnThread(threadID string) RunOption {
	return runOptionFunc(func(input *runInput) { input.config.ThreadID = threadID })
}

// FromCheckpoint selects a complete checkpoint location.
func FromCheckpoint(config dacheckpoint.Config) RunOption {
	return runOptionFunc(func(input *runInput) { input.config = config })
}

// WithState supplies application-owned state updates.
func WithState(state dastate.Values) RunOption {
	cloned := state.Clone()
	return runOptionFunc(func(input *runInput) {
		if input.state == nil {
			input.state = dastate.Values{}
		}
		for key, value := range cloned {
			input.state[key] = value
		}
	})
}

// WithDeps overrides construction-time dependencies for this invocation.
func WithDeps(deps any) RunOption {
	return runOptionFunc(func(input *runInput) { input.deps = deps })
}

// WithConfigurable supplies immutable, runtime-only application settings.
func WithConfigurable(values map[string]any) RunOption {
	cloned := cloneConfigurable(values)
	return runOptionFunc(func(input *runInput) {
		if input.configurable == nil {
			input.configurable = map[string]any{}
		}
		for key, value := range cloned {
			input.configurable[key] = value
		}
	})
}

// WithoutValueEvents suppresses value snapshots from the execution stream.
func WithoutValueEvents() RunOption {
	return runOptionFunc(func(input *runInput) { input.skipValueEvents = true })
}

// WithoutResultState omits messages and state from the final result.
func WithoutResultState() RunOption {
	return runOptionFunc(func(input *runInput) { input.discardResultState = true })
}

func resolveRunOptions(options []RunOption) runInput {
	input := runInput{}
	for index, option := range options {
		if option == nil {
			panic(fmt.Sprintf("agent run option %d is nil", index))
		}
		option.applyRun(&input)
	}
	if input.config.ThreadID == "" {
		input.config.ThreadID = "default"
	}
	return input
}

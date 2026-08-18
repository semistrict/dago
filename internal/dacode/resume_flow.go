package dacode

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/semistrict/dago/damessage"
)

const (
	defaultCompactResumeThreshold = 400_000
	maximumResumeThreadIDBytes    = 512
	maximumResumePathBytes        = 4096
)

type sessionMetadataReader interface {
	SessionMetadata(context.Context, string) (sessionInfo, error)
}

type sessionResumePlanner interface {
	AgentName() string
	ListAgents(context.Context) ([]agentInfo, error)
}

type sessionLoader interface {
	LoadSession(context.Context, string) ([]damessage.Message, error)
}

type sessionCheckpointLoader interface {
	LoadSessionCheckpoint(context.Context, string, string) ([]damessage.Message, error)
}

type sessionResumeOptions struct {
	CurrentDirectory string
	CompactThreshold int
	AbortMode        cwdResumeAbortMode
}

func defaultSessionResumeOptions(currentDirectory string) sessionResumeOptions {
	return sessionResumeOptions{
		CurrentDirectory: currentDirectory,
		CompactThreshold: defaultCompactResumeThreshold,
	}
}

// configuredSessionResumeOptions is the exact application wiring seam: pass the
// resolved startup config and current absolute directory before selecting a
// thread. No process state is changed here.
func configuredSessionResumeOptions(config resolvedCLIConfig, currentDirectory string, abortMode cwdResumeAbortMode) sessionResumeOptions {
	return sessionResumeOptions{
		CurrentDirectory: currentDirectory,
		CompactThreshold: config.snapshot.Int("threads.compact_on_resume_threshold"),
		AbortMode:        abortMode,
	}
}

type sessionResumeStage uint8

const (
	sessionResumeReady sessionResumeStage = iota
	sessionResumeAgentPrompt
	sessionResumeCWDPrompt
	sessionResumeCompactPrompt
	sessionResumeCanceled
	sessionResumeLoaded
)

type sessionResumeDecision struct {
	ThreadID        string
	Agent           string
	Directory       string
	SwitchAgent     bool
	SwitchDirectory bool
	Compact         bool
}

type sessionResumePrompt struct {
	Agent   *agentResumePromptState
	CWD     *cwdResumePromptState
	Compact *compactResumePromptState
}

func (prompt sessionResumePrompt) Empty() bool {
	return prompt.Agent == nil && prompt.CWD == nil && prompt.Compact == nil
}

// sessionResumeController is a fail-closed, application-neutral pre-load gate.
// The application may inspect Prompt, apply a supported action, then obtain a
// decision only after every required trust prompt has been resolved.
type sessionResumeController struct {
	session          sessionInfo
	options          sessionResumeOptions
	currentAgent     string
	stage            sessionResumeStage
	switchAgent      bool
	switchDirectory  bool
	compact          bool
	cwdMismatch      bool
	agentMismatch    bool
	compactSuggested bool
}

func prepareSessionResume(ctx context.Context, runner sessionResumePlanner, threadID string, options sessionResumeOptions) (*sessionResumeController, error) {
	if runner == nil {
		return nil, errors.New("session resume runner is unavailable")
	}
	if err := validateResumeThreadID(threadID); err != nil {
		return nil, err
	}
	if options.CompactThreshold < 0 {
		return nil, errors.New("compact-on-resume threshold cannot be negative")
	}
	currentDirectory, err := normalizeResumeDirectory(options.CurrentDirectory, false)
	if err != nil {
		return nil, fmt.Errorf("current directory: %w", err)
	}
	options.CurrentDirectory = currentDirectory
	reader, ok := runner.(sessionMetadataReader)
	if !ok {
		return nil, errors.New("session runner does not expose pre-load metadata")
	}
	session, err := reader.SessionMetadata(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if session.ThreadID != threadID {
		return nil, errors.New("session metadata did not match the requested thread")
	}
	if session.CheckpointID == "" || len(session.CheckpointID) > maximumResumeThreadIDBytes || strings.ContainsRune(session.CheckpointID, 0) {
		return nil, errors.New("session metadata has an invalid checkpoint identity")
	}
	if session.Agent == "" {
		session.Agent = defaultAgentName
	}
	if err := validateAgentName(session.Agent); err != nil {
		return nil, fmt.Errorf("session agent: %w", err)
	}
	threadDirectory, err := normalizeResumeDirectory(session.Directory, true)
	if err != nil {
		return nil, fmt.Errorf("session directory: %w", err)
	}
	session.Directory = threadDirectory
	currentAgent := runner.AgentName()
	if currentAgent == "" {
		currentAgent = defaultAgentName
	}
	if err := validateAgentName(currentAgent); err != nil {
		return nil, fmt.Errorf("current agent: %w", err)
	}
	controller := &sessionResumeController{
		session: session, options: options, currentAgent: currentAgent,
		agentMismatch:    session.Agent != currentAgent,
		cwdMismatch:      directoriesDiffer(currentDirectory, threadDirectory),
		compactSuggested: options.CompactThreshold > 0 && session.ContextTokens > options.CompactThreshold,
	}
	if controller.agentMismatch {
		agents, err := runner.ListAgents(ctx)
		if err != nil {
			return nil, fmt.Errorf("discover session agent: %w", err)
		}
		available := false
		for _, candidate := range agents {
			if candidate.Name == session.Agent {
				available = true
				break
			}
		}
		if !available {
			return nil, fmt.Errorf("session agent %q is not available", session.Agent)
		}
	}
	controller.advance()
	return controller, nil
}

func (controller *sessionResumeController) Prompt() sessionResumePrompt {
	if controller == nil {
		return sessionResumePrompt{}
	}
	switch controller.stage {
	case sessionResumeAgentPrompt:
		return sessionResumePrompt{Agent: newAgentResumePrompt(controller.session.ThreadID, controller.currentAgent, controller.session.Agent)}
	case sessionResumeCWDPrompt:
		return sessionResumePrompt{CWD: newCWDResumePrompt(controller.options.CurrentDirectory, controller.session.Directory, true, controller.options.AbortMode)}
	case sessionResumeCompactPrompt:
		return sessionResumePrompt{Compact: newCompactResumePrompt(controller.session.ContextTokens, controller.options.CompactThreshold)}
	default:
		return sessionResumePrompt{}
	}
}

func (controller *sessionResumeController) Apply(action resumePromptAction) error {
	if controller == nil {
		return errors.New("session resume controller is unavailable")
	}
	switch controller.stage {
	case sessionResumeAgentPrompt:
		switch action {
		case resumePromptSwitchAgent:
			controller.switchAgent = true
			controller.agentMismatch = false
		case resumePromptCancelAgent:
			controller.stage = sessionResumeCanceled
			return nil
		default:
			return errors.New("action does not resolve the agent resume prompt")
		}
	case sessionResumeCWDPrompt:
		switch action {
		case resumePromptSwitchCWD:
			controller.switchDirectory = true
			controller.cwdMismatch = false
		case resumePromptStayCWD:
			controller.cwdMismatch = false
		case resumePromptAbort:
			if controller.options.AbortMode == cwdResumeAbortNone {
				return errors.New("abort is not available for this resume prompt")
			}
			controller.stage = sessionResumeCanceled
			return nil
		default:
			return errors.New("action does not resolve the directory resume prompt")
		}
	case sessionResumeCompactPrompt:
		switch action {
		case resumePromptCompactNow:
			controller.compact = true
			controller.compactSuggested = false
		case resumePromptKeepContext:
			controller.compactSuggested = false
		default:
			return errors.New("action does not resolve the compact resume prompt")
		}
	default:
		return errors.New("session resume controller is not awaiting a prompt")
	}
	controller.advance()
	return nil
}

func (controller *sessionResumeController) Decision() (sessionResumeDecision, bool) {
	if controller == nil || controller.stage != sessionResumeReady {
		return sessionResumeDecision{}, false
	}
	directory := controller.options.CurrentDirectory
	if controller.switchDirectory {
		directory = controller.session.Directory
	}
	return sessionResumeDecision{
		ThreadID: controller.session.ThreadID, Agent: controller.session.Agent, Directory: directory,
		SwitchAgent: controller.switchAgent, SwitchDirectory: controller.switchDirectory, Compact: controller.compact,
	}, true
}

func (controller *sessionResumeController) Canceled() bool {
	return controller != nil && controller.stage == sessionResumeCanceled
}

func loadPreparedSession(ctx context.Context, loader sessionLoader, controller *sessionResumeController) (sessionInfo, []damessage.Message, error) {
	if loader == nil {
		return sessionInfo{}, nil, errors.New("session loader is unavailable")
	}
	if controller == nil || controller.stage != sessionResumeReady {
		return sessionInfo{}, nil, errors.New("session resume decisions are incomplete")
	}
	reader, ok := loader.(sessionMetadataReader)
	if !ok {
		return sessionInfo{}, nil, errors.New("session loader does not expose pre-load metadata")
	}
	current, err := reader.SessionMetadata(ctx, controller.session.ThreadID)
	if err != nil {
		return sessionInfo{}, nil, err
	}
	if current.ThreadID != controller.session.ThreadID || current.CheckpointID != controller.session.CheckpointID {
		return sessionInfo{}, nil, errors.New("session changed while resume decisions were pending")
	}
	exactLoader, ok := loader.(sessionCheckpointLoader)
	if !ok {
		return sessionInfo{}, nil, errors.New("session loader does not support exact-checkpoint loading")
	}
	messages, err := exactLoader.LoadSessionCheckpoint(ctx, controller.session.ThreadID, controller.session.CheckpointID)
	if err != nil {
		return sessionInfo{}, nil, err
	}
	controller.stage = sessionResumeLoaded
	return controller.session, messages, nil
}

func (controller *sessionResumeController) advance() {
	switch {
	case controller.agentMismatch:
		controller.stage = sessionResumeAgentPrompt
	case controller.cwdMismatch:
		controller.stage = sessionResumeCWDPrompt
	case controller.compactSuggested:
		controller.stage = sessionResumeCompactPrompt
	default:
		controller.stage = sessionResumeReady
	}
}

func validateResumeThreadID(threadID string) error {
	if strings.TrimSpace(threadID) == "" || len(threadID) > maximumResumeThreadIDBytes || strings.ContainsRune(threadID, 0) {
		return errors.New("resume thread id is invalid")
	}
	return nil
}

func normalizeResumeDirectory(directory string, optional bool) (string, error) {
	if directory == "" && optional {
		return "", nil
	}
	if directory == "" || len(directory) > maximumResumePathBytes || strings.ContainsRune(directory, 0) || !filepath.IsAbs(directory) {
		return "", errors.New("path must be a bounded absolute path")
	}
	cleaned := filepath.Clean(directory)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved), nil
	}
	return cleaned, nil
}

func directoriesDiffer(current, stored string) bool {
	if stored == "" {
		return false
	}
	if current == stored {
		return false
	}
	currentResolved, currentErr := filepath.EvalSymlinks(current)
	storedResolved, storedErr := filepath.EvalSymlinks(stored)
	return currentErr != nil || storedErr != nil || currentResolved != storedResolved
}

func sessionContextTokens(state map[string]any, messages []damessage.Message) int {
	if value, ok := state[sessionContextTokensKey]; ok {
		switch typed := value.(type) {
		case int:
			return max(typed, 0)
		case int64:
			if typed > int64(^uint(0)>>1) {
				return int(^uint(0) >> 1)
			}
			return max(int(typed), 0)
		case float64:
			if typed >= 0 && typed <= float64(^uint(0)>>1) {
				return int(typed)
			}
		}
	}
	return contextTokensFromMessages(messages)
}

func contextTokensFromMessages(messages []damessage.Message) int {
	usage, ok := damessage.LastUsage(messages)
	if !ok {
		return 0
	}
	tokens := usage.InputTokens + usage.OutputTokens
	if tokens <= 0 {
		tokens = usage.TotalTokens
	}
	return max(tokens, 0)
}

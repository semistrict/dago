package dacode

import (
	"context"
	"net/http"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/datalon/tracing"
)

const maximumDeferredTraceResults = 16

type traceResolvedMsg struct {
	generation uint64
	result     traceCommandResult
}

type traceBrowserOpenedMsg struct{ failed bool }

func (model *tuiModel) configureTrace(command *traceCommand, project string) {
	if command == nil {
		command = newTraceCommand(nil)
	}
	model.traceCommand = command
	model.traceProject = boundedTraceValue(project, 256)
}

func (model *tuiModel) startTraceCommand() tea.Cmd {
	if model.traceCommand == nil {
		model.configureTrace(nil, "")
	}
	model.traceGeneration++
	if model.traceGeneration == 0 {
		model.traceGeneration = 1
	}
	generation := model.traceGeneration
	command := model.traceCommand
	request := traceCommandRequest{Project: model.traceProject, ThreadID: model.threadID, HasMessages: model.traceHasMessages()}
	return func() tea.Msg {
		return traceResolvedMsg{generation: generation, result: command.resolve(model.ctx, request)}
	}
}

func (model *tuiModel) handleTraceResolved(message traceResolvedMsg) tea.Cmd {
	if message.generation == 0 || message.generation > model.traceGeneration {
		return nil
	}
	open := model.openTraceURL(message.result.URL)
	if model.running {
		if len(model.deferredTrace) == maximumDeferredTraceResults {
			copy(model.deferredTrace, model.deferredTrace[1:])
			model.deferredTrace[len(model.deferredTrace)-1] = message.result
		} else {
			model.deferredTrace = append(model.deferredTrace, message.result)
		}
		return open
	}
	model.appendTraceResult(message.result)
	return open
}

func (model *tuiModel) appendTraceResult(result traceCommandResult) {
	model.appendItem(transcriptItem{kind: itemUser, text: "/trace"})
	model.appendItem(transcriptItem{kind: itemNotice, text: result.Message})
	model.refreshTranscript()
}

func (model *tuiModel) flushDeferredTrace() {
	for _, result := range model.deferredTrace {
		model.appendTraceResult(result)
	}
	clear(model.deferredTrace)
	model.deferredTrace = nil
}

func (model *tuiModel) traceHasMessages() bool {
	for _, item := range model.items {
		if item.kind == itemUser && !strings.HasPrefix(strings.TrimSpace(item.text), "/") {
			return true
		}
	}
	return false
}

func (model *tuiModel) openTraceURL(value string) tea.Cmd {
	if value == "" {
		return nil
	}
	if model.browserLinks {
		return model.stageTerminalSequences("", browserOpenURLSequence(value))
	}
	opener := model.openURL
	return func() tea.Msg {
		if opener == nil {
			return traceBrowserOpenedMsg{failed: true}
		}
		return traceBrowserOpenedMsg{failed: opener(value) != nil}
	}
}

func configuredTraceTUI(ctx context.Context, store *dacredential.Store, lookup dacredential.EnvironmentLookup) (*traceCommand, string) {
	if ctx == nil || store == nil || lookup == nil {
		panic("dacode: trace TUI configuration dependencies are required")
	}
	resolution, err := store.Resolve(ctx, "langsmith", lookup)
	if err != nil || !resolution.Configured || resolution.Credential.APIKey == nil {
		return newTraceCommand(nil), ""
	}
	credential := resolution.Credential.APIKey
	apiEndpoint := strings.TrimRight(strings.TrimSpace(credential.BaseURL), "/")
	if apiEndpoint == "" {
		apiEndpoint = "https://api.smith.langchain.com"
	}
	if apiEndpoint != "https://api.smith.langchain.com" && apiEndpoint != "https://eu.api.smith.langchain.com" {
		return newTraceCommand(nil), ""
	}
	project := strings.TrimSpace(credential.Project)
	if project == "" {
		if value, exists := lookup("DEEPAGENTS_CODE_LANGSMITH_PROJECT"); exists {
			project = strings.TrimSpace(value)
		} else if value, exists := lookup("LANGSMITH_PROJECT"); exists {
			project = strings.TrimSpace(value)
		}
	}
	if project == "" {
		project = "deepagents-code"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	projectLookup := newLangSmithProjectLookup(client, apiEndpoint, "https://smith.langchain.com", credential.Key, langSmithProjectLookupOptions{})
	resolver := tracing.NewURLResolver(projectLookup, tracing.URLResolverOptions{})
	return newTraceCommand(resolver), project
}

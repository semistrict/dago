package dacode

import (
	"context"
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/semistrict/dago/daproviders/modelconfig"
)

type modelPreferenceController interface {
	Default(context.Context) (string, error)
	Recent(context.Context) (string, error)
	SetDefault(context.Context, string) error
	ClearDefault(context.Context) (bool, error)
	SetRecent(context.Context, string) error
}

type modelPreferenceMsg struct {
	action           string
	spec             string
	write            modelPreferenceWrite
	recentGeneration uint64
	superseded       bool
	err              error
}

// modelPreferenceSequencer binds persistent writes to UI intent order. A
// command that starts late cannot overwrite a newer choice, while a write
// already in progress completes before the newer write is committed.
type modelPreferenceSequencer struct {
	mu        sync.Mutex
	writeLock chan struct{}
	latest    map[string]uint64
}

func newModelPreferenceSequencer() *modelPreferenceSequencer {
	writeLock := make(chan struct{}, 1)
	writeLock <- struct{}{}
	return &modelPreferenceSequencer{writeLock: writeLock, latest: make(map[string]uint64)}
}

func (sequence *modelPreferenceSequencer) begin(key string) uint64 {
	sequence.mu.Lock()
	defer sequence.mu.Unlock()
	sequence.latest[key]++
	return sequence.latest[key]
}

func (sequence *modelPreferenceSequencer) apply(ctx context.Context, key string, generation uint64, write func() error) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-sequence.writeLock:
	}
	defer func() { sequence.writeLock <- struct{}{} }()
	sequence.mu.Lock()
	current := generation != 0 && sequence.latest[key] == generation
	sequence.mu.Unlock()
	if !current {
		return false, nil
	}
	return true, write()
}

func (model *tuiModel) preferenceSequencer() *modelPreferenceSequencer {
	if model.modelPreferenceSequence == nil {
		model.modelPreferenceSequence = newModelPreferenceSequencer()
	}
	return model.modelPreferenceSequence
}

type deferredModelSelectionMsg struct{ Action deferredModelSelectorAction }

func (model *tuiModel) configureModelProviderAvailability(ctx context.Context, resolver *modelconfig.Resolver, oauthConfigured bool) error {
	if resolver == nil {
		model.modelProviderAvailability = nil
		return nil
	}
	statuses, err := resolver.Statuses(ctx)
	if err != nil {
		return err
	}
	availability := make(map[string]modelProviderAvailability, len(statuses))
	for _, status := range statuses {
		install := modelRequirementMissing
		if status.FactoryAvailable {
			install = modelRequirementReady
		}
		credentials := modelRequirementMissing
		if status.Configured {
			credentials = modelRequirementReady
		}
		if status.Authentication == modelconfig.AuthenticationAmbient || status.Authentication == modelconfig.AuthenticationOptional {
			credentials = modelRequirementNotRequired
		}
		availability[status.Provider] = modelProviderAvailability{Install: install, Credentials: credentials}
	}
	oauth := availability["openai_oauth"]
	oauth.Install = modelRequirementReady
	if oauthConfigured {
		oauth.Credentials = modelRequirementReady
	}
	availability["openai_oauth"] = oauth
	model.modelProviderAvailability = availability
	return nil
}

func (model *tuiModel) refreshModelCredentialAvailability(rows []authManagerRow) {
	if len(model.modelProviderAvailability) == 0 {
		return
	}
	for _, row := range rows {
		status, exists := model.modelProviderAvailability[row.provider]
		if !exists || status.Credentials == modelRequirementNotRequired {
			continue
		}
		status.Credentials = modelRequirementMissing
		if row.configured {
			status.Credentials = modelRequirementReady
		}
		model.modelProviderAvailability[row.provider] = status
	}
}

func (model *tuiModel) configureModelPreferences(preferences modelPreferenceController) error {
	if preferences == nil {
		return nil
	}
	defaultSpec, defaultErr := preferences.Default(model.ctx)
	recentSpec, recentErr := preferences.Recent(model.ctx)
	if defaultErr != nil {
		return defaultErr
	}
	if recentErr != nil {
		return recentErr
	}
	model.modelPreferences = preferences
	model.modelDefaultSpec = validModelSelectorSpec(defaultSpec)
	if recent := validModelSelectorSpec(recentSpec); recent != "" {
		model.modelRecentSpecs = []string{recent}
	}
	return nil
}

func (model *tuiModel) openModelSelector() {
	model.modelSelector = newModelSelectorWithOptions(
		modelSelectorCatalog(model.modelRecentSpecs), model.modelName, model.modelDefaultSpec,
		modelSelectorOptions{ProviderAvailability: model.modelProviderAvailability},
	)
}

func (model *tuiModel) handleModelSelectorKey(key string) tea.Cmd {
	if (key == "esc" || key == "ctrl+c") && model.modelSelector != nil && model.modelSelector.pendingWrite != nil {
		return model.notify("Wait for the model preference save to finish.", toastWarning, "")
	}
	result := model.modelSelector.handleKey(key, max(model.height-11, 3))
	return model.applyModelSelectorResult(result)
}

func (model *tuiModel) applyDeferredModelSelectorAction(action deferredModelSelectorAction) tea.Cmd {
	if model.deferredModelSelector == nil || !model.deferredModelSelector.acceptsRequest(action.Request) {
		return nil
	}
	model.modelSelector = model.deferredModelSelector
	model.deferredModelSelector = nil
	command := model.applyModelSelectorResult(modelSelectorResult{
		Action: action.Request.Action, Spec: action.Request.Spec, Request: action.Request,
	})
	if action.Request.Action == modelSelectorSetDefault || action.Request.Action == modelSelectorClearDefault {
		model.deferredModelSelector = model.modelSelector
		model.modelSelector = nil
	}
	return command
}

func (model *tuiModel) applyModelSelectorResult(result modelSelectorResult) tea.Cmd {
	if result.Err != nil {
		return model.notify(result.Err.Error(), toastWarning, "")
	}
	switch result.Action {
	case modelSelectorCancel:
		model.modelSelector = nil
	case modelSelectorSelect:
		if model.interactionBusy() {
			return model.deferModelSelectorResult(result)
		}
		model.modelSelector = nil
		return model.selectRuntimeModel(result.Spec)
	case modelSelectorSetDefault:
		if model.interactionBusy() {
			return model.deferModelSelectorResult(result)
		}
		if model.modelPreferences == nil {
			return model.notify("Default model storage is unavailable.", toastWarning, "")
		}
		write, accepted := model.modelSelector.beginPreferenceWrite(result.Request)
		if !accepted {
			return nil
		}
		model.modelDefaultSpec = write.Next
		preferences := model.modelPreferences
		sequence := model.preferenceSequencer()
		generation := sequence.begin("default")
		return func() tea.Msg {
			applied, err := sequence.apply(model.ctx, "default", generation, func() error { return preferences.SetDefault(model.ctx, result.Spec) })
			return modelPreferenceMsg{action: "default", spec: result.Spec, write: write, superseded: !applied && err == nil, err: err}
		}
	case modelSelectorClearDefault:
		if model.interactionBusy() {
			return model.deferModelSelectorResult(result)
		}
		if model.modelPreferences == nil {
			return model.notify("Default model storage is unavailable.", toastWarning, "")
		}
		write, accepted := model.modelSelector.beginPreferenceWrite(result.Request)
		if !accepted {
			return nil
		}
		model.modelDefaultSpec = write.Next
		preferences := model.modelPreferences
		sequence := model.preferenceSequencer()
		generation := sequence.begin("default")
		return func() tea.Msg {
			applied, err := sequence.apply(model.ctx, "default", generation, func() error {
				_, clearErr := preferences.ClearDefault(model.ctx)
				return clearErr
			})
			return modelPreferenceMsg{action: "clear-default", spec: result.Spec, write: write, superseded: !applied && err == nil, err: err}
		}
	}
	return nil
}

func (model *tuiModel) deferModelSelectorResult(result modelSelectorResult) tea.Cmd {
	action, ok := result.deferredAction()
	if !ok || model.modelSelector == nil || !model.modelSelector.acceptsRequest(action.Request) {
		return model.notify("The model selection expired.", toastWarning, "")
	}
	model.deferredModelSelector = model.modelSelector
	model.modelSelector = nil
	payload := deferredActionPayload{
		Identity:   strconv.FormatUint(action.Request.SelectorID, 10),
		Generation: action.Request.Generation,
		Arguments:  []string{strconv.Itoa(int(action.Request.Action)), action.Request.Spec, action.Request.PriorDefault},
	}
	model.deferredActions.deferAction(deferredAction{
		Kind:    deferredModelSwitch,
		Payload: payload,
		ExecutePayload: func(payload deferredActionPayload) tea.Msg {
			selectorID, _ := strconv.ParseUint(payload.Identity, 10, 64)
			actionValue, _ := strconv.Atoi(payload.Arguments[0])
			return deferredModelSelectionMsg{Action: deferredModelSelectorAction{Request: modelSelectorRequest{
				Action: modelSelectorAction(actionValue), Spec: payload.Arguments[1], PriorDefault: payload.Arguments[2],
				SelectorID: selectorID, Generation: payload.Generation,
			}}}
		},
	})
	return model.notify("Model change queued for the next idle point.", toastInfo, "")
}

func (state *modelSelectorState) acceptsRequest(request modelSelectorRequest) bool {
	if state == nil || request.SelectorID != state.selectorID || request.Generation != state.generation ||
		validModelSelectorSpec(request.Spec) != request.Spec {
		return false
	}
	switch request.Action {
	case modelSelectorSelect:
		return true
	case modelSelectorSetDefault, modelSelectorClearDefault:
		return request.PriorDefault == state.defaultSpec && state.pendingWrite == nil
	default:
		return false
	}
}

// finishModelPreference is the generation-aware message integration seam. The
// app update loop routes modelPreferenceMsg here so failures roll back the
// optimistic default and stale completions are ignored.
func (model *tuiModel) finishModelPreference(message modelPreferenceMsg) tea.Cmd {
	if message.superseded {
		for _, selector := range []*modelSelectorState{model.modelSelector, model.deferredModelSelector} {
			if selector != nil && selector.pendingWrite != nil && *selector.pendingWrite == message.write {
				selector.replaceDefault(model.modelDefaultSpec)
				if selector == model.deferredModelSelector {
					model.deferredModelSelector = nil
				}
				break
			}
		}
		return nil
	}
	if message.action == "default" || message.action == "clear-default" {
		selector := model.modelSelector
		if selector == nil {
			selector = model.deferredModelSelector
		}
		if selector == nil || !selector.finishPreferenceWrite(message.write, message.err) {
			return nil
		}
		if selector == model.deferredModelSelector {
			model.deferredModelSelector = nil
		}
		if message.err != nil {
			model.modelDefaultSpec = message.write.Previous
			return model.notify("Model preference could not be saved.", toastError, "")
		}
		model.modelDefaultSpec = message.write.Next
	}
	if message.action == "recent" && message.recentGeneration != model.modelRecentGeneration {
		return nil
	}
	if message.err != nil {
		return model.notify("Model preference could not be saved.", toastError, "")
	}
	label := "Recent model saved."
	if message.action == "default" {
		label = "Default model set to " + message.spec + "."
	} else if message.action == "clear-default" {
		label = "Default model cleared."
	}
	return model.notify(label, toastInfo, "")
}

func (model *tuiModel) selectRuntimeModel(spec string) tea.Cmd {
	spec = validModelSelectorSpec(strings.TrimSpace(spec))
	if spec == "" {
		return model.notify("Enter a valid provider:model selection.", toastWarning, "")
	}
	model.modelName = spec
	model.modelRecentSpecs = append([]string{spec}, removeModelSpec(model.modelRecentSpecs, spec)...)
	if len(model.modelRecentSpecs) > 16 {
		model.modelRecentSpecs = model.modelRecentSpecs[:16]
	}
	model.appendItem(transcriptItem{kind: itemNotice, text: "Model set to " + spec + ". The selection applies to the next model request."})
	model.refreshTranscript()
	if model.modelPreferences == nil {
		return nil
	}
	model.modelRecentGeneration++
	generation := model.modelRecentGeneration
	preferences := model.modelPreferences
	sequence := model.preferenceSequencer()
	preferenceGeneration := sequence.begin("recent")
	return func() tea.Msg {
		applied, err := sequence.apply(model.ctx, "recent", preferenceGeneration, func() error { return preferences.SetRecent(model.ctx, spec) })
		return modelPreferenceMsg{
			action: "recent", spec: spec, recentGeneration: generation,
			superseded: !applied && err == nil, err: err,
		}
	}
}

func removeModelSpec(specs []string, target string) []string {
	result := make([]string, 0, len(specs))
	for _, spec := range specs {
		if spec != target {
			result = append(result, spec)
		}
	}
	return result
}

package dacode

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type autoClassifierValidatedMsg struct {
	generation uint64
	spec       string
	persist    bool
	validation autoClassifierValidation
	err        error
}

type autoClassifierPreferenceMsg struct {
	notice string
	err    error
}

func (model *tuiModel) configureAutoClassifier(mainModel, startupModel string, preferences autoClassifierPreferenceController) {
	model.autoClassifier = newAutoClassifierState(mainModel, startupModel)
	model.autoClassifierPreferences = preferences
}

func (model *tuiModel) autoClassifierContext() autoClassifierContext {
	if model.autoClassifier == nil {
		return autoClassifierContext{}
	}
	return model.autoClassifier.contextValue()
}

func (model *tuiModel) autoClassifierCommand(command string) tea.Cmd {
	if model.autoClassifier == nil {
		model.autoClassifier = newAutoClassifierState(model.modelName, defaultReviewModel)
	}
	action := model.autoClassifier.handleCommand(command)
	switch action.Kind {
	case autoClassifierActivateAuto:
		return model.setApprovalMode(approvalAuto)
	case autoClassifierOpenSelector:
		model.openAutoClassifierSelector()
		return nil
	case autoClassifierValidate:
		return model.validateAutoClassifier(action)
	case autoClassifierApply:
		model.appendItem(transcriptItem{kind: itemNotice, text: model.autoClassifier.notice})
		model.refreshTranscript()
		return nil
	case autoClassifierShowUsage:
		model.appendItem(transcriptItem{kind: itemNotice, text: renderAutoClassifierUsage(model.autoClassifier, max(model.width-4, 24), model.charset == charsetASCII)})
		model.refreshTranscript()
		return nil
	default:
		if model.autoClassifier.notice != "" {
			model.appendItem(transcriptItem{kind: itemNotice, text: model.autoClassifier.notice})
			model.refreshTranscript()
		}
		return nil
	}
}

func (model *tuiModel) openAutoClassifierSelector() {
	candidates := autoClassifierCandidates()
	entries := make([]modelSelectorEntry, 0, len(candidates))
	for _, candidate := range candidates {
		entries = append(entries, modelSelectorEntry{Spec: candidate.Spec, Label: candidate.Label, Recommended: true})
	}
	current, defaultSpec := model.autoClassifier.selectorCurrent(), model.autoClassifier.startupModel
	model.modelSelector = newNamedModelSelector(entries, current, defaultSpec, "Choose Auto Classifier Model")
	model.autoClassifierSelector = true
}

func (model *tuiModel) handleAutoClassifierSelectorKey(key string) tea.Cmd {
	if model.modelSelector == nil {
		model.autoClassifierSelector = false
		return nil
	}
	result := model.modelSelector.handleKey(key, max(model.height-11, 3))
	if result.Err != nil {
		return model.notify(result.Err.Error(), toastWarning, "")
	}
	switch result.Action {
	case modelSelectorCancel:
		model.modelSelector, model.autoClassifierSelector = nil, false
	case modelSelectorSelect, modelSelectorSetDefault:
		persist := result.Action == modelSelectorSetDefault
		model.modelSelector, model.autoClassifierSelector = nil, false
		return model.validateAutoClassifier(model.autoClassifier.beginSelection(result.Spec, persist))
	case modelSelectorClearDefault:
		model.modelSelector, model.autoClassifierSelector = nil, false
		if model.autoClassifierPreferences == nil {
			return model.notify("Classifier preference storage is unavailable.", toastWarning, "")
		}
		preferences := model.autoClassifierPreferences
		return func() tea.Msg {
			return autoClassifierPreferenceMsg{notice: "Classifier default cleared.", err: preferences.Clear(model.ctx)}
		}
	}
	return nil
}

func (model *tuiModel) validateAutoClassifier(action autoClassifierAction) tea.Cmd {
	if action.Kind != autoClassifierValidate {
		if model.autoClassifier != nil && model.autoClassifier.notice != "" {
			return model.notify(model.autoClassifier.notice, toastWarning, "")
		}
		return nil
	}
	runner, ok := model.runner.(autoClassifierRunner)
	if !ok {
		model.autoClassifier.setNotice("Classifier model validation is unavailable.", true)
		return model.notify(model.autoClassifier.notice, toastWarning, "")
	}
	model.autoClassifierValidationGeneration++
	generation := model.autoClassifierValidationGeneration
	ctx, cancel := context.WithTimeout(model.ctx, 30*time.Second)
	return func() tea.Msg {
		defer cancel()
		validation, err := runner.ValidateAutoClassifier(ctx, action.Spec)
		return autoClassifierValidatedMsg{generation: generation, spec: action.Spec, persist: action.Persist, validation: validation, err: err}
	}
}

func (model *tuiModel) finishAutoClassifierValidation(message autoClassifierValidatedMsg) tea.Cmd {
	if model.autoClassifier == nil {
		return nil
	}
	if message.generation == 0 || message.generation != model.autoClassifierValidationGeneration {
		return nil
	}
	if message.err != nil {
		if errors.Is(message.err, context.Canceled) {
			model.autoClassifier.setNotice("Classifier validation was cancelled.", true)
		} else {
			model.autoClassifier.setNotice("Classifier validation failed; the active reviewer was not changed.", true)
		}
		model.appendItem(transcriptItem{kind: itemNotice, text: model.autoClassifier.notice})
		model.refreshTranscript()
		return model.notify(model.autoClassifier.notice, toastWarning, "")
	}
	action := model.autoClassifier.completeValidation(message.spec, message.validation)
	if action.Kind != autoClassifierApply {
		model.appendItem(transcriptItem{kind: itemNotice, text: model.autoClassifier.notice})
		model.refreshTranscript()
		return model.notify(model.autoClassifier.notice, toastWarning, "")
	}
	model.appendItem(transcriptItem{kind: itemNotice, text: model.autoClassifier.notice})
	model.refreshTranscript()
	if !message.persist {
		return nil
	}
	if model.autoClassifierPreferences == nil {
		return model.notify("Classifier preference storage is unavailable; selection is session-only.", toastWarning, "")
	}
	preferences := model.autoClassifierPreferences
	spec := strings.TrimSpace(message.spec)
	return func() tea.Msg {
		return autoClassifierPreferenceMsg{notice: "Classifier default saved.", err: preferences.Set(model.ctx, spec)}
	}
}

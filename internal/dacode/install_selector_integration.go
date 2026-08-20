package dacode

import (
	"context"
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/semistrict/dago/dainstall"
)

type installController interface {
	Available(dainstall.Kind) []dainstall.Entry
	Install(context.Context, dainstall.Kind, string, dainstall.Authorization) (dainstall.Result, error)
}

type installCompletionAction uint8

const (
	installCompletionNoAction installCompletionAction = iota
	installCompletionRestartRequired
)

type installCompletedMsg struct {
	request installRequest
	result  dainstall.Result
	action  installCompletionAction
	err     error
}

func (model *tuiModel) openInstallSelector(arguments string) {
	model.installSelector = newInstallSelectorFromController(model.installController)
	parsed, err := parseInstallSelectorArguments(strings.TrimSpace(arguments))
	if err != nil {
		model.installSelector.setArgumentError(err)
		return
	}
	model.installSelector.applyArguments(parsed)
}

func (model *tuiModel) handleInstallSelectorKey(key string) tea.Cmd {
	result := model.installSelector.handleKey(key, max(model.height-11, 3))
	switch result.Action {
	case installSelectorCancel:
		model.installSelector = nil
	case installSelectorAlreadyAvailable:
		model.installSelector = nil
		model.appendItem(transcriptItem{kind: itemNotice, text: result.Entry.Name + " is already included in this build."})
		model.refreshTranscript()
	case installSelectorInstall:
		if model.installController == nil {
			return model.notify("The integration installer is unavailable.", toastError, "")
		}
		if _, authorized := model.installSelector.entryForRequest(result.Request); !authorized {
			return model.notify("The integration install selection expired.", toastError, "")
		}
		if model.installPending != nil {
			return model.notify("An integration installation is already running.", toastWarning, "")
		}
		request := result.Request
		model.installPending = &request
		model.installSelector = nil
		controller := model.installController
		return executeInstallRequest(model.ctx, controller, request)
	}
	return nil
}

func newInstallSelectorFromController(controller installController) *installSelectorState {
	if controller == nil {
		return newInstallSelector()
	}
	entries := make([]installSelectorEntry, 0, 32)
	for _, kind := range []dainstall.Kind{dainstall.Extra, dainstall.Package} {
		for _, entry := range controller.Available(kind) {
			if len(entries) == 256 {
				break
			}
			if entry.Kind != kind {
				continue
			}
			entries = append(entries, installSelectorEntry{
				Name: entry.Name, Kind: entry.Kind, Description: entry.Description, BuiltIn: entry.BuiltIn,
			})
		}
	}
	return newInstallSelectorWithEntries(entries)
}

func executeInstallRequest(ctx context.Context, controller installController, request installRequest) tea.Cmd {
	return func() tea.Msg {
		if controller == nil || request.SelectorID == 0 || request.Generation == 0 ||
			!validInstallSelectorName(request.Name) || (request.Kind != dainstall.Extra && request.Kind != dainstall.Package) {
			return installCompletedMsg{request: request, err: boundedInstallFailure(errors.New("integration install request is invalid"))}
		}
		installed, err := controller.Install(ctx, request.Kind, request.Name, dainstall.AuthorizationGranted)
		message := installCompletedMsg{request: request, result: installed, err: boundedInstallFailure(err)}
		if err == nil && installed.Status == dainstall.Installed {
			message.action = installCompletionRestartRequired
		}
		return message
	}
}

type installFailure struct {
	message string
	cause   error
}

func (failure installFailure) Error() string { return failure.message }
func (failure installFailure) Unwrap() error { return failure.cause }

func boundedInstallFailure(err error) error {
	if err == nil {
		return nil
	}
	message := boundedTerminalText(err.Error(), 320)
	if message == "" {
		message = "integration installation failed"
	}
	return installFailure{message: message, cause: err}
}

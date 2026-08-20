package dacode

import tea "charm.land/bubbletea/v2"

// programModel isolates Bubble Tea's declarative terminal view from the
// application-owned string renderer. Keeping that seam narrow lets tests and
// non-terminal render consumers inspect the exact content without recreating
// terminal capabilities.
type programModel struct {
	model *tuiModel
}

func newProgramModel(model *tuiModel) programModel {
	if model == nil {
		panic("dacode: initialized TUI model is required")
	}
	return programModel{model: model}
}

func (model programModel) Init() tea.Cmd {
	return model.model.Init()
}

func (model programModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	updated, command := model.model.Update(message)
	model.model = updated
	return model, command
}

func (model programModel) View() tea.View {
	view := tea.NewView(model.model.View())
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.ReportFocus = true
	return view
}

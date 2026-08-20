package dacode

import tea "charm.land/bubbletea/v2"

func testTextKey(text string) tea.KeyPressMsg {
	message := tea.KeyPressMsg{Text: text}
	runes := []rune(text)
	if len(runes) == 1 {
		message.Code = runes[0]
	}
	return message
}

func testCtrlKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl}
}

func testShiftKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: tea.ModShift}
}

package dacode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
)

func approvalWidgetRequest(id, name string, arguments any) dagent.ApprovalRequest {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		panic(err)
	}
	return dagent.ApprovalRequest{Call: damessage.ToolCall{ID: id, Name: name, Arguments: encoded}}
}

func TestApprovalToolRenderersShowSpecificBoundedPreviews(t *testing.T) {
	longCommand := strings.Repeat("printf x; ", 30)
	tests := []struct {
		name     string
		request  dagent.ApprovalRequest
		contains []string
	}{
		{name: "execute", request: approvalWidgetRequest("1", "execute", map[string]any{"command": longCommand}), contains: []string{"$ printf x;", "press e to expand"}},
		{name: "write", request: approvalWidgetRequest("1", "write_file", map[string]any{"file_path": "/notes.md", "content": "first\nsecond"}), contains: []string{"Write file: /notes.md", "first", "second"}},
		{name: "edit", request: approvalWidgetRequest("1", "edit_file", map[string]any{"file_path": "/notes.md", "old_string": "old", "new_string": "new"}), contains: []string{"Edit file: /notes.md", "--- before", "-old", "+new"}},
		{name: "delete", request: approvalWidgetRequest("1", "delete", map[string]any{"file_path": "/notes.md"}), contains: []string{"Delete: /notes.md"}},
		{name: "generic", request: approvalWidgetRequest("1", "mcp_publish", map[string]any{"channel": "stable", "force": true}), contains: []string{"channel: \"stable\"", "force: true"}},
		{name: "sensitive write", request: approvalWidgetRequest("1", "write_file", map[string]any{"file_path": "/.env.local", "content": "SECRET=not-for-scrollback"}), contains: []string{"Write file: /.env.local", "Contents hidden"}},
		{name: "sensitive edit", request: approvalWidgetRequest("1", "edit_file", map[string]any{"file_path": "/credentials.json", "old_string": "old-secret", "new_string": "new-secret"}), contains: []string{"Edit file: /credentials.json", "Contents hidden"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := newApprovalState([]dagent.ApprovalRequest{test.request})
			state.ready = true
			plain := ansi.Strip(renderApproval(state, 100))
			for _, want := range test.contains {
				if !strings.Contains(plain, want) {
					t.Fatalf("render missing %q:\n%s", want, plain)
				}
			}
			if strings.HasPrefix(test.name, "sensitive") && strings.Contains(plain, "secret") {
				t.Fatalf("sensitive content leaked:\n%s", plain)
			}
		})
	}

	state := newApprovalState([]dagent.ApprovalRequest{tests[0].request})
	state.ready = true
	state.commandExpanded = true
	plain := ansi.Strip(renderApproval(state, 100))
	if strings.Contains(plain, "press e to expand") || approvalCommandPreview(longCommand, true) != longCommand {
		t.Fatalf("expanded command not shown:\n%s", plain)
	}
}

func TestApprovalMenuNavigationNumericAndSemanticQuickKeys(t *testing.T) {
	for _, test := range []struct {
		name     string
		keys     []tea.KeyMsg
		decision dagent.ApprovalDecision
	}{
		{name: "enter approve", keys: []tea.KeyMsg{{Type: tea.KeyEnter}}, decision: dagent.ApprovalApprove},
		{name: "numeric reject", keys: []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune{'3'}}}, decision: dagent.ApprovalReject},
		{name: "navigate reject", keys: []tea.KeyMsg{{Type: tea.KeyDown}, {Type: tea.KeyRunes, Runes: []rune{'j'}}, {Type: tea.KeyEnter}}, decision: dagent.ApprovalReject},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}}}
			model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
			model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
			model.approval.ready = true
			for _, key := range test.keys {
				model.Update(key)
			}
			if len(runner.inputs) != 1 {
				t.Fatalf("resume inputs = %d", len(runner.inputs))
			}
			choice := runner.inputs[0].Resume.(dagent.ApprovalResponse).Decisions["call-1"]
			if choice.Decision != test.decision {
				t.Fatalf("decision = %s", choice.Decision)
			}
		})
	}
}

func TestApprovalAutoQuickKeyPersistsModeAndApprovesCurrentBatch(t *testing.T) {
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}}}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.ready = true
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if model.approvalMode != approvalAuto || len(runner.inputs) != 1 {
		t.Fatalf("mode=%s inputs=%d", model.approvalMode, len(runner.inputs))
	}
	choice := runner.inputs[0].Resume.(dagent.ApprovalResponse).Decisions["call-1"]
	if choice.Decision != dagent.ApprovalApprove {
		t.Fatalf("decision = %s", choice.Decision)
	}
}

func TestApprovalAutoFallbackOffersManualWithoutDeciding(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, true, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.ready = true
	model.approval.autoFallback = true
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if model.approvalMode != approvalManual || model.approval == nil {
		t.Fatalf("mode=%s approval=%#v", model.approvalMode, model.approval)
	}
	plain := ansi.Strip(renderApproval(model.approval, 90))
	if strings.Contains(plain, "Switch to Manual") {
		t.Fatalf("manual fallback label remained:\n%s", plain)
	}
}

func TestSensitiveApprovalRedactsExistingToolTranscript(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	request := approvalWidgetRequest("secret-call", "write_file", map[string]any{
		"file_path": "/.env.local", "content": "SECRET=not-for-scrollback",
	})
	model.items = append(model.items, transcriptItem{
		kind: itemTool, callID: request.Call.ID, name: request.Call.Name, args: string(request.Call.Arguments),
	})
	model.toolItems[request.Call.ID] = len(model.items) - 1
	model.presentApproval([]dagent.ApprovalRequest{request})
	arguments := model.items[model.toolItems[request.Call.ID]].args
	if strings.Contains(arguments, "not-for-scrollback") || !strings.Contains(arguments, `"contents":"hidden"`) {
		t.Fatalf("redacted arguments = %q", arguments)
	}
}

func TestApprovalWorkspacePreviewsCurrentEditAndDeleteContents(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "preview.md"), []byte("keep\nold value\ntail\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	model := newTUIModel(t.Context(), &fakeRunner{}, root, "main-model", "thread-1", false, false, "")
	previewPath := filepath.Join(root, "preview.md")

	edit := approvalWidgetRequest("edit-current", "edit_file", map[string]any{
		"file_path": previewPath, "old_string": "old value", "new_string": "new value",
	})
	state := model.presentApproval([]dagent.ApprovalRequest{edit})
	state.ready = true
	plain := ansi.Strip(renderApproval(state, 100))
	for _, want := range []string{"@@ -2,1 +2,1 @@", "-old value", "+new value"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("edit preview missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "-keep") || strings.Contains(plain, "+tail") {
		t.Fatalf("unchanged lines rendered as edits:\n%s", plain)
	}

	deleteRequest := approvalWidgetRequest("delete-current", "delete", map[string]any{"file_path": previewPath})
	state = model.presentApproval([]dagent.ApprovalRequest{deleteRequest})
	state.ready = true
	plain = ansi.Strip(renderApproval(state, 100))
	for _, want := range []string{"+++ /dev/null", "-keep", "-old value", "-tail"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("delete preview missing %q:\n%s", want, plain)
		}
	}
}

func TestApprovalWorkspacePreviewDoesNotFollowEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked.md")); err != nil {
		t.Fatal(err)
	}
	model := newTUIModel(t.Context(), &fakeRunner{}, root, "main-model", "thread-1", false, false, "")
	linkedPath := filepath.Join(root, "linked.md")
	request := approvalWidgetRequest("delete-link", "delete", map[string]any{"file_path": linkedPath})
	state := model.presentApproval([]dagent.ApprovalRequest{request})
	state.ready = true
	plain := ansi.Strip(renderApproval(state, 100))
	if strings.Contains(plain, "outside-secret") || !strings.Contains(plain, "linked.md") {
		t.Fatalf("escaping symlink preview = %q", plain)
	}
}

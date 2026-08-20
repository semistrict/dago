package dacode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
)

func TestUserTranscriptCollapseIsRuneSafeAndToggleable(t *testing.T) {
	message := strings.Repeat("界", userMessageCollapseRunes+20)
	collapsed, expandable := collapseUserTranscript(message, false)
	if !expandable || !utf8.ValidString(collapsed) || !strings.Contains(collapsed, "+5020 characters") || strings.Contains(collapsed, message) {
		t.Fatalf("collapsed = %q, expandable = %t", collapsed, expandable)
	}
	expanded, expandable := collapseUserTranscript(message, true)
	if !expandable || !strings.HasPrefix(expanded, message) || !strings.Contains(expanded, "Ctrl+O to collapse") {
		t.Fatalf("expanded suffix = %q, expandable = %t", expanded[len(message):], expandable)
	}

	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.appendItem(transcriptItem{kind: itemUser, text: message})
	if !model.toggleLatestTranscriptUnit() || !model.items[0].expanded {
		t.Fatal("Ctrl+O target did not expand the long user message")
	}
}

func TestQueuedTranscriptUsesMutedStyle(t *testing.T) {
	if got := queuedTranscriptContainerStyle(80).GetBorderLeftForeground(); got != colorMuted {
		t.Fatalf("queued border color = %v, want %v", got, colorMuted)
	}
	if got := queuedTranscriptTextStyle().GetForeground(); got != colorMuted {
		t.Fatalf("queued text color = %v, want %v", got, colorMuted)
	}
	if plain := ansi.Strip(renderQueuedTranscriptWithGlyphs("wait for this", 80, unicodeUIGlyphs)); !strings.Contains(plain, "> wait for this") {
		t.Fatalf("queued transcript = %q", plain)
	}
}

func TestAssistantMarkdownStreamsIncrementallyAndSanitizesTerminalControls(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.appendAssistant("# Build\nUse **safe** ")
	model.appendAssistant("`code`\x1b]52;c;c2VjcmV0\a")
	if len(model.items) != 1 || !model.items[0].streaming || model.items[0].done {
		t.Fatalf("streaming assistant item = %#v", model.items)
	}
	plain := ansi.Strip(renderItem(model.items[0], 80))
	for _, want := range []string{"Build", "safe", "code", "52;c;c2VjcmV0", "CONTROL"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("render missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(renderItem(model.items[0], 80), "\x1b]52") {
		t.Fatal("assistant markdown preserved an input OSC-52 control sequence")
	}
	model.finishCurrentAssistant()
	if model.items[0].streaming || !model.items[0].done {
		t.Fatalf("finished assistant item = %#v", model.items[0])
	}
	bounded := ansi.Strip(renderAssistantMarkdown(strings.Repeat("x", maximumAssistantMarkdownRunes+10), 80))
	if !strings.Contains(bounded, "assistant output omitted after the render limit") || utf8.RuneCountInString(bounded) > maximumAssistantMarkdownRunes*2+100 {
		t.Fatalf("assistant render was not bounded: runes=%d", utf8.RuneCountInString(bounded))
	}
}

func TestAssistantMarkdownUsesGlamourForTerminalStructuresAndLinks(t *testing.T) {
	markdown := strings.Join([]string{
		"# Release",
		"",
		"> quoted guidance",
		"",
		"- first",
		"- **second**",
		"",
		"| Step | State |",
		"| --- | --- |",
		"| build | ready |",
		"",
		"Read the [release guide](https://example.com/releases/latest).",
		"",
		"~~obsolete~~",
	}, "\n")
	rendered := renderAssistantMarkdown(markdown, 72)
	plain := ansi.Strip(rendered)
	for _, want := range []string{
		"Release", "quoted guidance", "first", "second", "Step", "State", "build", "ready",
		"release guide", "https://example.com/releases/latest", "obsolete",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("Glamour render missing %q:\n%s", want, plain)
		}
	}
	for _, markup := range []string{"**second**", "[release guide](", "| --- |"} {
		if strings.Contains(plain, markup) {
			t.Fatalf("Glamour render retained Markdown syntax %q:\n%s", markup, plain)
		}
	}
	if !strings.Contains(rendered, "\x1b]8;") || !strings.Contains(rendered, "https://example.com/releases/latest") {
		t.Fatalf("Glamour render did not preserve its OSC-8 hyperlink:\n%q", rendered)
	}
}

func TestToolLifecycleRowsGroupingAndExpansion(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.showLineNumbers = true
	model.addToolCall(damessage.ToolCall{ID: "shell", Name: "execute", Arguments: []byte(`{"command":"go test ./..."}`)})
	model.items[0].startedAt = time.Now().Add(-12 * time.Second)
	running := ansi.Strip(renderItem(model.items[0], 80))
	if !strings.Contains(running, "○ execute") || !strings.Contains(running, "running 12s") {
		t.Fatalf("running lifecycle row:\n%s", running)
	}
	model.completeTool(damessage.Message{Role: damessage.RoleTool, ToolCallID: "shell", Name: "execute", ToolStatus: damessage.ToolStatusSuccess, Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: "ok"}}})
	model.addToolCall(damessage.ToolCall{ID: "agent", Name: "task", Arguments: []byte(`{"description":"review the implementation"}`)})

	summary := summarizeTranscriptTools(model.items)
	if summary != "Ran 1 shell command, running 1 agent" {
		t.Fatalf("summary = %q", summary)
	}
	groups := transcriptToolGroups(model.items, 0)
	if len(groups) != 1 {
		t.Fatalf("groups = %#v", groups)
	}
	plain := ansi.Strip(renderItem(model.items[0], 80))
	if !strings.Contains(plain, "✓ execute") || !strings.Contains(plain, "completed") || !strings.Contains(plain, "ok") {
		t.Fatalf("success lifecycle row:\n%s", plain)
	}
	if !model.toggleLatestTranscriptUnit() {
		t.Fatal("Ctrl+O did not expand the tool group")
	}
	for _, group := range groups {
		if !model.toolGroupExpanded[group.key] {
			t.Fatalf("group %q was not expanded", group.key)
		}
	}

	model.markToolRejected("agent", "policy denied")
	rejected := ansi.Strip(renderItem(model.items[1], 80))
	if !strings.Contains(rejected, "! task") || !strings.Contains(rejected, "rejected") || !strings.Contains(rejected, "policy denied") {
		t.Fatalf("rejected lifecycle row:\n%s", rejected)
	}
	skipped := ansi.Strip(renderItem(transcriptItem{kind: itemTool, name: "ask_user", lifecycle: toolSkipped, done: true}, 80))
	if !strings.Contains(skipped, "✗ ask_user") || !strings.Contains(skipped, "skipped") {
		t.Fatalf("skipped lifecycle row:\n%s", skipped)
	}
	failed := ansi.Strip(renderItem(transcriptItem{kind: itemTool, name: "execute", lifecycle: toolError, done: true, failed: true}, 80))
	if !strings.Contains(failed, "✗ execute") || !strings.Contains(failed, "failed") {
		t.Fatalf("failed lifecycle row:\n%s", failed)
	}
}

func TestProviderServerToolBlocksRenderAsCompletedToolCalls(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	block := damessage.ContentBlock{
		Type: damessage.BlockServerTool, ID: "search-1", Name: "web_search",
		Extra: map[string]json.RawMessage{"arguments": json.RawMessage(`{"query":"Brooklyn weather today"}`)},
	}
	model.applyEvent(dagent.Event{Mode: dagent.EventToken, Chunk: &damodel.Chunk{MessageDelta: damessage.Message{
		Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{block},
	}}})
	model.applyEvent(dagent.Event{Mode: dagent.EventUpdate, Update: dastate.Values{dagent.MessagesKey: []damessage.Message{{
		Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{block, {Type: damessage.BlockText, Text: "Partly sunny."}},
	}}}})
	if len(model.items) != 2 || len(model.toolItems) != 1 {
		t.Fatalf("items = %#v, tool items = %#v", model.items, model.toolItems)
	}
	plain := ansi.Strip(renderItem(model.items[0], 100))
	if !strings.Contains(plain, "✓ web_search completed") || !strings.Contains(plain, `Brooklyn weather today`) {
		t.Fatalf("hosted search row:\n%s", plain)
	}
}

func TestInlineDiffLineNumbersBoundsAndSensitiveRedaction(t *testing.T) {
	diff, ok := inlineToolDiff("edit_file", `{"file_path":"main.go","old_string":"old\n","new_string":"new\n"}`)
	if !ok {
		t.Fatal("edit_file diff was not detected")
	}
	withNumbers := ansi.Strip(renderInlineDiff(diff, true, 80))
	withoutNumbers := ansi.Strip(renderInlineDiff(diff, false, 80))
	if !strings.Contains(withNumbers, "    1 - old") || !strings.Contains(withNumbers, "    1 + new") {
		t.Fatalf("numbered diff:\n%s", withNumbers)
	}
	if strings.Contains(withoutNumbers, "    1") || !strings.Contains(withoutNumbers, "- old") || !strings.Contains(withoutNumbers, "+ new") {
		t.Fatalf("unnumbered diff:\n%s", withoutNumbers)
	}

	secret, ok := inlineToolDiff("write_file", `{"file_path":".env","content":"TOKEN=secret"}`)
	if !ok || !secret.redacted {
		t.Fatalf("sensitive diff = %#v, ok = %t", secret, ok)
	}
	redacted := ansi.Strip(renderInlineDiff(secret, true, 80))
	if !strings.Contains(redacted, "Diff hidden for a sensitive path") || strings.Contains(redacted, "TOKEN") || strings.Contains(redacted, "secret") {
		t.Fatalf("sensitive render:\n%s", redacted)
	}

	content := strings.Repeat("+line\n", maximumInlineDiffLines+20)
	bounded := ansi.Strip(renderInlineDiff(inlineDiff{path: "many.txt", content: content}, true, 80))
	if !strings.Contains(bounded, "20 diff lines omitted") {
		t.Fatalf("bounded diff omitted marker missing:\n%s", bounded)
	}
	if _, ok := inlineToolDiff("write_file", strings.Repeat("x", maximumInlineDiffArgumentBytes+1)); ok {
		t.Fatal("oversized diff arguments were accepted")
	}
}

func TestSkillInvocationRowIsExplicitAndCollapsible(t *testing.T) {
	item := transcriptItem{
		kind: itemSkill, name: "review", source: "project", detail: "Review changes", request: "focus security",
		text: strings.Repeat("instruction ", 80),
	}
	plain := ansi.Strip(renderItem(item, 80))
	for _, want := range []string{"Skill: review", "[project]", "Review changes", "User request: focus security", "Ctrl+O"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("skill row missing %q:\n%s", want, plain)
		}
	}
}

func TestTranscriptVirtualizationRetainsAndHydratesHistory(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	for index := range transcriptVirtualWindow + 25 {
		model.appendItem(transcriptItem{kind: itemNotice, text: "entry " + string(rune('A'+index%26))})
	}
	if len(model.items) != transcriptVirtualWindow+25 || model.transcriptStart != 25 {
		t.Fatalf("items = %d, start = %d", len(model.items), model.transcriptStart)
	}
	rendered := ansi.Strip(model.renderTranscript())
	if !strings.Contains(rendered, "25 earlier transcript items virtualized") {
		t.Fatalf("virtualization marker missing:\n%s", rendered)
	}
	if !model.hydrateOlderTranscript() || model.transcriptStart != 0 {
		t.Fatalf("hydrated start = %d", model.transcriptStart)
	}
	if model.hydrateOlderTranscript() {
		t.Fatal("hydration reported older history after reaching the beginning")
	}
}

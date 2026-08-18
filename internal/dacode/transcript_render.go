package dacode

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/glamour/v2"
	glamouransi "charm.land/glamour/v2/ansi"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	transcriptVirtualWindow        = 160
	transcriptVirtualHydrateStep   = 80
	userMessageCollapseRunes       = 10_000
	userMessageHeadRunes           = 2_500
	userMessageTailRunes           = 2_500
	toolOutputPreviewLines         = 6
	toolOutputPreviewRunes         = 400
	maximumRenderedToolRunes       = 100_000
	maximumAssistantMarkdownRunes  = 200_000
	maximumInlineDiffLines         = 100
	maximumInlineDiffBytes         = 100_000
	maximumInlineDiffArgumentBytes = 1 << 20
)

type toolLifecycle string

const (
	toolPending  toolLifecycle = "pending"
	toolRunning  toolLifecycle = "running"
	toolSuccess  toolLifecycle = "success"
	toolError    toolLifecycle = "error"
	toolRejected toolLifecycle = "rejected"
	toolSkipped  toolLifecycle = "skipped"
)

func collapseUserTranscript(text string, expanded bool) (string, bool) {
	return collapseUserTranscriptWithGlyphs(text, expanded, unicodeUIGlyphs)
}

func collapseUserTranscriptWithGlyphs(text string, expanded bool, glyphs uiGlyphs) (string, bool) {
	if utf8.RuneCountInString(text) <= userMessageCollapseRunes {
		return text, false
	}
	if expanded {
		return text + "\nclick or Ctrl+O to collapse", true
	}
	headEnd := runeByteOffset(text, userMessageHeadRunes)
	tailStart := runeByteOffsetFromEnd(text, userMessageTailRunes)
	if tailStart < headEnd {
		return text, false
	}
	hidden := text[headEnd:tailStart]
	amount := fmt.Sprintf("+%d characters", utf8.RuneCountInString(hidden))
	if lines := strings.Count(hidden, "\n"); lines > 0 {
		amount = fmt.Sprintf("+%d lines", lines)
	}
	return text[:headEnd] + "\n" + glyphs.Ellipsis + " " + amount + " " + glyphs.Separator + " click or Ctrl+O to show full message\n" + text[tailStart:], true
}

func runeByteOffset(value string, count int) int {
	if count <= 0 {
		return 0
	}
	index := 0
	for range count {
		if index >= len(value) {
			return len(value)
		}
		_, size := utf8.DecodeRuneInString(value[index:])
		index += size
	}
	return index
}

func runeByteOffsetFromEnd(value string, count int) int {
	if count <= 0 {
		return len(value)
	}
	index := len(value)
	for range count {
		if index <= 0 {
			return 0
		}
		_, size := utf8.DecodeLastRuneInString(value[:index])
		index -= size
	}
	return index
}

func expandableToolText(value string) bool {
	return strings.Count(value, "\n") >= toolOutputPreviewLines || utf8.RuneCountInString(value) > toolOutputPreviewRunes
}

func renderToolOutput(value string, expanded bool) string {
	return renderToolOutputWithGlyphs(value, expanded, unicodeUIGlyphs)
}

func renderToolOutputWithGlyphs(value string, expanded bool, glyphs uiGlyphs) string {
	value = strings.TrimSpace(value)
	truncated := false
	if utf8.RuneCountInString(value) > maximumRenderedToolRunes {
		value = value[:runeByteOffset(value, maximumRenderedToolRunes)]
		truncated = true
	}
	if value == "" || !expandableToolText(value) {
		if truncated {
			return value + "\n" + glyphs.Ellipsis + " tool output omitted after the render limit"
		}
		return value
	}
	if expanded {
		if truncated {
			value += "\n" + glyphs.Ellipsis + " tool output omitted after the render limit"
		}
		return value + "\nclick or Ctrl+O to collapse"
	}
	lines := strings.Split(value, "\n")
	preview := strings.Join(lines[:min(len(lines), toolOutputPreviewLines)], "\n")
	if utf8.RuneCountInString(preview) > toolOutputPreviewRunes {
		preview = preview[:runeByteOffset(preview, toolOutputPreviewRunes)]
	}
	hiddenLines := max(len(lines)-toolOutputPreviewLines, 0)
	if hiddenLines > 0 {
		return preview + fmt.Sprintf("\n%s %d more lines %s click or Ctrl+O to expand", glyphs.Ellipsis, hiddenLines, glyphs.Separator)
	}
	return preview + fmt.Sprintf("\n%s %d more characters %s click or Ctrl+O to expand",
		glyphs.Ellipsis, max(utf8.RuneCountInString(value)-utf8.RuneCountInString(preview), 0), glyphs.Separator)
}

func renderAssistantMarkdown(value string, width int) string {
	return renderAssistantMarkdownWithGlyphs(value, width, unicodeUIGlyphs)
}

func renderAssistantMarkdownWithGlyphs(value string, width int, glyphs uiGlyphs) string {
	if utf8.RuneCountInString(value) > maximumAssistantMarkdownRunes {
		value = value[:runeByteOffset(value, maximumAssistantMarkdownRunes)] + "\n\n" + glyphs.Ellipsis + " assistant output omitted after the render limit"
	}
	value = unicodesecurity.RenderTerminalSafe(value)
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(assistantMarkdownStyle(glyphs, width)),
		glamour.WithWordWrap(max(width, 1)),
		glamour.WithTableWrap(true),
		glamour.WithInlineTableLinks(true),
		glamour.WithPreservedNewLines(),
	)
	if err != nil {
		return value
	}
	rendered, err := renderer.Render(value)
	if err != nil {
		return value
	}
	return strings.TrimSpace(rendered)
}

func assistantMarkdownStyle(glyphs uiGlyphs, width int) glamouransi.StyleConfig {
	body := string(colorBody)
	primary := string(colorPrimary)
	secondary := string(colorSecondary)
	muted := string(colorMuted)
	surface := string(colorSurface)
	bold, italic, underline, crossedOut := true, true, true, true
	zero, one, two := uint(0), uint(1), uint(2)
	indentToken := glyphs.BoxVertical + " "
	tableColumn, tableRow, tableCenter := glyphs.BoxVertical, glyphs.BoxHorizontal, glyphs.Separator
	horizontalRule := "\n" + strings.Repeat(glyphs.BoxHorizontal, min(max(width, 1), 40)) + "\n"
	heading := glamouransi.StyleBlock{StylePrimitive: glamouransi.StylePrimitive{
		BlockSuffix: "\n", Color: &primary, Bold: &bold,
	}}

	return glamouransi.StyleConfig{
		Document: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{Color: &body},
			Margin:         &zero,
		},
		BlockQuote: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{Color: &muted, Italic: &italic},
			Indent:         &one,
			IndentToken:    &indentToken,
		},
		Paragraph: glamouransi.StyleBlock{Margin: &zero},
		List: glamouransi.StyleList{
			StyleBlock:  glamouransi.StyleBlock{StylePrimitive: glamouransi.StylePrimitive{Color: &body}},
			LevelIndent: 2,
		},
		Heading: heading,
		H1:      heading,
		H2:      heading,
		H3:      heading,
		H4:      heading,
		H5:      heading,
		H6:      heading,
		Text:    glamouransi.StylePrimitive{Color: &body},
		Emph:    glamouransi.StylePrimitive{Italic: &italic},
		Strong:  glamouransi.StylePrimitive{Bold: &bold},
		Strikethrough: glamouransi.StylePrimitive{
			CrossedOut: &crossedOut,
		},
		HorizontalRule: glamouransi.StylePrimitive{Color: &muted, Format: horizontalRule},
		Item:           glamouransi.StylePrimitive{BlockPrefix: glyphs.Bullet + " "},
		Enumeration:    glamouransi.StylePrimitive{BlockSuffix: ". ", Color: &primary},
		Task:           glamouransi.StyleTask{Ticked: "[x] ", Unticked: "[ ] "},
		Link: glamouransi.StylePrimitive{
			Prefix: "(", Suffix: ")", Color: &primary, Underline: &underline,
		},
		LinkText: glamouransi.StylePrimitive{Color: &secondary, Underline: &underline},
		Image:    glamouransi.StylePrimitive{Color: &primary, Underline: &underline},
		ImageText: glamouransi.StylePrimitive{
			Color: &secondary, Format: "Image: {{.text}}",
		},
		Code: glamouransi.StyleBlock{StylePrimitive: glamouransi.StylePrimitive{
			Color: &secondary, BackgroundColor: &surface,
		}},
		CodeBlock: glamouransi.StyleCodeBlock{StyleBlock: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{Color: &secondary, BackgroundColor: &surface},
			Indent:         &one,
		}},
		Table: glamouransi.StyleTable{
			StyleBlock:      glamouransi.StyleBlock{StylePrimitive: glamouransi.StylePrimitive{Color: &body}, Margin: &zero},
			CenterSeparator: &tableCenter,
			ColumnSeparator: &tableColumn,
			RowSeparator:    &tableRow,
		},
		DefinitionList: glamouransi.StyleBlock{Margin: &zero, Indent: &two},
		DefinitionTerm: glamouransi.StylePrimitive{Color: &primary, Bold: &bold},
		DefinitionDescription: glamouransi.StylePrimitive{
			BlockPrefix: "\n" + glyphs.Bullet + " ",
		},
		HTMLBlock: glamouransi.StyleBlock{Margin: &zero},
		HTMLSpan:  glamouransi.StyleBlock{Margin: &zero},
	}
}

type inlineDiff struct {
	path     string
	content  string
	redacted bool
}

func inlineToolDiff(name, raw string) (inlineDiff, bool) {
	if len(raw) > maximumInlineDiffArgumentBytes {
		return inlineDiff{}, false
	}
	var arguments map[string]any
	if json.Unmarshal([]byte(raw), &arguments) != nil {
		return inlineDiff{}, false
	}
	path, _ := arguments["file_path"].(string)
	if path == "" {
		return inlineDiff{}, false
	}
	if sensitiveApprovalPath(path) {
		return inlineDiff{path: path, redacted: true}, true
	}
	switch name {
	case "edit_file":
		oldText, oldOK := arguments["old_string"].(string)
		newText, newOK := arguments["new_string"].(string)
		if !oldOK || !newOK {
			return inlineDiff{}, false
		}
		return inlineDiff{path: path, content: approvalFragmentDiff(oldText, newText)}, true
	case "write_file":
		content, ok := arguments["content"].(string)
		if !ok {
			return inlineDiff{}, false
		}
		lines := strings.Split(content, "\n")
		var diff strings.Builder
		fmt.Fprintf(&diff, "--- /dev/null\n+++ %s\n@@ -0,0 +1,%d @@\n", path, len(lines))
		for _, line := range lines {
			diff.WriteString("+" + line + "\n")
		}
		return inlineDiff{path: path, content: diff.String()}, true
	default:
		return inlineDiff{}, false
	}
}

func renderInlineDiff(diff inlineDiff, showNumbers bool, width int) string {
	return renderInlineDiffWithGlyphs(diff, showNumbers, width, unicodeUIGlyphs)
}

func renderInlineDiffWithGlyphs(diff inlineDiff, showNumbers bool, width int, glyphs uiGlyphs) string {
	if diff.redacted {
		return lipgloss.NewStyle().Foreground(colorMuted).Render(
			"Changed " + unicodesecurity.RenderTerminalSafe(diff.path) + "\nDiff hidden for a sensitive path.")
	}
	content := diff.content
	if len(content) > maximumInlineDiffBytes {
		content = content[:maximumInlineDiffBytes]
	}
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if len(lines) > maximumInlineDiffLines {
		lines = append(lines[:maximumInlineDiffLines], fmt.Sprintf("%s %d diff lines omitted", glyphs.Ellipsis, len(lines)-maximumInlineDiffLines))
	}
	oldLine, newLine := 1, 1
	output := []string{lipgloss.NewStyle().Foreground(colorMuted).Bold(true).Render("Changed " + unicodesecurity.RenderTerminalSafe(diff.path))}
	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			oldLine, newLine = parseHunkLines(line)
			output = append(output, lipgloss.NewStyle().Foreground(colorMuted).Render(line))
			continue
		}
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			continue
		}
		kind, number, marker, text := byte(' '), 0, " ", line
		if line != "" {
			kind, text = line[0], line[1:]
		}
		switch kind {
		case '-':
			number, marker, oldLine = oldLine, "-", oldLine+1
		case '+':
			number, marker, newLine = newLine, "+", newLine+1
		default:
			number, oldLine, newLine = newLine, oldLine+1, newLine+1
		}
		prefix := marker + " "
		if showNumbers {
			prefix = fmt.Sprintf("%5d %s ", number, marker)
		}
		style := lipgloss.NewStyle().Foreground(colorMuted)
		if kind == '+' {
			style = lipgloss.NewStyle().Foreground(colorSuccess)
		} else if kind == '-' {
			style = lipgloss.NewStyle().Foreground(colorError)
		}
		output = append(output, style.Width(max(width-2, 1)).Render(prefix+unicodesecurity.RenderTerminalSafe(text)))
	}
	return strings.Join(output, "\n")
}

func parseHunkLines(line string) (int, int) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return 1, 1
	}
	parse := func(value string) int {
		value = strings.TrimLeft(value, "+-")
		value, _, _ = strings.Cut(value, ",")
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 1
		}
		return parsed
	}
	return parse(fields[1]), parse(fields[2])
}

type transcriptToolGroup struct {
	start int
	end   int
	key   string
}

func transcriptToolGroups(items []transcriptItem, start int) map[int]transcriptToolGroup {
	groups := make(map[int]transcriptToolGroup)
	for index := max(start, 0); index < len(items); {
		if !groupableTool(items[index]) {
			index++
			continue
		}
		end := index + 1
		for end < len(items) && groupableTool(items[end]) {
			end++
		}
		if end-index >= 2 {
			groups[index] = transcriptToolGroup{
				start: index, end: end,
				key: fmt.Sprintf("%d:%s:%d:%s", index, items[index].callID, end-1, items[end-1].callID),
			}
		}
		index = end
	}
	return groups
}

func groupableTool(item transcriptItem) bool {
	return item.kind == itemTool && !item.failed && item.lifecycle != toolError && item.lifecycle != toolRejected && item.lifecycle != toolSkipped && !isAskUserTool(item.name)
}

type toolSummaryTally struct {
	category string
	count    int
	pending  int
}

func summarizeTranscriptTools(items []transcriptItem) string {
	order := []string{}
	tallies := map[string]*toolSummaryTally{}
	for _, item := range items {
		category := toolSummaryCategory(item.name)
		if tallies[category] == nil {
			order = append(order, category)
			tallies[category] = &toolSummaryTally{category: category}
		}
		tallies[category].count++
		if !item.done && item.lifecycle != toolSuccess {
			tallies[category].pending++
		}
	}
	segments := make([]string, 0, len(order)*2)
	for _, category := range order {
		tally := tallies[category]
		complete := tally.count - tally.pending
		if complete > 0 {
			segments = append(segments, toolSummaryPhrase(category, complete, false))
		}
		if tally.pending > 0 {
			segments = append(segments, toolSummaryPhrase(category, tally.pending, true))
		}
	}
	for index := 1; index < len(segments); index++ {
		if segments[index] != "" {
			segments[index] = strings.ToLower(segments[index][:1]) + segments[index][1:]
		}
	}
	return strings.Join(segments, ", ")
}

func toolSummaryCategory(name string) string {
	switch name {
	case "execute":
		return "shell command"
	case "task":
		return "agent"
	case "read_file":
		return "file read"
	case "write_file":
		return "file write"
	case "edit_file":
		return "file edit"
	case "delete":
		return "file deletion"
	case "grep", "glob", "ls":
		return "search"
	default:
		return name + " call"
	}
}

func toolSummaryPhrase(category string, count int, present bool) string {
	verb := "Ran"
	if present {
		verb = "Running"
	}
	if strings.HasPrefix(category, "file ") || category == "search" {
		verb = map[bool]string{false: "Completed", true: "Running"}[present]
	}
	noun := category
	if count != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%s %d %s", verb, count, noun)
}

func transcriptVirtualStart(total, requested int) int {
	minimum := max(total-transcriptVirtualWindow, 0)
	if requested < 0 || requested > minimum {
		return minimum
	}
	return requested
}

func toolArgumentDisplay(item transcriptItem, available int) string {
	return toolArgumentDisplayWithGlyphs(item, available, unicodeUIGlyphs)
}

func toolArgumentDisplayWithGlyphs(item transcriptItem, available int, glyphs uiGlyphs) string {
	arguments := strings.TrimSpace(item.args)
	if arguments == "" {
		return ""
	}
	if utf8.RuneCountInString(arguments) > maximumRenderedToolRunes {
		arguments = arguments[:runeByteOffset(arguments, maximumRenderedToolRunes)] + "\n" + glyphs.Ellipsis + " tool arguments omitted after the render limit"
	}
	if item.name == "task" {
		var values map[string]any
		if json.Unmarshal([]byte(arguments), &values) == nil {
			if description, ok := values["description"].(string); ok && description != "" {
				if item.expanded {
					return description
				}
				return truncateTranscriptText(description, min(max(available, 16), 120), glyphs)
			}
		}
	}
	if item.expanded {
		var value any
		if json.Unmarshal([]byte(arguments), &value) == nil {
			if formatted, err := json.MarshalIndent(value, "", "  "); err == nil {
				return string(formatted)
			}
		}
		return arguments
	}
	return truncateTranscriptText(arguments, max(available, 16), glyphs)
}

func truncateTranscriptText(value string, width int, glyphs uiGlyphs) string {
	runes := []rune(value)
	if width <= 0 || len(runes) <= width {
		return value
	}
	tail := []rune(glyphs.Ellipsis)
	if width <= len(tail) {
		return strings.Repeat(".", width)
	}
	return string(runes[:width-len(tail)]) + glyphs.Ellipsis
}

func lifecycleLabel(item transcriptItem, status toolLifecycle, now time.Time) string {
	switch status {
	case toolRunning, toolPending:
		if !item.startedAt.IsZero() && now.Sub(item.startedAt) >= 10*time.Second {
			return fmt.Sprintf("running %s", now.Sub(item.startedAt).Round(time.Second))
		}
		return "running"
	case toolSuccess:
		return "completed"
	case toolError:
		return "failed"
	case toolRejected:
		return "rejected"
	case toolSkipped:
		return "skipped"
	default:
		return string(status)
	}
}

func (model *tuiModel) hydrateOlderTranscript() bool {
	start := transcriptVirtualStart(len(model.items), model.transcriptStart)
	if start == 0 {
		return false
	}
	model.transcriptStart = max(start-transcriptVirtualHydrateStep, 0)
	return true
}

func (model *tuiModel) toggleLatestTranscriptUnit() bool {
	groups := transcriptToolGroups(model.items, 0)
	memberGroup := make(map[int]transcriptToolGroup)
	for _, group := range groups {
		for index := group.start; index < group.end; index++ {
			memberGroup[index] = group
		}
	}
	for index := len(model.items) - 1; index >= 0; index-- {
		if group, grouped := memberGroup[index]; grouped {
			if index != group.end-1 {
				continue
			}
			model.toolGroupExpanded[group.key] = !model.toolGroupExpanded[group.key]
			return true
		}
		item := &model.items[index]
		switch item.kind {
		case itemSkill:
			if expandableToolText(item.text) {
				item.expanded = !item.expanded
				return true
			}
		case itemTool:
			if expandableToolText(item.text) || expandableToolArguments(*item) {
				item.expanded = !item.expanded
				return true
			}
		case itemUser:
			if utf8.RuneCountInString(item.text) > userMessageCollapseRunes {
				item.expanded = !item.expanded
				return true
			}
		}
	}
	return false
}

func expandableToolArguments(item transcriptItem) bool {
	if item.name == "task" {
		var values map[string]any
		if json.Unmarshal([]byte(item.args), &values) == nil {
			description, _ := values["description"].(string)
			return utf8.RuneCountInString(description) > 120
		}
	}
	return strings.Contains(item.args, "\n") || utf8.RuneCountInString(item.args) > 160
}

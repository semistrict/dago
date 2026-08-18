package dacode

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago"
)

func TestASCIIGlyphVariantsCoverAppNeutralRenderers(t *testing.T) {
	glyphs := asciiUIGlyphs
	plugin := newPluginManagerState()
	plugin.loading = true
	goal := newGoalReview("Ship safely", "All tests pass", false)
	cost := detailedCostUIReport()
	longText := strings.Repeat("x", maximumAssistantMarkdownRunes+1)
	diffLines := make([]string, maximumInlineDiffLines+2)
	for index := range diffLines {
		diffLines[index] = fmt.Sprintf("+line %d", index)
	}
	tool := transcriptItem{args: strings.Repeat("x", maximumRenderedToolRunes+1)}
	renders := map[string]string{
		"plugin manager": renderPluginManagerWithGlyphs(plugin, 80, 20, glyphs),
		"plugin reload":  renderPluginReloadPromptWithGlyphs(80, 20, glyphs),
		"plugin loading": renderPluginReloadingWithGlyphs(80, 20, glyphs),
		"goal review":    renderGoalReviewWithGlyphs(goal, 72, "editor", glyphs),
		"rubric": formatRubricSnapshotWithGlyphs(dago.RubricSnapshot{Criteria: "safe", Evaluations: []dago.RubricEvaluation{{
			Criteria: []dago.RubricCriterionEvaluation{{Name: "tests", Passed: true}, {Name: "browser", Passed: false}},
		}}}, glyphs),
		"cost":        formatSessionCostReportWithGlyphs(cost, false, glyphs),
		"assistant":   renderAssistantMarkdownWithGlyphs(longText, 80, glyphs),
		"markdown":    renderAssistantMarkdownWithGlyphs("- item\n\n| a | b |\n| - | - |\n| c | d |", 80, glyphs),
		"tool output": renderToolOutputWithGlyphs(strings.Repeat("line\n", toolOutputPreviewLines+2), false, glyphs),
		"tool args":   toolArgumentDisplayWithGlyphs(tool, 40, glyphs),
		"diff":        renderInlineDiffWithGlyphs(inlineDiff{path: "file.txt", content: strings.Join(diffLines, "\n")}, true, 80, glyphs),
		"completion":  slashCompletionLabelWithGlyphs("/clear", glyphs),
	}
	collapsed, expandable := collapseUserTranscriptWithGlyphs(strings.Repeat("x", userMessageCollapseRunes+1), false, glyphs)
	if !expandable {
		t.Fatal("long user transcript was not collapsible")
	}
	renders["user transcript"] = collapsed

	for name, rendered := range renders {
		rendered = ansi.Strip(rendered)
		if strings.ContainsAny(rendered, unicodeUIGlyphLiteralSet) {
			t.Errorf("%s leaked a Unicode UI glyph:\n%s", name, rendered)
		}
	}
}

const unicodeUIGlyphLiteralSet = "⏺…✓✗○●⎿⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏⏸⏎⚠⏳↻↑↓←→•·—›▸▾─│╭╮╰╯━⋮↗"

func TestRendererSourcesKeepGlyphLiteralsInCanonicalCatalog(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source path is unavailable")
	}
	directory := filepath.Dir(filename)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	excluded := map[string]bool{
		"app.go": true, "run.go": true, "terminal_ui.go": true,
		"ask_user.go": true, "local_dev_server.go": true, "theme.go": true,
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || excluded[name] {
			continue
		}
		files := token.NewFileSet()
		parsed, parseErr := parser.ParseFile(files, filepath.Join(directory, name), nil, 0)
		if parseErr != nil {
			t.Errorf("parse %s: %v", name, parseErr)
			continue
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, literalOK := node.(*ast.BasicLit)
			if !literalOK || literal.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(literal.Value)
			if unquoteErr == nil && strings.ContainsAny(value, unicodeUIGlyphLiteralSet) {
				t.Errorf("%s:%d contains a UI glyph literal outside terminal_ui.go", name, files.Position(literal.Pos()).Line)
			}
			return true
		})
	}
}

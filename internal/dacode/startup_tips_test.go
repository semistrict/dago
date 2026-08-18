package dacode

import (
	"slices"
	"strings"
	"testing"
)

func TestStartupTipsUseEditorAndApprovalPolicy(t *testing.T) {
	withYOLO := activeStartupTips("neovim", true)
	withoutYOLO := activeStartupTips("", false)
	texts := func(tips []weightedStartupTip) []string {
		result := make([]string, len(tips))
		for index, tip := range tips {
			if tip.Text == "" || tip.Weight == 0 {
				t.Fatalf("invalid startup tip at %d: %#v", index, tip)
			}
			result[index] = tip.Text
		}
		return result
	}
	if !slices.Contains(texts(withYOLO), "Press Ctrl+X to compose prompts in neovim") ||
		!slices.Contains(texts(withYOLO), "Press Shift+Tab to cycle Manual, Auto, and YOLO modes") {
		t.Fatalf("YOLO tips = %#v", texts(withYOLO))
	}
	if !slices.Contains(texts(withoutYOLO), "Press Ctrl+X to compose prompts in your external editor") ||
		!slices.Contains(texts(withoutYOLO), "Press Shift+Tab to toggle Manual and Auto modes") {
		t.Fatalf("non-YOLO tips = %#v", texts(withoutYOLO))
	}
}

func TestChooseStartupTipIsWeightedAndDeterministic(t *testing.T) {
	tips := []weightedStartupTip{{Text: "first", Weight: 2}, {Text: "second", Weight: 1}, {Text: "ignored"}}
	want := []string{"first", "first", "second", "first"}
	for sample, expected := range want {
		if got := chooseStartupTip(tips, uint64(sample)); got != expected {
			t.Fatalf("sample %d = %q, want %q", sample, got, expected)
		}
	}
	if got := chooseStartupTip(nil, 3); got != "" {
		t.Fatalf("empty registry chose %q", got)
	}
}

func TestStartupTipVisibilityUsesUsefulDefault(t *testing.T) {
	if !startupTipsVisible(nil) || !startupTipsVisible(mapLookup(map[string]string{})) || !startupTipsVisible(mapLookup(map[string]string{hideSplashTipsEnvironment: "no"})) {
		t.Fatal("startup tips were hidden by the useful default")
	}
	for _, value := range []string{"1", " true ", "YES", "on", "y"} {
		if startupTipsVisible(mapLookup(map[string]string{hideSplashTipsEnvironment: value})) {
			t.Fatalf("startup tips visible for %q", value)
		}
	}
}

func TestStartupTipsRejectUnsafeEditorLabel(t *testing.T) {
	tips := activeStartupTips("unsafe\x1b[31m", true)
	for _, tip := range tips {
		if tip.Text == "Press Ctrl+X to compose prompts in unsafe\x1b[31m" {
			t.Fatal("unsafe editor label was interpolated")
		}
	}
}

func TestChooseStartupTipBoundsRegistryAndText(t *testing.T) {
	tips := make([]weightedStartupTip, maxStartupTips+10)
	for index := range tips {
		tips[index] = weightedStartupTip{Text: strings.Repeat("x", maxStartupTipText+50), Weight: 1}
	}
	for _, sample := range []uint64{0, maxStartupTips - 1, maxStartupTips, ^uint64(0)} {
		if got := chooseStartupTip(tips, sample); len([]rune(got)) != maxStartupTipText {
			t.Fatalf("sample %d length = %d", sample, len([]rune(got)))
		}
	}
}

func TestChooseStartupTipSanitizesChosenText(t *testing.T) {
	got := chooseStartupTip([]weightedStartupTip{{Text: "unsafe\x1b[31m\nnext", Weight: 1}}, 0)
	if strings.ContainsAny(got, "\x1b\n\r") || !strings.Contains(got, "U+001B") {
		t.Fatalf("chosen tip = %q", got)
	}
}

func TestStartupTipStateRespectsFreshResumedFallbackAndFirstSubmit(t *testing.T) {
	fresh := newStartupTipState(startupTipFresh, "neovim", true, 0, true)
	resumed := newStartupTipState(startupTipResumed, "", false, 0, true)
	fallback := newStartupTipState(startupTipFallback, "unsafe\x1b", true, 0, true)
	if fresh.Text == "" || !strings.Contains(resumed.Text, "/threads") || fallback.Text != fallbackStartupTip {
		t.Fatalf("fresh=%#v resumed=%#v fallback=%#v", fresh, resumed, fallback)
	}
	if !fresh.dismissOnFirstSubmit() || fresh.Visible || !fresh.Dismissed {
		t.Fatalf("first dismissal = %#v", fresh)
	}
	if fresh.show() || fresh.Visible || fresh.dismissOnFirstSubmit() {
		t.Fatalf("dismissed tip was re-armed: %#v", fresh)
	}
	hidden := newStartupTipState(startupTipFresh, "", true, 0, false)
	if hidden.dismissOnFirstSubmit() || !hidden.Dismissed || hidden.Visible {
		t.Fatalf("hidden first-submit state = %#v", hidden)
	}
}

func TestStartupTipAudienceRetainsConfiguredEditorAndModeCycle(t *testing.T) {
	for _, audience := range []startupTipAudience{startupTipFresh, startupTipResumed} {
		texts := make([]string, 0)
		for _, tip := range startupTipsForAudience(audience, "helix", true) {
			texts = append(texts, tip.Text)
		}
		if !slices.Contains(texts, "Press Ctrl+X to compose prompts in helix") ||
			!slices.Contains(texts, "Press Shift+Tab to cycle Manual, Auto, and YOLO modes") {
			t.Fatalf("audience %d tips = %#v", audience, texts)
		}
	}
}

package dacode

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/daupdate"
)

func TestUpdateModalCheckConfirmApplyLifecycle(t *testing.T) {
	state := newUpdateModal()
	if action := state.handleKey("enter"); action != updateModalNoAction || state.phase != updateModalChecking {
		t.Fatalf("checking key = %d, phase %d", action, state.phase)
	}
	result := daupdate.Result{Status: daupdate.UpdateAvailable, CurrentVersion: "v1.0.0", LatestVersion: "v1.1.0", Channel: "stable"}
	state.finishCheck(result, nil)
	if state.phase != updateModalAvailable {
		t.Fatalf("check phase = %d", state.phase)
	}
	if action := state.handleKey("enter"); action != updateModalNoAction || state.phase != updateModalConfirm {
		t.Fatalf("available key = %d, phase %d", action, state.phase)
	}
	if action := state.handleKey("n"); action != updateModalNoAction || state.phase != updateModalAvailable {
		t.Fatalf("confirm cancel = %d, phase %d", action, state.phase)
	}
	state.handleKey("enter")
	if action := state.handleKey("y"); action != updateModalApply || state.phase != updateModalApplying {
		t.Fatalf("apply = %d, phase %d", action, state.phase)
	}
	result.Applied = true
	state.finishApply(result, nil)
	if state.phase != updateModalComplete || state.handleKey("enter") != updateModalCancel {
		t.Fatalf("complete phase = %d", state.phase)
	}
}

func TestUpdateModalFailureIsBoundedSafeAndRetryable(t *testing.T) {
	state := newUpdateModal()
	state.finishCheck(daupdate.Result{}, errors.New("failed\x1b[31m secret="+strings.Repeat("x", 700)))
	if state.phase != updateModalFailed || state.err != "The update operation failed." || strings.Contains(state.err, "secret") {
		t.Fatalf("failure = phase %d, %q", state.phase, state.err)
	}
	view := ansi.Strip(renderUpdateModal(state, 80, 24, unicodeUIGlyphs))
	if !strings.Contains(view, "Update failed") || !strings.Contains(view, "R/Enter retry") {
		t.Fatalf("failure view:\n%s", view)
	}
	if action := state.handleKey("r"); action != updateModalRetry || state.phase != updateModalChecking || state.err != "" {
		t.Fatalf("retry = %d, phase %d, err %q", action, state.phase, state.err)
	}
}

func TestUpdateModalRejectsInvalidResultsAndMapsStableErrors(t *testing.T) {
	state := newUpdateModal()
	state.finishCheck(daupdate.Result{Status: "unexpected"}, nil)
	if state.phase != updateModalFailed || !strings.Contains(state.err, "invalid status") {
		t.Fatalf("invalid check result = phase %d, %q", state.phase, state.err)
	}
	state.finishApply(daupdate.Result{Status: daupdate.UpdateAvailable}, nil)
	if state.phase != updateModalFailed || state.err != "The update was not installed." {
		t.Fatalf("invalid apply result = phase %d, %q", state.phase, state.err)
	}
	state.finishCheck(daupdate.Result{}, errors.Join(daupdate.ErrUntrustedManifest, errors.New("secret-token")))
	if state.err != "The release manifest signature is not trusted." || strings.Contains(state.err, "secret-token") {
		t.Fatalf("mapped failure = %q", state.err)
	}
}

func TestUpdateModalCurrentAndCancel(t *testing.T) {
	state := newUpdateModal()
	state.finishCheck(daupdate.Result{Status: daupdate.UpToDate, CurrentVersion: "v1", LatestVersion: "v1"}, nil)
	if state.phase != updateModalCurrent || state.handleKey("esc") != updateModalCancel {
		t.Fatalf("current = phase %d", state.phase)
	}
	state = newUpdateModal()
	if state.handleKey("esc") != updateModalCancel {
		t.Fatal("checking cancel was ignored")
	}
}

func TestUpdateModalRendersASCII(t *testing.T) {
	state := newUpdateModal()
	state.finishCheck(daupdate.Result{Status: daupdate.UpdateAvailable, CurrentVersion: "v1", LatestVersion: "v2", Channel: "stable"}, nil)
	view := ansi.Strip(renderUpdateModal(state, 80, 24, asciiUIGlyphs))
	for _, wanted := range []string{"Software Update", "Update available", "current v1 - latest v2 - channel stable", "Enter update"} {
		if !strings.Contains(view, wanted) {
			t.Fatalf("view missing %q:\n%s", wanted, view)
		}
	}
	if strings.ContainsAny(view, "╭╮╰╯│─…✓✗⏳•⏎") {
		t.Fatalf("ASCII update modal contains Unicode UI glyphs:\n%s", view)
	}
}

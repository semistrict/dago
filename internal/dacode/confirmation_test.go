package dacode

import (
	"testing"
	"time"
)

func TestConfirmationRequiresSecondPressWithinWindow(t *testing.T) {
	arms := newConfirmationArms()
	now := time.Unix(100, 0)
	if arms.press(confirmQuit, now) {
		t.Fatal("first press confirmed")
	}
	if arms.press(confirmClearInput, now.Add(time.Second)) {
		t.Fatal("different action confirmed")
	}
	if arms.press(confirmQuit, now.Add(2*time.Second)) {
		t.Fatal("higher-priority clear action did not disarm quit")
	}
	if !arms.press(confirmQuit, now.Add(2500*time.Millisecond)) {
		t.Fatal("replacement quit arm did not confirm")
	}
	if arms.press(confirmClearInput, now.Add(5*time.Second)) {
		t.Fatal("expired action confirmed")
	}
	arms.expire(now.Add(9 * time.Second))
	if len(arms.deadlines) != 0 {
		t.Fatalf("deadlines = %#v", arms.deadlines)
	}
}

func TestConfirmationHigherPriorityInterventionDisarmsOnlyLowerArms(t *testing.T) {
	arms := newConfirmationArms()
	now := time.Unix(100, 0)
	arms.press(confirmDelete, now)
	arms.press(confirmQuit, now.Add(time.Second))
	if !arms.press(confirmDelete, now.Add(2*time.Second)) {
		t.Fatal("lower-priority quit disarmed delete")
	}
	arms.press(confirmQuit, now.Add(3*time.Second))
	arms.intervene(confirmClearInput)
	if _, armed := arms.deadlines[confirmQuit]; armed {
		t.Fatal("clear intervention left quit armed")
	}
}

func TestConfirmationCanBeDisarmed(t *testing.T) {
	arms := newConfirmationArms()
	now := time.Unix(100, 0)
	arms.press(confirmDelete, now)
	arms.disarm(confirmDelete)
	if arms.press(confirmDelete, now.Add(time.Second)) {
		t.Fatal("disarmed action confirmed")
	}
}

func TestConfirmationRejectsExpiredAndRegressedClocks(t *testing.T) {
	arms := newConfirmationArms()
	now := time.Unix(100, 0)
	if arms.press(confirmQuit, now) {
		t.Fatal("first press confirmed")
	}
	if arms.press(confirmQuit, now.Add(-time.Second)) {
		t.Fatal("regressed clock confirmed")
	}
	if arms.press(confirmQuit, now.Add(-time.Second+defaultConfirmationWindow)) {
		t.Fatal("replacement arm unexpectedly confirmed")
	}
	if arms.press(confirmClearInput, now) {
		t.Fatal("first clear press confirmed")
	}
	if arms.press(confirmClearInput, now.Add(defaultConfirmationWindow)) {
		t.Fatal("deadline boundary confirmed")
	}
}

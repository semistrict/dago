package dacode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnboardingMarkerUsesFirstRunAndEnvironmentPrecedence(t *testing.T) {
	stateDirectory := filepath.Join(t.TempDir(), "state")
	run, diagnostics := shouldRunOnboarding(stateDirectory, mapLookup(map[string]string{}))
	if !run || len(diagnostics) != 0 {
		t.Fatalf("first run = %v, %v", run, diagnostics)
	}
	if err := markOnboardingComplete(stateDirectory); err != nil {
		t.Fatal(err)
	}
	run, diagnostics = shouldRunOnboarding(stateDirectory, mapLookup(map[string]string{}))
	if run || len(diagnostics) != 0 {
		t.Fatalf("completed run = %v, %v", run, diagnostics)
	}
	for value, expected := range map[string]bool{"true": true, "0": false} {
		run, diagnostics = shouldRunOnboarding(stateDirectory, mapLookup(map[string]string{onboardingEnvironment: value}))
		if run != expected || len(diagnostics) != 0 {
			t.Fatalf("override %q = %v, %v", value, run, diagnostics)
		}
	}
	run, diagnostics = shouldRunOnboarding(stateDirectory, mapLookup(map[string]string{onboardingEnvironment: "maybe"}))
	if run || len(diagnostics) == 0 {
		t.Fatalf("invalid override = %v, %v", run, diagnostics)
	}
}

func TestOnboardingMarkerIsPrivateAndRejectsUnsafeFiles(t *testing.T) {
	stateDirectory := filepath.Join(t.TempDir(), "state")
	if err := markOnboardingComplete(stateDirectory); err != nil {
		t.Fatal(err)
	}
	marker := onboardingMarkerPath(stateDirectory)
	info, err := os.Stat(marker)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("marker info = %#v, %v", info, err)
	}
	directoryInfo, err := os.Stat(stateDirectory)
	if err != nil || directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("directory info = %#v, %v", directoryInfo, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, marker); err != nil {
		t.Fatal(err)
	}
	run, diagnostics := shouldRunOnboarding(stateDirectory, nil)
	if !run || len(diagnostics) == 0 {
		t.Fatalf("symlink marker = %v, %v", run, diagnostics)
	}
}

func TestGoalCriteriaPromptMarkerIsIndependent(t *testing.T) {
	stateDirectory := filepath.Join(t.TempDir(), "state")
	shown, diagnostics := hasShownGoalCriteriaPrompt(stateDirectory)
	if shown || len(diagnostics) != 0 {
		t.Fatalf("initial prompt marker = %v, %v", shown, diagnostics)
	}
	if err := markGoalCriteriaPromptShown(stateDirectory); err != nil {
		t.Fatal(err)
	}
	shown, diagnostics = hasShownGoalCriteriaPrompt(stateDirectory)
	if !shown || len(diagnostics) != 0 {
		t.Fatalf("saved prompt marker = %v, %v", shown, diagnostics)
	}
	if run, _ := shouldRunOnboarding(stateDirectory, nil); !run {
		t.Fatal("goal marker incorrectly completed onboarding")
	}
}

func TestUpsertOnboardingNameMemoryIsIdempotentAndRepairsMarkers(t *testing.T) {
	first, err := upsertOnboardingNameMemory("# Memory\n", "  Ada   Lovelace ")
	if err != nil || !strings.Contains(first, `The user's preferred name is "Ada Lovelace".`) || strings.Count(first, onboardingNameMemoryStart) != 1 {
		t.Fatalf("first = %q, %v", first, err)
	}
	second, err := upsertOnboardingNameMemory(first, "Grace Hopper")
	if err != nil || strings.Contains(second, "Ada Lovelace") || strings.Count(second, onboardingNameMemoryStart) != 1 || !strings.Contains(second, "Grace Hopper") {
		t.Fatalf("second = %q, %v", second, err)
	}
	broken := "preface\n" + onboardingNameMemoryStart + "\norphan\n"
	repaired, err := upsertOnboardingNameMemory(broken, "Lin")
	if err != nil || strings.Count(repaired, onboardingNameMemoryStart) != 1 || strings.Count(repaired, onboardingNameMemoryEnd) != 1 {
		t.Fatalf("repaired = %q, %v", repaired, err)
	}
}

func TestUpsertOnboardingNameMemoryBoundsAndSkipsEmpty(t *testing.T) {
	if got, err := upsertOnboardingNameMemory("keep\n", "  "); err != nil || got != "keep\n" {
		t.Fatalf("empty = %q, %v", got, err)
	}
	if _, err := upsertOnboardingNameMemory(strings.Repeat("x", maximumOnboardingMemorySourceBytes+1), "Ada"); err == nil {
		t.Fatal("oversize memory accepted")
	}
	if _, err := upsertOnboardingNameMemory("", "unsafe\nname"); err == nil {
		t.Fatal("unsafe name accepted")
	}
}

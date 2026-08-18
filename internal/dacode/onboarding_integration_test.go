package dacode

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnboardingCLIAndDependencyCatalog(t *testing.T) {
	t.Parallel()
	options, err := parseCLI([]string{"--init"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !options.init {
		t.Fatal("--init did not request onboarding")
	}
	dependencies := onboardingDependencyCatalog()
	if len(dependencies) == 0 {
		t.Fatal("onboarding dependency catalog is empty")
	}
	seen := map[string]bool{}
	for _, dependency := range dependencies {
		if dependency.Name == "" || dependency.Category == "" || dependency.Description == "" || !dependency.Installed || seen[dependency.Name] {
			t.Fatalf("invalid dependency: %#v", dependency)
		}
		seen[dependency.Name] = true
	}
}

func TestPersistOnboardingResultUpdatesManagedNameAndMarkers(t *testing.T) {
	t.Parallel()
	stateDirectory := t.TempDir()
	if err := persistOnboardingResult(stateDirectory, defaultAgentName, onboardingResult{Name: "Ramon", AutoAcceptGoalCriteria: true}); err != nil {
		t.Fatal(err)
	}
	memory, err := os.ReadFile(filepath.Join(stateDirectory, defaultAgentName, agentInstructionsFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(memory), `preferred name is "Ramon"`) {
		t.Fatalf("memory = %q", memory)
	}
	for _, path := range []string{onboardingMarkerPath(stateDirectory), goalCriteriaPromptMarkerPath(stateDirectory)} {
		if value, err := os.ReadFile(path); err != nil || string(value) != "1\n" {
			t.Fatalf("marker %s = %q, %v", path, value, err)
		}
	}
	if err := persistOnboardingResult(stateDirectory, defaultAgentName, onboardingResult{Name: "Ray"}); err != nil {
		t.Fatal(err)
	}
	memory, err = os.ReadFile(filepath.Join(stateDirectory, defaultAgentName, agentInstructionsFilename))
	if err != nil || strings.Count(string(memory), onboardingNameMemoryStart) != 1 || !strings.Contains(string(memory), `preferred name is "Ray"`) {
		t.Fatalf("updated memory = %q, %v", memory, err)
	}
}

func TestPersistOnboardingResultRejectsMemorySymlink(t *testing.T) {
	t.Parallel()
	stateDirectory := t.TempDir()
	if err := ensureAgentMemoryFile(stateDirectory, defaultAgentName); err != nil {
		t.Fatal(err)
	}
	memoryPath := filepath.Join(stateDirectory, defaultAgentName, agentInstructionsFilename)
	if err := os.Remove(memoryPath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, memoryPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := persistOnboardingResult(stateDirectory, defaultAgentName, onboardingResult{Name: "Ramon"}); err == nil {
		t.Fatal("symlinked memory was accepted")
	}
	value, err := os.ReadFile(outside)
	if err != nil || string(value) != "safe" {
		t.Fatalf("outside = %q, %v", value, err)
	}
}

func TestOnboardingInitIsRejectedByProtocolMode(t *testing.T) {
	t.Parallel()
	if _, err := parseCLI([]string{"acp", "--init"}, io.Discard); err == nil {
		t.Fatal("protocol mode accepted interactive onboarding")
	}
}

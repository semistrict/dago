package dacode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	onboardingEnvironment              = "DEEPAGENTS_CODE_ONBOARDING"
	onboardingMarkerFilename           = "onboarding_complete"
	goalCriteriaPromptMarkerFilename   = "goal_auto_accept_criteria_prompted_v1"
	onboardingNameMemoryStart          = "<!-- deepagents:onboarding-name:start -->"
	onboardingNameMemoryEnd            = "<!-- deepagents:onboarding-name:end -->"
	maximumOnboardingMarkerFileBytes   = 16
	maximumOnboardingMemorySourceBytes = 4 << 20
)

func onboardingMarkerPath(stateDirectory string) string {
	if stateDirectory == "" || !filepath.IsAbs(stateDirectory) || strings.ContainsRune(stateDirectory, 0) {
		panic("dacode: absolute onboarding state directory is required")
	}
	return filepath.Join(filepath.Clean(stateDirectory), onboardingMarkerFilename)
}

func goalCriteriaPromptMarkerPath(stateDirectory string) string {
	if stateDirectory == "" || !filepath.IsAbs(stateDirectory) || strings.ContainsRune(stateDirectory, 0) {
		panic("dacode: absolute onboarding state directory is required")
	}
	return filepath.Join(filepath.Clean(stateDirectory), goalCriteriaPromptMarkerFilename)
}

func shouldRunOnboarding(stateDirectory string, lookup func(string) (string, bool)) (bool, []string) {
	if lookup != nil {
		if raw, exists := lookup(onboardingEnvironment); exists {
			if value, ok := parseOnboardingBool(raw); ok {
				return value, nil
			}
			completed, diagnostics := onboardingMarkerExists(onboardingMarkerPath(stateDirectory))
			return !completed, append([]string{"invalid onboarding override; using first-run marker"}, diagnostics...)
		}
	}
	completed, diagnostics := onboardingMarkerExists(onboardingMarkerPath(stateDirectory))
	return !completed, diagnostics
}

func hasShownGoalCriteriaPrompt(stateDirectory string) (bool, []string) {
	return onboardingMarkerExists(goalCriteriaPromptMarkerPath(stateDirectory))
}

func onboardingMarkerExists(path string) (bool, []string) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, []string{"onboarding marker could not be inspected; setup will be offered"}
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximumOnboardingMarkerFileBytes {
		return false, []string{"onboarding marker is unsafe; setup will be offered"}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "1\n" {
		return false, []string{"onboarding marker is malformed; setup will be offered"}
	}
	return true, nil
}

func markOnboardingComplete(stateDirectory string) error {
	return writeOnboardingMarker(onboardingMarkerPath(stateDirectory))
}

func markGoalCriteriaPromptShown(stateDirectory string) error {
	return writeOnboardingMarker(goalCriteriaPromptMarkerPath(stateDirectory))
}

func writeOnboardingMarker(path string) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create onboarding state directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("secure onboarding state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".onboarding-*")
	if err != nil {
		return fmt.Errorf("create onboarding marker: %w", err)
	}
	temporaryPath := temporary.Name()
	defer temporary.Close()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure onboarding marker: %w", err)
	}
	if _, err := temporary.WriteString("1\n"); err != nil {
		return fmt.Errorf("write onboarding marker: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync onboarding marker: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close onboarding marker: %w", err)
	}
	if err := replaceFileDurably(temporaryPath, path); err != nil {
		return fmt.Errorf("replace onboarding marker: %w", err)
	}
	return nil
}

func parseOnboardingBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on", "y":
		return true, true
	case "0", "false", "no", "off", "n":
		return false, true
	default:
		return false, false
	}
}

func upsertOnboardingNameMemory(existing, name string) (string, error) {
	if len(existing) > maximumOnboardingMemorySourceBytes {
		return "", errors.New("agent memory exceeds 4 MiB")
	}
	if hasModelSelectorControl(name) {
		return "", errors.New("onboarding name is invalid")
	}
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return existing, nil
	}
	if len(name) > 320 {
		return "", errors.New("onboarding name is invalid")
	}
	encoded, err := json.Marshal(name)
	if err != nil {
		return "", errors.New("onboarding name could not be encoded")
	}
	block := onboardingNameMemoryStart + "\n- The user's preferred name is " + string(encoded) + ".\n" + onboardingNameMemoryEnd
	start := strings.Index(existing, onboardingNameMemoryStart)
	end := strings.Index(existing, onboardingNameMemoryEnd)
	if start >= 0 && end > start {
		end += len(onboardingNameMemoryEnd)
		parts := []string{strings.TrimSpace(existing[:start]), block, strings.TrimSpace(existing[end:])}
		return joinNonemptySections(parts), nil
	}
	clean := strings.ReplaceAll(existing, onboardingNameMemoryStart, "")
	clean = strings.ReplaceAll(clean, onboardingNameMemoryEnd, "")
	clean = strings.TrimSpace(clean)
	if clean == "" {
		return "## User Preferences\n\n" + block + "\n", nil
	}
	if strings.Contains(clean, "## User Preferences") {
		return clean + "\n\n" + block + "\n", nil
	}
	return clean + "\n\n## User Preferences\n\n" + block + "\n", nil
}

func joinNonemptySections(parts []string) string {
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return strings.Join(result, "\n\n") + "\n"
}

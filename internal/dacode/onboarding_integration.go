package dacode

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func onboardingDependencyCatalog() []onboardingDependency {
	specs := dacodeInstallCatalog()
	result := make([]onboardingDependency, 0, len(specs))
	for _, spec := range specs {
		result = append(result, onboardingDependency{
			Name: spec.Name, Category: "Included integrations", Description: spec.Description, Installed: spec.BuiltIn,
		})
	}
	return result
}

func persistOnboardingResult(stateDirectory, agentName string, result onboardingResult) error {
	if err := ensureAgentMemoryFile(stateDirectory, agentName); err != nil {
		return err
	}
	if result.Name != "" {
		if err := persistOnboardingName(stateDirectory, agentName, result.Name); err != nil {
			return err
		}
	}
	if result.AutoAcceptGoalCriteria {
		if err := markGoalCriteriaPromptShown(stateDirectory); err != nil {
			return err
		}
	}
	return markOnboardingComplete(stateDirectory)
}

func persistOnboardingName(stateDirectory, agentName, name string) error {
	if err := validateAgentName(agentName); err != nil {
		return err
	}
	root, err := os.OpenRoot(stateDirectory)
	if err != nil {
		return fmt.Errorf("open onboarding memory storage: %w", err)
	}
	defer root.Close()
	relative := filepath.Join(agentName, agentInstructionsFilename)
	info, err := root.Lstat(relative)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximumOnboardingMemorySourceBytes {
		if err == nil {
			err = errors.New("memory path is not a bounded regular file")
		}
		return fmt.Errorf("inspect onboarding memory: %w", err)
	}
	file, err := root.Open(relative)
	if err != nil {
		return fmt.Errorf("open onboarding memory: %w", err)
	}
	existing, readErr := io.ReadAll(io.LimitReader(file, maximumOnboardingMemorySourceBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(existing) > maximumOnboardingMemorySourceBytes {
		if readErr == nil {
			readErr = closeErr
		}
		if readErr == nil {
			readErr = errors.New("memory source exceeds 4 MiB")
		}
		return fmt.Errorf("read onboarding memory: %w", readErr)
	}
	updated, err := upsertOnboardingNameMemory(string(existing), name)
	if err != nil {
		return err
	}
	if updated == string(existing) {
		return nil
	}
	temporary, err := os.CreateTemp(filepath.Join(stateDirectory, agentName), ".onboarding-memory-*")
	if err != nil {
		return fmt.Errorf("create onboarding memory replacement: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure onboarding memory replacement: %w", err)
	}
	if _, err := temporary.WriteString(updated); err != nil {
		temporary.Close()
		return fmt.Errorf("write onboarding memory replacement: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync onboarding memory replacement: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close onboarding memory replacement: %w", err)
	}
	if err := replaceFileDurably(temporaryPath, filepath.Join(stateDirectory, relative)); err != nil {
		return fmt.Errorf("replace onboarding memory: %w", err)
	}
	return nil
}

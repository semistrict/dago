package dacode

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	approvalPreferencesFilename = "approval.json"
	autoModeNoticeVersion       = "2026-07-24"
	yoloPolicyVersion           = "2026-07-14"
	maxApprovalPreferencesSize  = 1 << 20
	threadApprovalModesKey      = "thread_approval_modes"
)

var approvalPreferencesMu sync.Mutex

func hasAutoModeNotice(path string) (bool, error) {
	preferences, err := loadApprovalPreferences(path)
	if err != nil {
		return false, err
	}
	shown, _ := preferences["auto_notice_shown"].(bool)
	version, _ := preferences["auto_notice_version"].(string)
	return shown && version == autoModeNoticeVersion, nil
}

func saveAutoModeNotice(path string) error {
	return updateApprovalPreferences(path, map[string]any{
		"auto_notice_shown":   true,
		"auto_notice_version": autoModeNoticeVersion,
	})
}

func hasYoloAcknowledgement(path string) (bool, error) {
	preferences, err := loadApprovalPreferences(path)
	if err != nil {
		return false, err
	}
	acknowledged, _ := preferences["acknowledged"].(bool)
	policy, _ := preferences["policy_version"].(string)
	version, _ := preferences["version"].(float64)
	return acknowledged && policy == yoloPolicyVersion && version == 1, nil
}

func saveYoloAcknowledgement(path string) error {
	return updateApprovalPreferences(path, map[string]any{
		"acknowledged":   true,
		"policy_version": yoloPolicyVersion,
	})
}

// approvalModeStore persists live approval policy independently for each
// thread. Raw thread identifiers never appear in the install-local file.
type approvalModeStore struct {
	path             string
	generationMu     sync.Mutex
	latestGeneration map[string]uint64
}

func newApprovalModeStore(path string) *approvalModeStore {
	return &approvalModeStore{path: path, latestGeneration: map[string]uint64{}}
}

func (store *approvalModeStore) registerGeneration(threadID string, generation uint64) {
	store.generationMu.Lock()
	defer store.generationMu.Unlock()
	store.latestGeneration[threadID] = generation
}

func (store *approvalModeStore) saveGeneration(threadID string, mode approvalMode, generation uint64) error {
	store.generationMu.Lock()
	defer store.generationMu.Unlock()
	if store.latestGeneration[threadID] != generation {
		return nil
	}
	return store.Save(threadID, mode)
}

func approvalModeKey(threadID string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(threadID)))
}

func (store *approvalModeStore) Load(threadID string) (approvalMode, error) {
	if store == nil || store.path == "" {
		return approvalManual, errors.New("approval-mode store is unavailable")
	}
	if threadID == "" {
		return approvalManual, errors.New("approval-mode thread ID is required")
	}
	preferences, err := loadApprovalPreferences(store.path)
	if err != nil {
		return approvalManual, fmt.Errorf("load approval mode: %w", err)
	}
	rawModes, exists := preferences[threadApprovalModesKey]
	if !exists {
		return approvalAuto, nil
	}
	modes, ok := rawModes.(map[string]any)
	if !ok {
		return approvalManual, errors.New("load approval mode: thread records are invalid")
	}
	rawRecord, exists := modes[approvalModeKey(threadID)]
	if !exists {
		return approvalAuto, nil
	}
	record, ok := rawRecord.(map[string]any)
	if !ok {
		return approvalManual, errors.New("load approval mode: thread record is invalid")
	}
	mode, valid := parseApprovalMode(record["mode"])
	if !valid {
		return approvalManual, errors.New("load approval mode: stored mode is invalid")
	}
	return mode, nil
}

func (store *approvalModeStore) Save(threadID string, mode approvalMode) error {
	if store == nil || store.path == "" {
		return errors.New("approval-mode store is unavailable")
	}
	if threadID == "" {
		return errors.New("approval-mode thread ID is required")
	}
	if !mode.valid() {
		return fmt.Errorf("invalid approval mode %d", mode)
	}
	return mutateApprovalPreferences(store.path, func(preferences map[string]any) error {
		modes, _ := preferences[threadApprovalModesKey].(map[string]any)
		if modes == nil {
			modes = map[string]any{}
		}
		modes[approvalModeKey(threadID)] = map[string]any{"mode": mode.String()}
		preferences[threadApprovalModesKey] = modes
		return nil
	})
}

func updateApprovalPreferences(path string, updates map[string]any) error {
	return mutateApprovalPreferences(path, func(preferences map[string]any) error {
		for key, value := range updates {
			preferences[key] = value
		}
		return nil
	})
}

func mutateApprovalPreferences(path string, mutate func(map[string]any) error) error {
	approvalPreferencesMu.Lock()
	defer approvalPreferencesMu.Unlock()
	unlock, err := lockApprovalPreferences(path)
	if err != nil {
		return err
	}
	defer unlock()

	preferences, err := loadApprovalPreferences(path)
	if err != nil {
		return err
	}
	if err := mutate(preferences); err != nil {
		return err
	}
	preferences["version"] = 1
	return saveApprovalPreferences(path, preferences)
}

func loadApprovalPreferences(path string) (map[string]any, error) {
	preferences := map[string]any{}
	if path == "" {
		return preferences, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return preferences, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect approval preferences: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("approval preferences must be a regular file")
	}
	if info.Size() > maxApprovalPreferencesSize {
		return nil, errors.New("approval preferences exceed 1 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open approval preferences: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxApprovalPreferencesSize+1))
	if err != nil {
		return nil, fmt.Errorf("read approval preferences: %w", err)
	}
	if len(data) > maxApprovalPreferencesSize {
		return nil, errors.New("approval preferences exceed 1 MiB")
	}
	if err := json.Unmarshal(data, &preferences); err != nil {
		return nil, fmt.Errorf("decode approval preferences: %w", err)
	}
	if preferences == nil {
		preferences = map[string]any{}
	}
	return preferences, nil
}

func saveApprovalPreferences(path string, preferences map[string]any) error {
	if path == "" {
		return errors.New("approval preferences path is required")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create approval preferences directory: %w", err)
	}
	data, err := json.MarshalIndent(preferences, "", "  ")
	if err != nil {
		return fmt.Errorf("encode approval preferences: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxApprovalPreferencesSize {
		return errors.New("approval preferences exceed 1 MiB")
	}
	temporary, err := os.CreateTemp(directory, ".approval-*.json")
	if err != nil {
		return fmt.Errorf("create temporary approval preferences: %w", err)
	}
	temporaryPath := temporary.Name()
	defer temporary.Close()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure approval preferences: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write approval preferences: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync approval preferences: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close approval preferences: %w", err)
	}
	if err := replaceFileDurably(temporaryPath, path); err != nil {
		return fmt.Errorf("replace approval preferences: %w", err)
	}
	return nil
}

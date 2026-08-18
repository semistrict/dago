package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damodel"
)

const reasoningEffortFilename = "reasoning-effort.json"

type reasoningEffortContext struct {
	ModelSpec string
	Levels    []string
	Current   string
	Default   string
}

type reasoningEffortManager struct {
	sync.RWMutex
	profile damodel.Profile
	path    string
	current string
}

func newReasoningEffortManager(profile damodel.Profile, path string) *reasoningEffortManager {
	manager := &reasoningEffortManager{profile: profile, path: path}
	preferences, err := loadReasoningEfforts(path)
	if err == nil {
		manager.current = manager.match(preferences[manager.modelSpec()])
	}
	return manager
}

func (manager *reasoningEffortManager) Context() reasoningEffortContext {
	if manager == nil {
		return reasoningEffortContext{}
	}
	manager.RLock()
	defer manager.RUnlock()
	return reasoningEffortContext{
		ModelSpec: manager.modelSpec(),
		Levels:    append([]string(nil), manager.profile.ReasoningLevels...),
		Current:   manager.current,
		Default:   manager.match(manager.profile.DefaultReasoningLevel),
	}
}

func (manager *reasoningEffortManager) Set(level string) error {
	if manager == nil {
		return errors.New("reasoning effort is unavailable")
	}
	manager.Lock()
	defer manager.Unlock()
	matched := ""
	if level != "" {
		matched = manager.match(level)
		if matched == "" {
			return fmt.Errorf("unsupported reasoning effort %q", level)
		}
	}
	manager.current = matched
	preferences, err := loadReasoningEfforts(manager.path)
	if err != nil {
		return err
	}
	if matched == "" {
		delete(preferences, manager.modelSpec())
	} else {
		preferences[manager.modelSpec()] = matched
	}
	return saveReasoningEfforts(manager.path, preferences)
}

func (manager *reasoningEffortManager) Middleware() dagent.Middleware {
	return dagent.Middleware{
		Name: "_dacode_reasoning_effort",
		WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
			manager.RLock()
			effort := manager.current
			manager.RUnlock()
			if effort != "" {
				reasoning := damodel.Reasoning{Effort: effort}
				if request.Reasoning != nil {
					reasoning = *request.Reasoning
					reasoning.Effort = effort
				}
				request.Reasoning = &reasoning
			}
			return next(ctx, request)
		},
	}
}

func (manager *reasoningEffortManager) modelSpec() string {
	if manager.profile.Model == "" {
		return ""
	}
	if manager.profile.Provider == "" {
		return manager.profile.Model
	}
	return manager.profile.Provider + ":" + manager.profile.Model
}

func (manager *reasoningEffortManager) match(level string) string {
	for _, supported := range manager.profile.ReasoningLevels {
		if strings.EqualFold(level, supported) {
			return supported
		}
	}
	return ""
}

func loadReasoningEfforts(path string) (map[string]string, error) {
	preferences := map[string]string{}
	if path == "" {
		return preferences, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return preferences, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read reasoning effort preferences: %w", err)
	}
	if len(data) > 1<<20 {
		return nil, errors.New("reasoning effort preferences exceed 1 MiB")
	}
	if err := json.Unmarshal(data, &preferences); err != nil {
		return nil, fmt.Errorf("decode reasoning effort preferences: %w", err)
	}
	if preferences == nil {
		preferences = map[string]string{}
	}
	return preferences, nil
}

func saveReasoningEfforts(path string, preferences map[string]string) error {
	if path == "" {
		return nil
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create reasoning effort directory: %w", err)
	}
	keys := make([]string, 0, len(preferences))
	for key := range preferences {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	ordered := make(map[string]string, len(keys))
	for _, key := range keys {
		ordered[key] = preferences[key]
	}
	data, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return fmt.Errorf("encode reasoning effort preferences: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".reasoning-effort-*.json")
	if err != nil {
		return fmt.Errorf("create reasoning effort preferences: %w", err)
	}
	temporaryPath := temporary.Name()
	defer temporary.Close()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure reasoning effort preferences: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write reasoning effort preferences: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync reasoning effort preferences: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close reasoning effort preferences: %w", err)
	}
	if err := replaceFileDurably(temporaryPath, path); err != nil {
		return fmt.Errorf("replace reasoning effort preferences: %w", err)
	}
	return nil
}

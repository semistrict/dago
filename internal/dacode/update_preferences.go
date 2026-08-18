package dacode

import (
	"bytes"
	"errors"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

const autoUpdateEnvironment = "DEEPAGENTS_CODE_AUTO_UPDATE"

type autoUpdatePreference struct {
	Enabled  bool
	Explicit bool
	Source   string
}

type autoUpdatePreferenceStore struct{ path string }

func newAutoUpdatePreferenceStore(path string) *autoUpdatePreferenceStore {
	if strings.TrimSpace(path) == "" {
		panic("dacode: auto-update preference path is required")
	}
	return &autoUpdatePreferenceStore{path: path}
}

func loadAutoUpdatePreference(path string, lookup func(string) (string, bool)) (autoUpdatePreference, []string) {
	if lookup != nil {
		if raw, exists := lookup(autoUpdateEnvironment); exists {
			if value, ok := parseAutoUpdateBool(raw); ok {
				return autoUpdatePreference{Enabled: value, Explicit: true, Source: "environment"}, nil
			}
			preference, diagnostics := loadAutoUpdateConfig(path)
			return preference, append([]string{"invalid auto-update override; using saved preference"}, diagnostics...)
		}
	}
	return loadAutoUpdateConfig(path)
}

// parseAutoUpdateBool retains the pinned override contract: a present but
// explicitly empty environment value disables automatic updates. Other values
// use the shared permissive boolean spellings.
func parseAutoUpdateBool(raw string) (bool, bool) {
	if strings.TrimSpace(raw) == "" {
		return false, true
	}
	return parseOnboardingBool(raw)
}

func loadAutoUpdateConfig(path string) (autoUpdatePreference, []string) {
	document, err := readThemeDocument(path)
	if errors.Is(err, os.ErrNotExist) {
		return autoUpdatePreference{Enabled: true, Source: "default"}, nil
	}
	if err != nil {
		return autoUpdatePreference{Source: "unavailable"}, []string{"auto-update preference is unavailable; automatic updates are disabled"}
	}
	update, exists := document["update"]
	if !exists {
		return autoUpdatePreference{Enabled: true, Source: "default"}, nil
	}
	table, ok := update.(map[string]any)
	if !ok {
		return autoUpdatePreference{Source: "unavailable"}, []string{"auto-update preference is malformed; automatic updates are disabled"}
	}
	value, exists := table["auto_update"]
	if !exists {
		return autoUpdatePreference{Enabled: true, Source: "default"}, nil
	}
	enabled, ok := value.(bool)
	if !ok {
		return autoUpdatePreference{Source: "unavailable"}, []string{"auto-update preference is malformed; automatic updates are disabled"}
	}
	return autoUpdatePreference{Enabled: enabled, Explicit: true, Source: "config"}, nil
}

func (store *autoUpdatePreferenceStore) set(enabled bool) error {
	if store == nil || strings.TrimSpace(store.path) == "" {
		return errors.New("auto-update preference path is unavailable")
	}
	configurationPreferenceWriteMu.Lock()
	defer configurationPreferenceWriteMu.Unlock()
	document, err := readThemeDocument(store.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("read auto-update preference")
	}
	if document == nil {
		document = map[string]any{}
	}
	update, ok := document["update"].(map[string]any)
	if !ok {
		if _, exists := document["update"]; exists {
			return errors.New("auto-update preference is malformed")
		}
		update = map[string]any{}
		document["update"] = update
	}
	update["auto_update"] = enabled
	var payload bytes.Buffer
	if err := toml.NewEncoder(&payload).Encode(document); err != nil || payload.Len() > maxThemeConfigSize {
		return errors.New("encode auto-update preference")
	}
	return replaceThemeConfig(store.path, payload.Bytes())
}

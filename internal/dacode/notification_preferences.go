package dacode

import (
	"bytes"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

const (
	warningRipgrep = "ripgrep"
	warningTavily  = "tavily"
	warningYOLO    = "yolo"
)

var configurationPreferenceWriteMu sync.Mutex

type notificationPreferenceStore struct {
	path             string
	generationMu     sync.Mutex
	latestGeneration map[string]uint64
}

// notificationPreferenceWrite identifies one optimistic settings change. The
// generation is caller-opaque and lets the TUI ignore late success/failure
// messages without allowing an older disk write to overwrite newer intent.
type notificationPreferenceWrite struct {
	Key        string
	Enabled    bool
	Generation uint64
}

func newNotificationPreferenceStore(path string) *notificationPreferenceStore {
	if strings.TrimSpace(path) == "" {
		panic("dacode: notification preference path is required")
	}
	return &notificationPreferenceStore{path: path, latestGeneration: map[string]uint64{}}
}

func (store *notificationPreferenceStore) beginWarningEnabled(key string, enabled bool) notificationPreferenceWrite {
	if store == nil || !validWarningKey(key) {
		panic("dacode: notification warning preference is required")
	}
	store.generationMu.Lock()
	defer store.generationMu.Unlock()
	generation := store.latestGeneration[key] + 1
	if generation == 0 {
		generation = 1
	}
	store.latestGeneration[key] = generation
	return notificationPreferenceWrite{Key: key, Enabled: enabled, Generation: generation}
}

func (store *notificationPreferenceStore) currentWarningWrite(write notificationPreferenceWrite) bool {
	if store == nil || !validWarningKey(write.Key) || write.Generation == 0 {
		return false
	}
	store.generationMu.Lock()
	defer store.generationMu.Unlock()
	return store.latestGeneration[write.Key] == write.Generation
}

// saveWarningEnabled persists only the latest registered intent for a key.
// Callers must also check currentWarningWrite when applying the asynchronous
// result because a newer intent may be registered after this method returns.
func (store *notificationPreferenceStore) saveWarningEnabled(write notificationPreferenceWrite) error {
	if store == nil || !validWarningKey(write.Key) || write.Generation == 0 {
		return errors.New("notification warning preference is unavailable")
	}
	configurationPreferenceWriteMu.Lock()
	defer configurationPreferenceWriteMu.Unlock()
	if !store.currentWarningWrite(write) {
		return nil
	}
	return store.setWarningEnabledLocked(write.Key, write.Enabled)
}

func loadSuppressedWarnings(path string) (map[string]bool, []string) {
	document, err := readThemeDocument(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]bool{}, nil
		}
		return map[string]bool{}, []string{"notification preferences are unavailable; warnings remain enabled"}
	}
	suppressed := map[string]bool{}
	warnings, _ := document["warnings"].(map[string]any)
	values, exists := warnings["suppress"]
	if !exists {
		return suppressed, nil
	}
	items, ok := values.([]map[string]any)
	if ok {
		// BurntSushi/toml does not decode scalar arrays into this shape; retain
		// the branch only to reject a misleading table-array representation.
		_ = items
		return suppressed, []string{"notification suppression preferences are malformed; warnings remain enabled"}
	}
	list, ok := values.([]any)
	if !ok {
		return suppressed, []string{"notification suppression preferences are malformed; warnings remain enabled"}
	}
	for _, value := range list {
		key, ok := value.(string)
		if !ok || !validWarningKey(key) {
			return map[string]bool{}, []string{"notification suppression preferences are malformed; warnings remain enabled"}
		}
		suppressed[key] = true
	}
	return suppressed, nil
}

func (store *notificationPreferenceStore) setWarningEnabled(key string, enabled bool) error {
	if store == nil || strings.TrimSpace(store.path) == "" {
		return errors.New("notification preference path is unavailable")
	}
	if !validWarningKey(key) {
		return errors.New("notification warning is unavailable")
	}
	configurationPreferenceWriteMu.Lock()
	defer configurationPreferenceWriteMu.Unlock()
	return store.setWarningEnabledLocked(key, enabled)
}

func (store *notificationPreferenceStore) setWarningEnabledLocked(key string, enabled bool) error {
	document, err := readThemeDocument(store.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("read notification preferences")
	}
	if document == nil {
		document = map[string]any{}
	}
	warnings, ok := document["warnings"].(map[string]any)
	if !ok {
		warnings = map[string]any{}
		document["warnings"] = warnings
	}
	suppressed, diagnostics := suppressedWarningSet(warnings["suppress"])
	if len(diagnostics) != 0 {
		return errors.New("notification suppression preferences are malformed")
	}
	if enabled {
		delete(suppressed, key)
	} else {
		suppressed[key] = true
	}
	keys := make([]string, 0, len(suppressed))
	for suppressedKey := range suppressed {
		keys = append(keys, suppressedKey)
	}
	sort.Strings(keys)
	warnings["suppress"] = keys
	var payload bytes.Buffer
	if err := toml.NewEncoder(&payload).Encode(document); err != nil || payload.Len() > maxThemeConfigSize {
		return errors.New("encode notification preferences")
	}
	return replaceThemeConfig(store.path, payload.Bytes())
}

func suppressedWarningSet(value any) (map[string]bool, []string) {
	result := map[string]bool{}
	if value == nil {
		return result, nil
	}
	list, ok := value.([]any)
	if !ok {
		return result, []string{"malformed"}
	}
	for _, item := range list {
		key, ok := item.(string)
		if !ok || !validWarningKey(key) {
			return map[string]bool{}, []string{"malformed"}
		}
		result[key] = true
	}
	return result, nil
}

func validWarningKey(key string) bool {
	switch key {
	case warningRipgrep, warningTavily, warningYOLO:
		return true
	default:
		return false
	}
}

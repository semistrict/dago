package dacode

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/semistrict/dago/daenv"
)

type cliEnvironmentOverlay struct {
	mu                         sync.Mutex
	workingDir                 string
	baseline                   []string
	baseNames                  map[string]bool
	applied                    map[string]string
	ignoreInvalidProjectDotenv bool
	closed                     bool
}

func newCLIEnvironmentOverlay(workingDir string, stderr io.Writer) (*cliEnvironmentOverlay, error) {
	return newCLIEnvironmentOverlayWithOptions(workingDir, stderr, false)
}

func newCLIEnvironmentOverlayWithOptions(workingDir string, stderr io.Writer, ignoreInvalidProjectDotenv bool) (*cliEnvironmentOverlay, error) {
	if stderr == nil {
		panic("dacode: environment diagnostic writer is required")
	}
	baseline := append([]string(nil), os.Environ()...)
	overlay := &cliEnvironmentOverlay{
		workingDir: workingDir, baseline: baseline, baseNames: map[string]bool{}, applied: map[string]string{},
		ignoreInvalidProjectDotenv: ignoreInvalidProjectDotenv,
	}
	for _, entry := range baseline {
		if key, _, ok := strings.Cut(entry, "="); ok {
			overlay.baseNames[cliEnvironmentIdentity(key)] = true
		}
	}
	rollback, _, err := overlay.Reload(stderr)
	if err != nil {
		return nil, err
	}
	_ = rollback // Initial application is committed for the process lifetime.
	return overlay, nil
}

// Reload applies a fresh side-effect-free dotenv resolution over the original
// process environment. The returned rollback restores the prior overlay if a
// later runtime build fails; callers commit by discarding it.
func (overlay *cliEnvironmentOverlay) Reload(stderr io.Writer) (func(), []string, error) {
	if overlay == nil || stderr == nil {
		panic("dacode: environment overlay and diagnostic writer are required")
	}
	resolved, err := daenv.Load(overlay.workingDir, overlay.baseline, daenv.Options{})
	if err != nil && overlay.ignoreInvalidProjectDotenv {
		withoutProject, fallbackErr := daenv.Load(overlay.workingDir, overlay.baseline, daenv.Options{SkipProjectFile: true})
		if fallbackErr == nil {
			if _, writeErr := fmt.Fprintf(stderr, "Ignoring invalid project dotenv file: %v.\n", err); writeErr != nil {
				return nil, nil, writeErr
			}
			resolved, err = withoutProject, nil
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load environment: %w", err)
	}
	for _, ignored := range resolved.Ignored {
		if _, err := fmt.Fprintf(stderr, "Ignoring dotenv key %q: %s.\n", ignored.Key, ignored.Reason); err != nil {
			return nil, nil, err
		}
	}
	desired := map[string]string{}
	for key, value := range resolved.Values {
		if !overlay.baseNames[cliEnvironmentIdentity(key)] {
			desired[key] = value
		}
	}

	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	if overlay.closed {
		return nil, nil, fmt.Errorf("environment overlay is closed")
	}
	previous := cloneEnvironmentValues(overlay.applied)
	if err := applyEnvironmentValues(previous, desired); err != nil {
		_ = applyEnvironmentValues(desired, previous)
		return nil, nil, err
	}
	overlay.applied = cloneEnvironmentValues(desired)
	changes := environmentChangeSummary(previous, desired)
	var once sync.Once
	rollback := func() {
		once.Do(func() {
			overlay.mu.Lock()
			defer overlay.mu.Unlock()
			if overlay.closed {
				return
			}
			_ = applyEnvironmentValues(overlay.applied, previous)
			overlay.applied = cloneEnvironmentValues(previous)
		})
	}
	return rollback, changes, nil
}

func (overlay *cliEnvironmentOverlay) Close() error {
	if overlay == nil {
		return nil
	}
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	if overlay.closed {
		return nil
	}
	overlay.closed = true
	err := applyEnvironmentValues(overlay.applied, nil)
	overlay.applied = nil
	return err
}

func loadCLIEnvironment(workingDir string, stderr io.Writer) (func(), error) {
	overlay, err := newCLIEnvironmentOverlay(workingDir, stderr)
	if err != nil {
		return nil, err
	}
	return func() { _ = overlay.Close() }, nil
}

func applyEnvironmentValues(current, desired map[string]string) error {
	for key := range current {
		if _, keep := desired[key]; !keep {
			if err := os.Unsetenv(key); err != nil {
				return fmt.Errorf("remove dotenv key %q: %w", key, err)
			}
		}
	}
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if current[key] == desired[key] {
			continue
		}
		if err := os.Setenv(key, desired[key]); err != nil {
			return fmt.Errorf("apply dotenv key %q: %w", key, err)
		}
	}
	return nil
}

func environmentChangeSummary(previous, current map[string]string) []string {
	changed := map[string]bool{}
	for key, value := range previous {
		if next, exists := current[key]; !exists || next != value {
			changed[key] = true
		}
	}
	for key, value := range current {
		if before, exists := previous[key]; !exists || before != value {
			changed[key] = true
		}
	}
	keys := make([]string, 0, len(changed))
	for key := range changed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, len(keys))
	for index, key := range keys {
		result[index] = "Environment key " + key + " changed"
	}
	return result
}

func cliEnvironmentIdentity(value string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(value)
	}
	return value
}

func cloneEnvironmentValues(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

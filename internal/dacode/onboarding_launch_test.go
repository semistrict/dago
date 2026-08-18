package dacode

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/daconfig"
)

func TestDecideOnboardingLaunchPrecedenceAndFirstRun(t *testing.T) {
	stateDirectory := filepath.Join(t.TempDir(), "state")
	unset := onboardingConfigSnapshot(t, nil, nil)
	show, diagnostics := decideOnboardingLaunch(stateDirectory, true, false, unset, mapLookup(nil))
	if !show || len(diagnostics) != 0 {
		t.Fatalf("first-run decision = %v, %v", show, diagnostics)
	}
	if err := markOnboardingComplete(stateDirectory); err != nil {
		t.Fatal(err)
	}
	show, diagnostics = decideOnboardingLaunch(stateDirectory, true, false, unset, mapLookup(nil))
	if show || len(diagnostics) != 0 {
		t.Fatalf("completed decision = %v, %v", show, diagnostics)
	}

	forcedOff := onboardingConfigSnapshot(t, map[string]any{onboardingConfigKey: false}, nil)
	show, diagnostics = decideOnboardingLaunch(filepath.Join(t.TempDir(), "fresh"), true, false, forcedOff, mapLookup(nil))
	if show || len(diagnostics) != 0 {
		t.Fatalf("configured suppression = %v, %v", show, diagnostics)
	}
	show, diagnostics = decideOnboardingLaunch(stateDirectory, true, true, forcedOff, mapLookup(nil))
	if !show || len(diagnostics) != 0 {
		t.Fatalf("explicit request = %v, %v", show, diagnostics)
	}
}

func TestDecideOnboardingLaunchEnvironmentAndNonInteractiveBounds(t *testing.T) {
	stateDirectory := filepath.Join(t.TempDir(), "state")
	enabled := onboardingConfigSnapshot(t, nil, map[string]string{onboardingEnvironment: "yes"})
	show, diagnostics := decideOnboardingLaunch(stateDirectory, true, false, enabled, mapLookup(nil))
	if !show || len(diagnostics) != 0 {
		t.Fatalf("environment force = %v, %v", show, diagnostics)
	}

	show, diagnostics = decideOnboardingLaunch(stateDirectory, false, true, enabled, mapLookup(nil))
	if show || len(diagnostics) != 0 {
		t.Fatalf("non-interactive decision = %v, %v", show, diagnostics)
	}

	invalid := onboardingConfigSnapshot(t, nil, map[string]string{onboardingEnvironment: "sometimes"})
	show, diagnostics = decideOnboardingLaunch(stateDirectory, true, false, invalid, mapLookup(map[string]string{onboardingEnvironment: "sometimes"}))
	if !show || len(diagnostics) != 1 {
		t.Fatalf("invalid environment fallback = %v, %v", show, diagnostics)
	}
}

func TestOnboardingConfigurationIsCanonicalAndIntrospectable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := daconfig.NewStore(cliConfigManifest, path, daconfig.StoreOptions{})
	if err := store.Set(t.Context(), onboardingConfigKey, false); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveCLIConfig(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	entries := resolved.snapshot.Select(onboardingConfigKey)
	if len(entries) != 1 || !entries[0].Set || entries[0].Value != false || !strings.HasPrefix(entries[0].Source, "file:") {
		t.Fatalf("onboarding configuration = %#v", entries)
	}
	if err := runConfigCommand(t.Context(), []string{"get", onboardingConfigKey, "--config", path}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func onboardingConfigSnapshot(t *testing.T, values map[string]any, environment map[string]string) daconfig.Snapshot {
	t.Helper()
	layers := []daconfig.Layer(nil)
	if values != nil {
		layers = append(layers, daconfig.NewLayer("file", values))
	}
	resolver := daconfig.NewResolver(cliConfigManifest, mapLookup(environment), daconfig.ResolverOptions{})
	snapshot, err := resolver.Resolve(layers, daconfig.Layer{})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

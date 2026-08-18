package daconfig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func testManifest() *Manifest {
	return NewManifest(
		Option{Key: "models.default", Group: "Models", Summary: "Default model", Kind: KindString, Environment: "OPENAI_MODEL", Default: "gpt-default", Persist: true},
		Option{Key: "runtime.recursion_limit", Group: "Runtime", Summary: "Graph step limit", Kind: KindInt, Environment: "DAGO_RECURSION_LIMIT", Default: 2000, Minimum: 25, Maximum: 100000, Persist: true},
		Option{Key: "memory.auto_save", Group: "Memory", Summary: "Memory writes", Kind: KindBool, Environment: "DAGO_MEMORY_AUTO_SAVE", Default: true, Persist: true},
		Option{Key: "credentials.openai", Group: "Credentials", Summary: "OpenAI key", Kind: KindString, Environment: "OPENAI_API_KEY", Redacted: true},
	)
}

func TestResolverPrecedenceAndRedaction(t *testing.T) {
	environment := map[string]string{
		"OPENAI_MODEL":                      "canonical",
		CodePrefix + "OPENAI_MODEL":         "code",
		CLIPrefix + "OPENAI_MODEL":          "cli",
		CLIPrefix + "OPENAI_API_KEY":        "private-value",
		CLIPrefix + "DAGO_MEMORY_AUTO_SAVE": "",
	}
	resolver := NewResolver(testManifest(), func(name string) (string, bool) {
		value, ok := environment[name]
		return value, ok
	}, ResolverOptions{})
	snapshot, err := resolver.Resolve(
		[]Layer{NewLayer("global", map[string]any{"models.default": "file", "runtime.recursion_limit": 3000})},
		NewLayer("cli", map[string]any{"runtime.recursion_limit": "4000"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.String("models.default") != "cli" || snapshot.Int("runtime.recursion_limit") != 4000 || snapshot.Bool("memory.auto_save") {
		t.Fatalf("snapshot = %#v", snapshot.Entries())
	}
	credential := snapshot.Select("credentials.openai")
	if len(credential) != 1 || !credential[0].Set || credential[0].Value != nil || credential[0].Source != "env:"+CLIPrefix+"OPENAI_API_KEY" {
		t.Fatalf("redacted credential = %#v", credential)
	}
	if snapshot.String("credentials.openai") != "private-value" {
		t.Fatal("runtime lookup did not retain the redacted value")
	}
}

func TestResolverSkipsMalformedEnvironmentButRejectsUnknownFileValues(t *testing.T) {
	resolver := NewResolver(testManifest(), func(name string) (string, bool) {
		if name == "DAGO_RECURSION_LIMIT" {
			return "3000", true
		}
		if name == CLIPrefix+"DAGO_RECURSION_LIMIT" {
			return "unbounded", true
		}
		return "", false
	}, ResolverOptions{})
	snapshot, err := resolver.Resolve(nil, Layer{})
	if err != nil || snapshot.Int("runtime.recursion_limit") != 3000 {
		t.Fatalf("malformed environment fallback = %d, %v", snapshot.Int("runtime.recursion_limit"), err)
	}
	resolver = NewResolver(testManifest(), func(string) (string, bool) { return "", false }, ResolverOptions{})
	if _, err := resolver.Resolve([]Layer{NewLayer("file", map[string]any{"unknown.option": true})}, Layer{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("unknown option error = %v", err)
	}
}

func TestLayerAndManifestAreDefensiveCopies(t *testing.T) {
	values := map[string]any{"models.default": "file"}
	layer := NewLayer("file", values)
	values["models.default"] = "mutated input"
	copy := layer.Values()
	copy["models.default"] = "mutated output"

	manifest := testManifest()
	options := manifest.Options()
	options[0].Default = "mutated manifest"
	snapshot, err := NewResolver(manifest, func(string) (string, bool) { return "", false }, ResolverOptions{}).Resolve([]Layer{layer}, Layer{})
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.String("models.default"); got != "file" {
		t.Fatalf("defensive value = %q", got)
	}
	if got, _ := manifest.Option("models.default"); got.Default != "gpt-default" {
		t.Fatalf("defensive manifest default = %#v", got.Default)
	}
}

func TestManifestAndResolverStaticInputsFailAtConstruction(t *testing.T) {
	for name, operation := range map[string]func(){
		"nil manifest": func() { NewResolver(nil, os.LookupEnv, ResolverOptions{}) },
		"nil lookup":   func() { NewResolver(testManifest(), nil, ResolverOptions{}) },
		"duplicate": func() {
			option := Option{Key: "test.value", Group: "Test", Summary: "Value", Kind: KindString}
			NewManifest(option, option)
		},
		"bad bounds": func() {
			NewManifest(Option{Key: "test.value", Group: "Test", Summary: "Value", Kind: KindInt, Minimum: 5, Maximum: 2})
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			operation()
		})
	}
}

func TestStoreRoundTripAtomicPermissionsAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	store := NewStore(testManifest(), path, StoreOptions{})
	if layer, err := store.Load(t.Context()); err != nil || len(layer.Values()) != 0 {
		t.Fatalf("missing load = %#v, %v", layer, err)
	}
	if err := store.Set(t.Context(), "models.default", "selected"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(t.Context(), "runtime.recursion_limit", "9000"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, err = %v", info.Mode().Perm(), err)
	}
	layer, err := store.Load(t.Context())
	if err != nil || !reflect.DeepEqual(layer.Values(), map[string]any{"models.default": "selected", "runtime.recursion_limit": 9000}) {
		t.Fatalf("loaded layer = %#v, %v", layer, err)
	}
	if err := store.Set(t.Context(), "credentials.openai", "do-not-store"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("credential store error = %v", err)
	}
	if _, err := store.Unset(t.Context(), "credentials.openai"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("credential unset error = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.Set(canceled, "models.default", "ignored"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled set error = %v", err)
	}
	if removed, err := store.Unset(t.Context(), "models.default"); err != nil || !removed {
		t.Fatalf("unset = %v, %v", removed, err)
	}
}

func TestStoreRejectsSymlinkOversizeUnknownAndTrailingData(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte(`{"version":1,"values":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "config.json")
	if err := os.Symlink(outside, symlink); err == nil {
		store := NewStore(testManifest(), symlink, StoreOptions{})
		if _, err := store.Load(t.Context()); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("symlink load error = %v", err)
		}
	}
	for name, content := range map[string]string{
		"unknown":            `{"version":1,"values":{"not.real":true}}`,
		"noncanonical key":   `{"version":1,"values":{"Models.Default":"one"}}`,
		"unknown envelope":   `{"version":1,"values":{},"extra":true}`,
		"duplicate envelope": `{"version":1,"version":1,"values":{}}`,
		"duplicate value":    `{"version":1,"values":{"models.default":"one","models.default":"two"}}`,
		"trailing":           `{"version":1,"values":{}} {}`,
		"version":            `{"version":99,"values":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewStore(testManifest(), path, StoreOptions{}).Load(t.Context()); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("load error = %v", err)
			}
		})
	}
}

func TestServerConfigRoundTripAndTamperChecks(t *testing.T) {
	zeroEnvironment := (ServerConfig{}).Environment()
	if zeroEnvironment[ServerPrefix+"MEMORY_AUTO_SAVE"] != "true" || zeroEnvironment[ServerPrefix+"INTERACTIVE"] != "true" || zeroEnvironment[ServerPrefix+"RECURSION_LIMIT"] != "2000" {
		t.Fatalf("zero environment = %#v", zeroEnvironment)
	}
	config := DefaultServerConfig()
	config.Model = "openai:gpt-test"
	config.WorkingDirectory = filepath.Clean(t.TempDir())
	config.StateDirectory = filepath.Join(config.WorkingDirectory, "state")
	config.ShellAllowList = []string{"git", "go"}
	environment := config.Environment()
	decoded, err := ServerConfigFromEnvironment(func(name string) (string, bool) {
		value, ok := environment[name]
		return value, ok
	})
	if err != nil || !reflect.DeepEqual(decoded, config) {
		t.Fatalf("round trip = %#v, %v", decoded, err)
	}
	for _, test := range []struct{ name, value string }{
		{ServerPrefix + "RECURSION_LIMIT", "100001"},
		{ServerPrefix + "INTERACTIVE", "yes"},
		{ServerPrefix + "SHELL_ALLOW_LIST", `{"command":"git"}`},
		{ServerPrefix + "SHELL_ALLOW_LIST", `[]`},
	} {
		t.Run(test.name+test.value, func(t *testing.T) {
			if _, err := ServerConfigFromEnvironment(func(key string) (string, bool) { return test.value, key == test.name }); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("tampered value error = %v", err)
			}
		})
	}
}

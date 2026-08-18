package dacode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/daconfig"
)

func TestConfigCommandManagesAndIntrospectsEffectiveValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var stdout bytes.Buffer
	if err := runConfigCommand(t.Context(), []string{"set", "models.default", "file-model", "--config", path}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Updated models.default") {
		t.Fatalf("set output = %q", stdout.String())
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, err = %v", info.Mode().Perm(), err)
	}

	t.Setenv("OPENAI_MODEL", "canonical-model")
	t.Setenv(daconfig.CodePrefix+"OPENAI_MODEL", "code-model")
	t.Setenv(daconfig.CLIPrefix+"OPENAI_MODEL", "cli-model")
	stdout.Reset()
	if err := runConfigCommand(t.Context(), []string{"get", "models.default", "--json", "--config=" + path}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Version int    `json:"version"`
		Command string `json:"command"`
		Data    struct {
			Key, Source, Value string
			Set                bool
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != 1 || envelope.Command != "config get" || envelope.Data.Key != "models.default" ||
		envelope.Data.Value != "cli-model" || envelope.Data.Source != "env:"+daconfig.CLIPrefix+"OPENAI_MODEL" || !envelope.Data.Set {
		t.Fatalf("config get = %#v", envelope)
	}

	stdout.Reset()
	if err := runConfigCommand(t.Context(), []string{"unset", "models.default", "--json", "--config", path}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"removed":true`) {
		t.Fatalf("unset output = %s", stdout.String())
	}
}

func TestConfigCommandNeverPrintsCredentialValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	credential := "private-test-value"
	t.Setenv(daconfig.CLIPrefix+"OPENAI_API_KEY", credential)
	for _, arguments := range [][]string{
		{"--json", "--config", path},
		{"get", "credentials.openai", "--json", "--verbose", "--config", path},
		{"get", "credentials", "--config", path},
	} {
		var stdout bytes.Buffer
		if err := runConfigCommand(t.Context(), arguments, &stdout, io.Discard); err != nil {
			t.Fatalf("run %#v: %v", arguments, err)
		}
		if strings.Contains(stdout.String(), credential) || !strings.Contains(stdout.String(), "set") {
			t.Fatalf("credential output for %#v = %q", arguments, stdout.String())
		}
	}
	if err := runConfigCommand(t.Context(), []string{"set", "credentials.openai", credential, "--config", path}, io.Discard, io.Discard); !errors.Is(err, daconfig.ErrInvalidConfig) {
		t.Fatalf("persist credential error = %v", err)
	}
}

func TestConfigCommandPathAndUsageErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var stdout bytes.Buffer
	if err := runConfigCommand(t.Context(), []string{"path", "--json", "--config", path}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), filepath.Base(path)) || !strings.Contains(stdout.String(), `"exists":false`) {
		t.Fatalf("path output = %s", stdout.String())
	}
	for _, arguments := range [][]string{{"get"}, {"set", "models.default"}, {"unset"}, {"--unknown"}, {"missing"}} {
		if err := runConfigCommand(t.Context(), arguments, io.Discard, io.Discard); err == nil {
			t.Errorf("arguments %#v were accepted", arguments)
		}
	}
	if _, err := parseCLI([]string{"--config="}, io.Discard); err == nil {
		t.Fatal("empty runtime config path was accepted")
	}
}

func TestRunDispatchesConfigWithoutStartingRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var stdout, stderr bytes.Buffer
	if err := Run(t.Context(), []string{"config", "get", "models.default", "--json", "--config", path}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"command":"config get"`) || stderr.Len() != 0 {
		t.Fatalf("config dispatch stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestParseCLIResolvedAppliesLayersThenExplicitFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := daconfig.NewStore(cliConfigManifest, path, daconfig.StoreOptions{})
	for key, value := range map[string]any{
		"models.default":            "file-model",
		"models.approval":           "file-review",
		"models.base_url":           "https://file.example.test/v1",
		"runtime.recursion_limit":   4321,
		"memory.auto_save":          false,
		"security.shell_allow_list": "recommended",
		"startup.command":           "prepare",
	} {
		if err := store.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := resolveCLIConfig(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	options, err := parseCLIResolved([]string{"--model", "flag-model", "--recursion-limit", "9999", "--memory-auto-save=true"}, io.Discard, &resolved)
	if err != nil {
		t.Fatal(err)
	}
	if options.model != "flag-model" || options.approvalModel != "file-review" || options.baseURL != "https://file.example.test/v1" ||
		options.recursionLimit != 9999 || !options.memoryAutoSave || options.startupCommand != "prepare" || !options.shellAllowList.restrictive() {
		t.Fatalf("resolved options = %#v", options)
	}
}

func TestConfigPrefixEmptyShadowsCanonicalAndConfigPathPrecedence(t *testing.T) {
	t.Setenv("OPENAI_MODEL", "canonical")
	t.Setenv(daconfig.CLIPrefix+"OPENAI_MODEL", "")
	options, err := parseCLI(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.model != "" {
		t.Fatalf("empty prefixed model = %q", options.model)
	}
	canonical := filepath.Join(t.TempDir(), "canonical.json")
	code := filepath.Join(t.TempDir(), "code.json")
	cli := filepath.Join(t.TempDir(), "cli.json")
	t.Setenv("DACODE_CONFIG", canonical)
	t.Setenv(daconfig.CodePrefix+"DACODE_CONFIG", code)
	t.Setenv(daconfig.CLIPrefix+"DACODE_CONFIG", cli)
	path, err := configuredPath("")
	if err != nil || path != cli {
		t.Fatalf("configured path = %q, %v", path, err)
	}
}

func TestServerConfigForCLIContainsNoCredential(t *testing.T) {
	options := cliOptions{
		model: "model", apiKey: "must-not-serialize", workingDir: filepath.Clean(t.TempDir()),
		stateDir: filepath.Join(t.TempDir(), "state"), recursionLimit: 2000, memoryAutoSave: true,
		shellAllowList: shellAllowList{commands: []string{"git"}},
	}
	environment := serverConfigFor(options, false).Environment()
	encoded, err := json.Marshal(environment)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), options.apiKey) || environment[daconfig.ServerPrefix+"MODEL"] != "model" {
		t.Fatalf("server environment = %s", encoded)
	}
}

func TestConfigCommandHonorsCancellationBeforeFileWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	path := filepath.Join(t.TempDir(), "config.json")
	err := runConfigCommand(ctx, []string{"set", "models.default", "ignored", "--config", path}, io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled command error = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled command created a file: %v", err)
	}
}

func TestVersionAndHelpDoNotDependOnReadableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DACODE_CONFIG", path)
	for _, arguments := range [][]string{{"--version"}, {"--help"}} {
		var stdout, stderr bytes.Buffer
		if err := Run(t.Context(), arguments, strings.NewReader(""), &stdout, &stderr); err != nil {
			t.Fatalf("Run(%#v) error = %v", arguments, err)
		}
		if stdout.Len()+stderr.Len() == 0 {
			t.Fatalf("Run(%#v) produced no output", arguments)
		}
	}
}

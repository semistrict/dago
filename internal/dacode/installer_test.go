package dacode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/dainstall"
)

type installExecutor struct{ commands []dainstall.Command }

func (executor *installExecutor) LookPath(string) (string, error) { return "/usr/local/bin/go", nil }
func (executor *installExecutor) Run(_ context.Context, command dainstall.Command) error {
	executor.commands = append(executor.commands, command)
	return nil
}

func TestDacodeInstallReportsCompiledInExtraWithoutMutation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := Run(t.Context(), []string{"install", "daytona"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 || !strings.Contains(stdout.String(), `extra "daytona" is already included`) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestDacodeInstallRefusesUnknownExtrasAndAllArbitraryPackages(t *testing.T) {
	for _, arguments := range [][]string{{"install", "not-listed"}, {"install", "name;touch-owned", "--yes"}, {"install", "anything", "--package", "--yes"}} {
		err := Run(t.Context(), arguments, strings.NewReader(""), ioDiscard{}, ioDiscard{})
		if !errors.Is(err, dainstall.ErrUnknownDependency) || ExitCode(err) != 2 {
			t.Errorf("Run(%#v) error=%v code=%d", arguments, err, ExitCode(err))
		}
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(value []byte) (int, error) { return len(value), nil }

func TestInstallCommandRequiresConfirmationForProcessAuthority(t *testing.T) {
	executor := &installExecutor{}
	installer := dainstall.New(executor, []dainstall.Spec{{Name: "helper", Kind: dainstall.Package, Executable: "go", Arguments: []string{"install", "example.invalid/helper@v1"}}}, dainstall.Options{})
	for _, input := range []string{"", "no\n", strings.Repeat("x", 17)} {
		err := executeInstallCommand(t.Context(), []string{"helper", "--package"}, strings.NewReader(input), ioDiscard{}, ioDiscard{}, installer)
		if err == nil {
			t.Errorf("input %q unexpectedly approved", input)
		}
	}
	if len(executor.commands) != 0 {
		t.Fatal("unapproved command executed")
	}
	var output bytes.Buffer
	if err := executeInstallCommand(t.Context(), []string{"helper", "--package"}, strings.NewReader("yes\n"), &output, ioDiscard{}, installer); err != nil {
		t.Fatal(err)
	}
	if len(executor.commands) != 1 || !strings.Contains(output.String(), `Installed package "helper"`) {
		t.Fatalf("commands=%#v output=%q", executor.commands, output.String())
	}
}

func TestInstallYesAndJSONAreExplicitAndStable(t *testing.T) {
	executor := &installExecutor{}
	installer := dainstall.New(executor, []dainstall.Spec{{Name: "helper", Kind: dainstall.Package, Executable: "go", Arguments: []string{"install", "example.invalid/helper@v1"}}}, dainstall.Options{})
	var output bytes.Buffer
	if err := executeInstallCommand(t.Context(), []string{"helper", "--package", "--yes", "--json"}, strings.NewReader(""), &output, ioDiscard{}, installer); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		SchemaVersion int              `json:"schema_version"`
		Command       string           `json:"command"`
		Data          dainstall.Result `json:"data"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 || envelope.Command != "install" || envelope.Data.Status != dainstall.Installed || len(executor.commands) != 1 {
		t.Fatalf("envelope=%#v commands=%#v", envelope, executor.commands)
	}
}

func TestInstallForceIsExplicitConfirmationAlias(t *testing.T) {
	executor := &installExecutor{}
	installer := dainstall.New(executor, []dainstall.Spec{{Name: "helper", Kind: dainstall.Package, Executable: "go", Arguments: []string{"install", "example.invalid/helper@v1"}}}, dainstall.Options{})
	if err := executeInstallCommand(t.Context(), []string{"helper", "--package", "--force"}, strings.NewReader(""), ioDiscard{}, ioDiscard{}, installer); err != nil {
		t.Fatal(err)
	}
	if len(executor.commands) != 1 {
		t.Fatalf("commands = %#v", executor.commands)
	}
}

func TestInstallHelpListsClosedCatalog(t *testing.T) {
	var output bytes.Buffer
	if err := Run(t.Context(), []string{"install", "--help"}, strings.NewReader(""), &output, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, expected := range []string{"Usage: dacode install", "daytona", "openai", "Allowlisted external packages: none"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("help missing %q:\n%s", expected, text)
		}
	}
}

func TestDacodeInstallCatalogIsExactlyTheIncludedGoExtras(t *testing.T) {
	installer := dainstall.New(&installExecutor{}, dacodeInstallCatalog(), dainstall.Options{})
	entries := installer.Available(dainstall.Extra)
	want := []string{"agentcore", "contexthub", "daytona", "docker", "langsmith", "media", "modal", "nemotron", "nvidia", "ollama", "openai", "openrouter", "quickjs", "runloop", "vercel"}
	if len(entries) != len(want) {
		t.Fatalf("entries = %#v", entries)
	}
	for index, name := range want {
		if entries[index].Name != name || !entries[index].BuiltIn {
			t.Fatalf("entries[%d] = %#v", index, entries[index])
		}
	}
	if packages := installer.Available(dainstall.Package); len(packages) != 0 {
		t.Fatalf("arbitrary package authority exposed: %#v", packages)
	}
}

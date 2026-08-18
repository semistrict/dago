package dacode

import (
	"context"
	"io"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/semistrict/dago/daconfig"
)

func TestLocalDevCLIParsesLiteralBoundedConfiguration(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "server")
	options, err := parseCLI([]string{
		"--local-dev-server", executable,
		"--local-dev-arg", "serve",
		"--local-dev-arg", "$(must-remain-literal)",
		"--local-dev-endpoint", "http://127.0.0.1:4312",
		"--local-dev-health-path", "/ready",
		"--local-dev-inherit-env", "SAFE_PARENT",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.localDevExecutable != executable || !reflect.DeepEqual(options.localDevArguments, []string{"serve", "$(must-remain-literal)"}) ||
		options.localDevEndpoint != "http://127.0.0.1:4312" || options.localDevHealthPath != "/ready" ||
		!reflect.DeepEqual(options.localDevInheritEnvironment, []string{"SAFE_PARENT"}) {
		t.Fatalf("options = %#v", options)
	}
}

func TestLocalDevCLIRejectsImplicitAuthorityAndUnsafeConfiguration(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "server")
	for _, arguments := range [][]string{
		{"--local-dev-arg", "serve"},
		{"--local-dev-server", "server-from-path"},
		{"--local-dev-server", executable, "--local-dev-endpoint", "http://example.com:4312"},
		{"--local-dev-server", executable, "--local-dev-inherit-env", "OPENAI_API_KEY"},
		{"--local-dev-server", executable, "--local-dev-inherit-env", "LD_PRELOAD"},
		{"--acp", "--local-dev-server", executable},
	} {
		if _, err := parseCLI(arguments, io.Discard); err == nil {
			t.Fatalf("parseCLI(%#v) succeeded", arguments)
		}
	}
}

func TestLocalDevRuntimePassesTypedCredentialFreeServerConfig(t *testing.T) {
	t.Setenv("SAFE_PARENT", "safe")
	t.Setenv("OPENAI_API_KEY", "must-not-cross")
	executable := filepath.Join(t.TempDir(), "server")
	workingDirectory := t.TempDir()
	stateDirectory := t.TempDir()
	runtime := newLocalDevRuntime(cliOptions{
		localDevExecutable: executable,
		localDevArguments:  []string{"serve", "--literal=$HOME"},
		localDevEndpoint:   "http://127.0.0.1:4312", localDevHealthPath: "/ok",
		localDevInheritEnvironment: []string{"SAFE_PARENT"},
	}, daconfig.ServerConfig{
		Model: "test-model", WorkingDirectory: workingDirectory, StateDirectory: stateDirectory,
		RecursionLimit: 123, MemoryReadOnly: true, NonInteractive: true,
	})
	if runtime == nil || runtime.RestartController() == nil {
		t.Fatal("runtime or restart controller missing")
	}
	process := newFakeLocalDevProcess()
	process.finishOnSignal = true
	var gotPath string
	var gotArguments []string
	var gotLaunch localDevProcessLaunch
	runtime.server.config.processFactory = func(path string, arguments []string, launch localDevProcessLaunch) (localDevProcess, error) {
		gotPath, gotArguments, gotLaunch = path, append([]string(nil), arguments...), launch
		return process, nil
	}
	runtime.server.config.probe = func(context.Context, *url.URL) (bool, error) { return true, nil }
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if gotPath != executable || !reflect.DeepEqual(gotArguments, []string{"serve", "--literal=$HOME"}) {
		t.Fatalf("launch = %q %#v", gotPath, gotArguments)
	}
	environment := strings.Join(gotLaunch.Environment, "\n")
	for _, wanted := range []string{
		"SAFE_PARENT=safe", daconfig.ServerPrefix + "MODEL=test-model",
		daconfig.ServerPrefix + "RECURSION_LIMIT=123",
		daconfig.ServerPrefix + "INTERACTIVE=false",
	} {
		if !strings.Contains(environment, wanted) {
			t.Fatalf("environment missing %q: %s", wanted, environment)
		}
	}
	if strings.Contains(environment, "must-not-cross") || strings.Contains(environment, "OPENAI_API_KEY") {
		t.Fatalf("credential crossed child boundary: %s", environment)
	}
	if err := runtime.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestLocalDevFlagsAreDocumented(t *testing.T) {
	var output strings.Builder
	printUsage(&output)
	for _, flag := range []string{"--local-dev-server", "--local-dev-arg", "--local-dev-endpoint", "--local-dev-health-path", "--local-dev-inherit-env"} {
		if !strings.Contains(output.String(), flag) {
			t.Fatalf("usage missing %s", flag)
		}
	}
}

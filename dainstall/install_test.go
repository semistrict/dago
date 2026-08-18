package dainstall

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeExecutor struct {
	path     string
	lookups  []string
	commands []Command
	err      error
}

type mapExecutor map[string]string

func (mapExecutor) LookPath(string) (string, error)    { return "", nil }
func (mapExecutor) Run(context.Context, Command) error { return nil }

func (executor *fakeExecutor) LookPath(name string) (string, error) {
	executor.lookups = append(executor.lookups, name)
	if executor.path == "" {
		return "", errors.New("missing")
	}
	return executor.path, nil
}
func (executor *fakeExecutor) Run(_ context.Context, command Command) error {
	executor.commands = append(executor.commands, command)
	return executor.err
}

func TestInstallerRequiresExplicitAuthorizationAndUsesOnlyFixedArgv(t *testing.T) {
	executor := &fakeExecutor{path: "/usr/local/bin/go"}
	installer := New(executor, []Spec{{Name: "helper", Kind: Package, Executable: "go", Arguments: []string{"install", "example.invalid/helper@v1.2.3"}}}, Options{})
	if _, err := installer.Install(t.Context(), Package, "helper", AuthorizationDenied); !errors.Is(err, ErrAuthorization) {
		t.Fatalf("denied error = %v", err)
	}
	if len(executor.commands) != 0 {
		t.Fatal("denied install launched a process")
	}
	result, err := installer.Install(t.Context(), Package, "HELPER", AuthorizationGranted)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != Installed || len(executor.commands) != 1 {
		t.Fatalf("result=%#v commands=%#v", result, executor.commands)
	}
	command := executor.commands[0]
	if command.Executable != "/usr/local/bin/go" || !reflect.DeepEqual(command.Arguments, []string{"install", "example.invalid/helper@v1.2.3"}) || command.Timeout != 5*time.Minute || command.LockWait != 5*time.Second || command.LockPath == "" {
		t.Fatalf("command = %#v", command)
	}
}

func TestInstallerRejectsUnknownAndHostileNamesBeforeAuthority(t *testing.T) {
	executor := &fakeExecutor{path: "/usr/local/bin/go"}
	installer := New(executor, []Spec{{Name: "safe", Kind: Package, Executable: "go", Arguments: []string{"install", "example.invalid/safe@v1"}}}, Options{})
	for _, name := range []string{"unknown", "safe; touch /tmp/owned", "safe\u001b]52;c;owned\a\u202e", strings.Repeat("x", 1000)} {
		_, err := installer.Install(t.Context(), Package, name, AuthorizationGranted)
		if !errors.Is(err, ErrUnknownDependency) {
			t.Errorf("Install(%q) error = %v", name, err)
		}
		if err != nil && (strings.ContainsRune(err.Error(), '\x1b') || strings.ContainsRune(err.Error(), '\u202e')) {
			t.Errorf("Install(%q) returned terminal-unsafe error %q", name, err)
		}
	}
	if len(executor.lookups) != 0 || len(executor.commands) != 0 {
		t.Fatalf("unknown input reached executor: %#v %#v", executor.lookups, executor.commands)
	}
}

func TestBuiltInExtraNeedsNoProcessAuthority(t *testing.T) {
	executor := &fakeExecutor{}
	installer := New(executor, []Spec{{Name: "daytona", Kind: Extra, Description: "Compiled-in sandbox backend", BuiltIn: true}}, Options{})
	result, err := installer.Install(t.Context(), Extra, "daytona", AuthorizationDenied)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != AlreadyAvailable || len(executor.commands) != 0 {
		t.Fatalf("result=%#v commands=%#v", result, executor.commands)
	}
	available := installer.Available(Extra)
	if len(available) != 1 || available[0].Name != "daytona" || !available[0].BuiltIn {
		t.Fatalf("available = %#v", available)
	}
}

func TestInstallerDoesNotLeakExecutorFailures(t *testing.T) {
	executor := &fakeExecutor{path: "/usr/local/bin/go", err: errors.New("registry response carried private-value")}
	installer := New(executor, []Spec{{Name: "helper", Kind: Package, Executable: "go", Arguments: []string{"install", "example.invalid/helper@v1"}}}, Options{})
	_, err := installer.Install(t.Context(), Package, "helper", AuthorizationGranted)
	if !errors.Is(err, ErrInstallFailed) || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("error = %v", err)
	}
}

func TestInstallerPropagatesCancellationAndDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	installer := New(&fakeExecutor{}, []Spec{{Name: "built-in", Kind: Extra, BuiltIn: true}}, Options{})
	if _, err := installer.Install(ctx, Extra, "built-in", AuthorizationDenied); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
	executor := &fakeExecutor{path: "/bin/tool", err: context.DeadlineExceeded}
	installer = New(executor, []Spec{{Name: "helper", Kind: Package, Executable: "tool"}}, Options{})
	if _, err := installer.Install(t.Context(), Package, "helper", AuthorizationGranted); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
}

func TestInstallerRejectsInvalidStaticCatalogs(t *testing.T) {
	tests := []struct {
		name    string
		catalog []Spec
	}{
		{"duplicate", []Spec{{Name: "same", Kind: Extra, BuiltIn: true}, {Name: "same", Kind: Extra, BuiltIn: true}}},
		{"invalid name", []Spec{{Name: "bad name", Kind: Extra, BuiltIn: true}}},
		{"invalid kind", []Spec{{Name: "name", Kind: "other", BuiltIn: true}}},
		{"built-in command", []Spec{{Name: "name", Kind: Extra, BuiltIn: true, Executable: "go"}}},
		{"relative executable path", []Spec{{Name: "name", Kind: Package, Executable: "./go"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New did not panic")
				}
			}()
			New(&fakeExecutor{}, test.catalog, Options{})
		})
	}
	var executor *fakeExecutor
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("New accepted a typed nil executor")
			}
		}()
		New(executor, nil, Options{})
	}()
	var nilMap mapExecutor
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("New accepted a map-kind typed nil executor")
			}
		}()
		New(nilMap, nil, Options{})
	}()
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a relative lock path")
		}
	}()
	New(&fakeExecutor{}, nil, Options{LockPath: "relative.lock"})
}

func TestAvailableIsSortedAndDefensivelyCopied(t *testing.T) {
	arguments := []string{"install", "example.invalid/z@v1"}
	installer := New(&fakeExecutor{path: "/bin/go"}, []Spec{{Name: "z", Kind: Package, Executable: "go", Arguments: arguments}, {Name: "a", Kind: Package, BuiltIn: true}}, Options{})
	arguments[0] = "mutated"
	available := installer.Available(Package)
	if len(available) != 2 || available[0].Name != "a" || available[1].Name != "z" {
		t.Fatalf("available = %#v", available)
	}
}

func TestInstallLockSerializesAcrossInstallers(t *testing.T) {
	path := t.TempDir() + "/install.lock"
	unlock, err := lockInstall(t.Context(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := lockInstall(ctx, path, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	secondUnlock, err := lockInstall(t.Context(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := secondUnlock(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallEnvironmentExcludesAmbientCredentials(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "private-value")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "private-value")
	t.Setenv("PATH", "/safe/bin")
	environment := installEnvironment()
	if !slices.Contains(environment, "PATH=/safe/bin") {
		t.Fatalf("environment = %#v", environment)
	}
	for _, value := range environment {
		if strings.Contains(value, "private-value") {
			t.Fatalf("credential leaked into environment: %q", value)
		}
	}
}

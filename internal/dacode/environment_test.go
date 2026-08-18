package dacode

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunLoadsProjectEnvironmentForEarlyCommandsAndRestoresIt(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, ".env"), []byte("TAVILY_API_KEY=project-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	previous, present := os.LookupEnv("TAVILY_API_KEY")
	if err := os.Unsetenv("TAVILY_API_KEY"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv("TAVILY_API_KEY", previous)
		} else {
			_ = os.Unsetenv("TAVILY_API_KEY")
		}
	})
	t.Setenv("HOME", t.TempDir())

	var output, stderr bytes.Buffer
	err := Run(context.Background(), []string{
		"auth", "status", "tavily", "--auth-file", filepath.Join(t.TempDir(), "auth.json"),
	}, strings.NewReader(""), &output, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != "tavily\tenv: TAVILY_API_KEY\n" || stderr.Len() != 0 {
		t.Fatalf("output = %q, stderr = %q", output.String(), stderr.String())
	}
	if _, exists := os.LookupEnv("TAVILY_API_KEY"); exists {
		t.Fatal("project environment leaked after command completion")
	}
}

func TestLoadCLIEnvironmentAppliesAndRestoresSafeLayers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".deepagents")
	if err := os.MkdirAll(globalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, ".env"), []byte("DACODE_GLOBAL_DOTENV_TEST=global\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, ".env"), []byte(strings.Join([]string{
		"DACODE_PROJECT_DOTENV_TEST=project",
		"DACODE_SHELL_DOTENV_TEST=project",
		"PATH=/attacker/bin",
	}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DACODE_SHELL_DOTENV_TEST", "shell")
	_ = os.Unsetenv("DACODE_PROJECT_DOTENV_TEST")
	_ = os.Unsetenv("DACODE_GLOBAL_DOTENV_TEST")
	originalPath := os.Getenv("PATH")

	var stderr bytes.Buffer
	dotenvEnvironmentMu.Lock()
	restore, err := loadCLIEnvironment(project, &stderr)
	if err != nil {
		dotenvEnvironmentMu.Unlock()
		t.Fatal(err)
	}
	if os.Getenv("DACODE_PROJECT_DOTENV_TEST") != "project" ||
		os.Getenv("DACODE_GLOBAL_DOTENV_TEST") != "global" ||
		os.Getenv("DACODE_SHELL_DOTENV_TEST") != "shell" || os.Getenv("PATH") != originalPath {
		restore()
		dotenvEnvironmentMu.Unlock()
		t.Fatalf("unexpected resolved environment")
	}
	if !strings.Contains(stderr.String(), `Ignoring dotenv key "PATH"`) || strings.Contains(stderr.String(), "attacker") {
		restore()
		dotenvEnvironmentMu.Unlock()
		t.Fatalf("stderr = %q", stderr.String())
	}
	restore()
	dotenvEnvironmentMu.Unlock()
	if _, exists := os.LookupEnv("DACODE_PROJECT_DOTENV_TEST"); exists {
		t.Fatal("project value was not restored")
	}
	if _, exists := os.LookupEnv("DACODE_GLOBAL_DOTENV_TEST"); exists {
		t.Fatal("global value was not restored")
	}
}

func TestCLIEnvironmentOverlayReloadsAtomicallyAndReportsNamesOnly(t *testing.T) {
	dotenvEnvironmentMu.Lock()
	defer dotenvEnvironmentMu.Unlock()
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	key := "DACODE_RELOAD_OVERLAY_TEST"
	previous, present := os.LookupEnv(key)
	_ = os.Unsetenv(key)
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(key, previous)
		} else {
			_ = os.Unsetenv(key)
		}
	})
	path := filepath.Join(project, ".env")
	if err := os.WriteFile(path, []byte(key+"=first-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay, err := newCLIEnvironmentOverlay(project, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer overlay.Close()
	if got := os.Getenv(key); got != "first-value" {
		t.Fatalf("initial value = %q", got)
	}
	if err := os.WriteFile(path, []byte(key+"=second-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rollback, changes, err := overlay.Reload(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(key); got != "second-value" {
		t.Fatalf("reloaded value = %q", got)
	}
	if len(changes) != 1 || changes[0] != "Environment key "+key+" changed" || strings.Contains(strings.Join(changes, "\n"), "value") {
		t.Fatalf("changes = %#v", changes)
	}
	rollback()
	if got := os.Getenv(key); got != "first-value" {
		t.Fatalf("rolled back value = %q", got)
	}
	if _, _, err := overlay.Reload(io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(key); got != "second-value" {
		t.Fatalf("committed value = %q", got)
	}
	if err := overlay.Close(); err != nil {
		t.Fatal(err)
	}
	if _, exists := os.LookupEnv(key); exists {
		t.Fatal("overlay value remained after close")
	}
}

func TestCLIEnvironmentOverlayPreservesShellPrecedence(t *testing.T) {
	dotenvEnvironmentMu.Lock()
	defer dotenvEnvironmentMu.Unlock()
	t.Setenv("HOME", t.TempDir())
	key := "DACODE_RELOAD_SHELL_TEST"
	t.Setenv(key, "shell")
	project := t.TempDir()
	path := filepath.Join(project, ".env")
	if err := os.WriteFile(path, []byte(key+"=project-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay, err := newCLIEnvironmentOverlay(project, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer overlay.Close()
	if err := os.WriteFile(path, []byte(key+"=project-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, changes, err := overlay.Reload(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(key); got != "shell" || len(changes) != 0 {
		t.Fatalf("shell precedence = %q changes=%#v", got, changes)
	}
}

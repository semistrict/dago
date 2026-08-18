package datalon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigZeroValueUsesBoundedDefaultsAndPrivateState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := t.TempDir()
	host := NewHost(nil, Config{StateRoot: root, Workspace: workspace})
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Stop(t.Context()) })
	config := host.Config()
	if config.AssistantID != "default" || config.RecursionLimit != 500 {
		t.Fatalf("defaults = %+v", config)
	}
	if config.MaxMessageBytes != 1<<20 || config.SendTimeout != 30*time.Second || config.StopTimeout != 10*time.Second {
		t.Fatalf("bounded defaults = %+v", config)
	}
	for _, relative := range []string{".", "agents", "cron", "channels", filepath.Join("media", "inbound")} {
		path := filepath.Join(root, "default", relative)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if permission := info.Mode().Perm(); permission != 0o700 {
			t.Fatalf("%s permission = %o, want 700", path, permission)
		}
	}
}

func TestNewHostPanicsOnNegativeStaticConfigBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config Config
	}{
		{name: "recursion limit", config: Config{RecursionLimit: -1}},
		{name: "message bytes", config: Config{MaxMessageBytes: -1}},
		{name: "send timeout", config: Config{SendTimeout: -1}},
		{name: "stop timeout", config: Config{StopTimeout: -1}},
		{name: "approval timeout", config: Config{ApprovalTimeout: -1}},
		{name: "approval actions", config: Config{MaxApprovalActions: -1}},
		{name: "approval prompt bytes", config: Config{MaxApprovalPromptBytes: -1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatal("NewHost did not panic")
				}
			}()
			_ = NewHost(nil, test.config)
		})
	}
}

func TestConfigFromEnvPrecedenceAndValidation(t *testing.T) {
	t.Parallel()
	config, err := ConfigFromEnv(map[string]string{
		legacyAssistantIDEnv: "legacy", assistantIDEnv: "primary",
		homeEnv: "/state", workspaceEnv: "/workspace", recursionEnv: "750",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.AssistantID != "primary" || config.StateRoot != "/state" || config.Workspace != "/workspace" || config.RecursionLimit != 750 {
		t.Fatalf("config = %+v", config)
	}
	for _, env := range []map[string]string{
		{assistantIDEnv: "../escape"},
		{assistantIDEnv: ".."},
		{recursionEnv: "0"},
		{recursionEnv: "many"},
	} {
		if _, err := ConfigFromEnv(env); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("ConfigFromEnv(%v) error = %v", env, err)
		}
	}
}

func TestStateDirFailsClosedForUnsafeAssistantID(t *testing.T) {
	t.Parallel()
	if got := (Config{AssistantID: "../escape", StateRoot: t.TempDir()}).StateDir(); got != "" {
		t.Fatalf("StateDir = %q, want empty", got)
	}
}

func TestStartRejectsInvalidWorkspaceWithoutCreatingOutsideState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	host := NewHost(nil, Config{StateRoot: root, Workspace: filepath.Join(root, "missing")})
	if err := host.Start(t.Context()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Start error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "default")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state directory should not be created after workspace validation: %v", err)
	}
}

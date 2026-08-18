package daenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadUsesShellProjectGlobalPrecedenceAndNearestDiscovery(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	start := filepath.Join(project, "nested", "work")
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("IGNORED_PARENT=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".env"), []byte("PROJECT=project\nSHARED=project\nexport QUOTED='hello world'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(global, []byte("GLOBAL=global\nSHARED=global\nPROJECT=global\nDOUBLE=\"line\\nvalue\" # comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Load(start, []string{"SHARED=shell", "EMPTY="}, Options{GlobalPath: global})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProjectPath != filepath.Join(project, ".env") || result.GlobalPath != global {
		t.Fatalf("paths = %q %q", result.ProjectPath, result.GlobalPath)
	}
	want := map[string]string{
		"SHARED": "shell", "EMPTY": "", "PROJECT": "project", "QUOTED": "hello world",
		"GLOBAL": "global", "DOUBLE": "line\nvalue",
	}
	for key, value := range want {
		if result.Values[key] != value {
			t.Fatalf("%s = %q, want %q", key, result.Values[key], value)
		}
	}
	if _, exists := result.Values["IGNORED_PARENT"]; exists {
		t.Fatalf("parent file was loaded: %#v", result.Values)
	}
	if !sortIsStable(result.Environment) {
		t.Fatalf("environment is not sorted: %v", result.Environment)
	}
}

func TestLoadRejectsUntrustedProjectControlsButAllowsTrustedGlobalControls(t *testing.T) {
	start := t.TempDir()
	project := strings.Join([]string{
		"PATH=/attacker/bin",
		"HTTPS_PROXY=https://attacker.example",
		"LANGSMITH_ENDPOINT=https://attacker.example",
		"OPENAI_BASE_URL=https://attacker.example",
		"GIT_SSH_COMMAND=attacker-command",
		"ZDOTDIR=/attacker/zsh",
		"GIT_CONFIG_KEY_0=credential.helper",
		"DEEPAGENTS_CODE_DANGEROUSLY_ENABLE_PROJECT_MCP_SERVERS=evil",
		"SAFE=value",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(start, ".env"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(global, []byte("HTTPS_PROXY=https://trusted.example\nPATH=/still-denied\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Load(start, nil, Options{GlobalPath: global})
	if err != nil {
		t.Fatal(err)
	}
	if result.Values["SAFE"] != "value" || result.Values["HTTPS_PROXY"] != "https://trusted.example" {
		t.Fatalf("values = %#v", result.Values)
	}
	if _, exists := result.Values["PATH"]; exists {
		t.Fatalf("denied PATH loaded: %#v", result.Values)
	}
	ignored := map[string]int{}
	for _, item := range result.Ignored {
		ignored[item.Key]++
		if strings.Contains(item.Reason, "attacker") {
			t.Fatalf("ignored reason leaked value: %#v", item)
		}
	}
	for _, key := range []string{
		"PATH", "HTTPS_PROXY", "LANGSMITH_ENDPOINT", "OPENAI_BASE_URL",
		"GIT_SSH_COMMAND", "ZDOTDIR", "GIT_CONFIG_KEY_0",
		"DEEPAGENTS_CODE_DANGEROUSLY_ENABLE_PROJECT_MCP_SERVERS",
	} {
		if ignored[key] == 0 {
			t.Fatalf("missing ignored key %s in %#v", key, result.Ignored)
		}
	}
}

func TestLoadFailsClosedOnSymlinksMalformedInputAndBounds(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		start := t.TempDir()
		target := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(target, []byte("SECRET=value\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(start, ".env")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := Load(start, nil, Options{GlobalPath: filepath.Join(t.TempDir(), "missing")}); err == nil {
			t.Fatal("expected symlink rejection")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		start := t.TempDir()
		if err := os.WriteFile(filepath.Join(start, ".env"), []byte("1INVALID=value\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(start, nil, Options{GlobalPath: filepath.Join(t.TempDir(), "missing")}); err == nil {
			t.Fatal("expected malformed assignment rejection")
		}
	})
	t.Run("size", func(t *testing.T) {
		start := t.TempDir()
		if err := os.WriteFile(filepath.Join(start, ".env"), []byte("VALUE=0123456789\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(start, nil, Options{GlobalPath: filepath.Join(t.TempDir(), "missing"), MaxFileBytes: 8}); err == nil {
			t.Fatal("expected size rejection")
		}
	})
}

func TestLoadRejectsInvalidStaticArguments(t *testing.T) {
	for _, invoke := range []func(){
		func() { _, _ = Load("", nil, Options{}) },
		func() { _, _ = Load(t.TempDir(), nil, Options{GlobalPath: "relative"}) },
		func() { _, _ = Load(t.TempDir(), nil, Options{MaxLines: -1}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			invoke()
		}()
	}
}

func sortIsStable(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] > values[index] {
			return false
		}
	}
	return true
}

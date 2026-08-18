package damcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestFingerprintMatchesPinnedCanonicalJSON(t *testing.T) {
	first := json.RawMessage(`{"url":"https://example.test/mcp","headers":{"X":"<tag>"}}`)
	second := json.RawMessage(" { \"headers\" : {\"X\":\"<tag>\"}, \"url\": \"https://example.test/mcp\" }")
	want := "sha256:d7497a92e2d7cefcc60b3d3c7302e83106fb7818da88f5a1340bfbcf9812b281"
	for _, definition := range []json.RawMessage{first, second} {
		got, err := Fingerprint(definition)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("Fingerprint() = %q, want %q", got, want)
		}
	}
	changed, err := Fingerprint(json.RawMessage(`{"url":"https://changed.test/mcp"}`))
	if err != nil {
		t.Fatal(err)
	}
	if changed == want {
		t.Fatal("changed server definition retained its approval fingerprint")
	}
	unicodeValue, err := Fingerprint(json.RawMessage("{\"line\":\"\u2028\"}"))
	if err != nil {
		t.Fatal(err)
	}
	if unicodeValue != "sha256:d9aafad1e14d1f5a8c2857aa2cbc882e7ec7dbd4912dc54fc7722766320e80c2" {
		t.Fatalf("Unicode fingerprint = %q", unicodeValue)
	}
}

func TestStoreLoadsPolicyAndResolvesWithRejectPrecedence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.toml")
	mustWriteFile(t, path, `[mcp]
disabled_project_servers = ["blocked", "both"]
enabled_project_servers = ["old"]
`)
	env := map[string]string{
		DangerouslyEnableProjectServersEnv: " global, both ",
		DisabledProjectServersEnv:          "env-denied,both",
		LegacyEnabledProjectServersEnv:     "ignored",
	}
	store := NewStore(path, mapLookup(env), Options{})
	policy, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(policy.Enabled, ","), "global"; got != want {
		t.Fatalf("Enabled = %q, want %q", got, want)
	}
	if got, want := strings.Join(policy.Disabled, ","), "blocked,both,env-denied"; got != want {
		t.Fatalf("Disabled = %q, want %q", got, want)
	}
	if !policy.Diagnostics.LegacyEnvIgnored || strings.Join(policy.Diagnostics.LegacyIgnored, ",") != "old" {
		t.Fatalf("Diagnostics = %#v", policy.Diagnostics)
	}

	servers := []Server{
		{Name: "global", Definition: json.RawMessage(`{"command":"global"}`)},
		{Name: "both", Definition: json.RawMessage(`{"command":"both"}`)},
		{Name: "env-denied", Definition: json.RawMessage(`{"command":"denied"}`)},
		{Name: "prompt", Definition: json.RawMessage(`{"command":"prompt"}`)},
	}
	resolution, err := policy.Resolve(root, servers, false)
	if err != nil {
		t.Fatal(err)
	}
	assertServerNames(t, resolution.Allowed, "global")
	assertServerNames(t, resolution.Disabled, "both", "env-denied")
	assertServerNames(t, resolution.Prompt, "prompt")

	trusted, err := policy.Resolve(root, servers, true)
	if err != nil {
		t.Fatal(err)
	}
	assertServerNames(t, trusted.Allowed, "global", "prompt")
	assertServerNames(t, trusted.Disabled, "both", "env-denied")
	if len(trusted.Prompt) != 0 {
		t.Fatalf("trusted Prompt = %#v", trusted.Prompt)
	}

	servers[0].Definition[0] = 'x'
	if string(resolution.Allowed[0].Definition) != `{"command":"global"}` {
		t.Fatal("resolution retained caller-owned mutable definition bytes")
	}
}

func TestRememberPersistsScopedApprovalAndPreservesConfig(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "state", "config.toml")
	mustWriteFile(t, path, `[ui]
theme = "night"

[mcp]
enabled_project_servers = ["docs", "other"]
`)
	store := NewStore(path, emptyLookup, Options{})
	servers := []Server{{Name: "docs", Definition: json.RawMessage(`{"command":"docs","args":["--safe"]}`)}}
	if err := store.Remember(context.Background(), root, servers, "docs"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("policy mode = %o, want 600", info.Mode().Perm())
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	if !strings.Contains(text, `theme = "night"`) || !strings.Contains(text, `enabled_project_servers = ["other"]`) {
		t.Fatalf("Remember discarded unrelated or unselected settings:\n%s", text)
	}

	policy, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := policy.Resolve(root, servers, false)
	if err != nil {
		t.Fatal(err)
	}
	assertServerNames(t, resolution.Allowed, "docs")

	changed := []Server{{Name: "docs", Definition: json.RawMessage(`{"command":"docs","args":["--unsafe"]}`)}}
	resolution, err = policy.Resolve(root, changed, false)
	if err != nil {
		t.Fatal(err)
	}
	assertServerNames(t, resolution.Prompt, "docs")
}

func TestRememberFixedRemoteSharesOnlyValidatedGitWorktrees(t *testing.T) {
	requireGit(t)
	parent := t.TempDir()
	mainRoot := filepath.Join(parent, "main")
	linkedRoot := filepath.Join(parent, "linked")
	runGit(t, parent, "init", mainRoot)
	runGit(t, mainRoot, "config", "user.email", "test@example.invalid")
	runGit(t, mainRoot, "config", "user.name", "Test")
	mustWriteFile(t, filepath.Join(mainRoot, "tracked.txt"), "x")
	runGit(t, mainRoot, "add", "tracked.txt")
	runGit(t, mainRoot, "commit", "-m", "initial")
	runGit(t, mainRoot, "worktree", "add", "-b", "linked", linkedRoot, "HEAD")

	path := filepath.Join(t.TempDir(), "config.toml")
	store := NewStore(path, emptyLookup, Options{})
	remote := Server{Name: "remote", Definition: json.RawMessage(`{"url":"https://example.test/mcp","type":"http"}`)}
	local := Server{Name: "local", Definition: json.RawMessage(`{"command":"node","args":["server.js"]}`)}
	if err := store.Remember(context.Background(), mainRoot, []Server{remote, local}, "remote", "local"); err != nil {
		t.Fatal(err)
	}
	policy, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := policy.Resolve(linkedRoot, []Server{remote, local}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertServerNames(t, resolution.Allowed, "remote")
	assertServerNames(t, resolution.Prompt, "local")

	interpolated := Server{Name: "interpolated", Definition: json.RawMessage(`{"url":"${MCP_URL}","type":"http"}`)}
	if err := store.Remember(context.Background(), mainRoot, []Server{interpolated}, "interpolated"); err != nil {
		t.Fatal(err)
	}
	policy, err = store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resolution, err = policy.Resolve(linkedRoot, []Server{interpolated}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertServerNames(t, resolution.Prompt, "interpolated")
}

func TestLoadFailureFailsClosedButRetainsProcessPolicy(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.toml")
	mustWriteFile(t, path, "[mcp\nSECRET_MARKER")
	env := map[string]string{
		DangerouslyEnableProjectServersEnv: "explicit",
		DisabledProjectServersEnv:          "denied",
	}
	policy, err := NewStore(path, mapLookup(env), Options{}).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !policy.LoadFailed() || policy.Diagnostics.ReadError == "" {
		t.Fatalf("policy did not surface failed load: %#v", policy)
	}
	servers := []Server{
		{Name: "explicit", Definition: json.RawMessage(`{"command":"a"}`)},
		{Name: "denied", Definition: json.RawMessage(`{"command":"b"}`)},
		{Name: "other", Definition: json.RawMessage(`{"command":"c"}`)},
	}
	resolution, err := policy.Resolve(root, servers, true)
	if err != nil {
		t.Fatal(err)
	}
	assertServerNames(t, resolution.Allowed, "explicit")
	assertServerNames(t, resolution.Disabled, "denied")
	assertServerNames(t, resolution.Prompt, "other")
	if strings.Contains(policy.Diagnostics.ReadError, "SECRET_MARKER") {
		t.Fatalf("read diagnostic leaked policy content: %q", policy.Diagnostics.ReadError)
	}
}

func TestMalformedApprovalsAreDroppedAndWrongDenyTypeFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	mustWriteFile(t, path, `[[mcp.enabled_project_server_approvals]]
project_root = "/tmp/project"
name = ""
fingerprint = "sha256:x"
`)
	policy, err := NewStore(path, emptyLookup, Options{}).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if policy.Diagnostics.MalformedApprovals != 1 || len(policy.Approvals) != 0 {
		t.Fatalf("malformed approval handling = %#v", policy)
	}

	mustWriteFile(t, path, "[mcp]\ndisabled_project_servers = 42\n")
	policy, err = NewStore(path, emptyLookup, Options{}).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !policy.LoadFailed() {
		t.Fatal("wrong-typed deny list did not fail closed")
	}
}

func TestTrustStoreRejectsSymlinkOversizeUnknownAndCancellation(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation generally requires privileges")
		}
		dir := t.TempDir()
		target := filepath.Join(dir, "target.toml")
		link := filepath.Join(dir, "config.toml")
		mustWriteFile(t, target, "[mcp]\n")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		policy, err := NewStore(link, emptyLookup, Options{}).Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !policy.LoadFailed() {
			t.Fatal("symlinked policy did not fail closed")
		}
	})

	t.Run("oversize environment", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.toml")
		policy, err := NewStore(path, mapLookup(map[string]string{
			DangerouslyEnableProjectServersEnv: strings.Repeat("x", 9),
		}), Options{MaxEnvBytes: 8}).Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !policy.Diagnostics.EnvironmentTruncated || len(policy.Enabled) != 0 {
			t.Fatalf("oversize environment was honored: %#v", policy)
		}
	})

	t.Run("oversize deny also suppresses grants", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(t.TempDir(), "config.toml")
		fingerprint, err := Fingerprint(json.RawMessage(`{"command":"local"}`))
		if err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, path, "[[mcp.enabled_project_server_approvals]]\nproject_root = \""+root+"\"\nname = \"local\"\nfingerprint = \""+fingerprint+"\"\n")
		policy, err := NewStore(path, mapLookup(map[string]string{
			DangerouslyEnableProjectServersEnv: "allowed",
			DisabledProjectServersEnv:          strings.Repeat("x", 9),
		}), Options{MaxEnvBytes: 8}).Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !policy.LoadFailed() || len(policy.Enabled) != 0 {
			t.Fatalf("unreadable deny retained a grant: %#v", policy)
		}
		resolution, err := policy.Resolve(root, []Server{{Name: "local", Definition: json.RawMessage(`{"command":"local"}`)}}, false)
		if err != nil {
			t.Fatal(err)
		}
		assertServerNames(t, resolution.Prompt, "local")
	})

	t.Run("unknown and secret-free error", func(t *testing.T) {
		store := NewStore(filepath.Join(t.TempDir(), "config.toml"), emptyLookup, Options{})
		secret := "do-not-disclose"
		err := store.Remember(context.Background(), t.TempDir(), []Server{{
			Name: "known", Definition: json.RawMessage(`{"token":"` + secret + `"}`),
		}}, "missing")
		if !errors.Is(err, ErrUnknownServer) || strings.Contains(err.Error(), secret) {
			t.Fatalf("Remember error = %v", err)
		}
	})

	t.Run("cancelled lock wait", func(t *testing.T) {
		store := NewStore(filepath.Join(t.TempDir(), "config.toml"), emptyLookup, Options{})
		<-store.lock
		defer store.release()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := store.Load(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Load error = %v, want cancellation", err)
		}
	})
}

func TestRememberSerializesConcurrentUpdates(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "config.toml"), emptyLookup, Options{})
	root := t.TempDir()
	const count = 24
	servers := make([]Server, 0, count)
	for i := range count {
		name := string(rune('a' + i))
		servers = append(servers, Server{Name: name, Definition: json.RawMessage(`{"command":"` + name + `"}`)})
	}
	var wait sync.WaitGroup
	errorsSeen := make(chan error, count)
	for _, server := range servers {
		wait.Add(1)
		go func(server Server) {
			defer wait.Done()
			errorsSeen <- store.Remember(context.Background(), root, servers, server.Name)
		}(server)
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	policy, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.Approvals) != count {
		t.Fatalf("persisted %d approvals, want %d", len(policy.Approvals), count)
	}
}

func TestNewStoreStaticValidationPanics(t *testing.T) {
	for _, test := range []struct {
		name    string
		path    string
		lookup  LookupEnv
		options Options
	}{
		{name: "empty path", lookup: emptyLookup},
		{name: "nil lookup", path: "config.toml"},
		{name: "negative limit", path: "config.toml", lookup: emptyLookup, options: Options{MaxServers: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewStore did not panic")
				}
			}()
			_ = NewStore(test.path, test.lookup, test.options)
		})
	}
}

func mapLookup(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func emptyLookup(string) (string, bool) { return "", false }

func mustWriteFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertServerNames(t *testing.T, servers []Server, want ...string) {
	t.Helper()
	got := make([]string, 0, len(servers))
	for _, server := range servers {
		got = append(got, server.Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("server names = %v, want %v", got, want)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable unavailable")
	}
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

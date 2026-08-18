package fleet_test

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/semistrict/dago/datalon/fleet"
)

type zipEntry struct {
	name   string
	body   string
	mode   os.FileMode
	method uint16
}

func TestImportMaterializesFleetAndSanitizesMCP(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	archive := filepath.Join(root, "operator-export.zip")
	credentialURL := "https://" + "operator" + ":" + "password" + "@tools.example/tenant/bearer-token/mcp?api_key=secret#oauth"
	tools := toolManifest(
		credentialURL,
		"sample",
		"read_remote",
		"write_remote",
	)
	writeZip(t, archive, []zipEntry{
		{name: "AGENTS.md", body: "root prompt"},
		{name: "config.json", body: `{"ignored":true}`},
		{name: "tools.json", body: tools},
		{name: "skills/review/SKILL.md", body: "---\nname: review\n---\nReview things."},
		{name: "subagents/researcher/AGENTS.md", body: "Research carefully."},
		{name: "subagents/researcher/tools.json", body: tools},
		{name: "unrelated.txt", body: "ignored"},
	})
	target := filepath.Join(root, "assistant")
	mustWriteFile(t, filepath.Join(target, "cron", "keep.json"), "keep")
	mustWriteFile(t, filepath.Join(target, "skills", "stale", "SKILL.md"), "stale")
	mustWriteFile(t, filepath.Join(target, "agents", "stale", "AGENTS.md"), "stale")

	result, err := fleet.Import(t.Context(), archive, target, fleet.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.RootPrompts != 1 || result.SubagentPrompts != 1 || !result.ConfigIgnored || !result.MCPConfigWritten || !result.MCPSetupWritten {
		t.Fatalf("result = %+v", result)
	}
	if strings.Join(result.InterruptTools, ",") != "write_remote" {
		t.Fatalf("interrupt tools = %v", result.InterruptTools)
	}
	assertFile(t, filepath.Join(target, "AGENTS.md"), "root prompt", 0o600)
	assertFile(t, filepath.Join(target, "skills", "review", "SKILL.md"), "---\nname: review\n---\nReview things.", 0o600)
	assertFile(t, filepath.Join(target, "agents", "researcher", "AGENTS.md"), "Research carefully.", 0o600)
	assertFile(t, filepath.Join(target, "cron", "keep.json"), "keep", 0)
	for _, path := range []string{
		filepath.Join(target, "tools.json"), filepath.Join(target, "config.json"),
		filepath.Join(target, "subagents"), filepath.Join(target, "unrelated.txt"),
		filepath.Join(target, "skills", "stale"), filepath.Join(target, "agents", "stale"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should not exist: %v", path, err)
		}
	}

	configText := readFile(t, filepath.Join(target, ".mcp.json"))
	for _, secret := range []string{"operator", "password", "api_key", "secret#oauth", "bearer-token"} {
		if strings.Contains(configText, secret) {
			t.Fatalf("sanitized config contains %q: %s", secret, configText)
		}
	}
	var config struct {
		Servers map[string]struct {
			Type         string   `json:"type"`
			URL          string   `json:"url"`
			Auth         string   `json:"auth"`
			AllowedTools []string `json:"allowedTools"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(configText), &config); err != nil {
		t.Fatal(err)
	}
	server := config.Servers["sample"]
	if server.Type != "http" || server.Auth != "oauth" || server.URL != "https://tools.example/tenant/<secret-redacted>/mcp" {
		t.Fatalf("server = %+v", server)
	}
	if strings.Join(server.AllowedTools, ",") != "read_remote,write_remote" {
		t.Fatalf("allowed tools = %v", server.AllowedTools)
	}
	setup := readFile(t, filepath.Join(target, ".mcp.json.setup"))
	for _, text := range []string{"Server: sample", "Tool count: 2", "Scopes: researcher, root", "Interrupt-enabled tools: write_remote"} {
		if !strings.Contains(setup, text) {
			t.Fatalf("setup missing %q:\n%s", text, setup)
		}
	}
	handoff := fleet.FormatHandoff(result)
	if !strings.Contains(handoff, "DEEPAGENTS_TALON_INTERRUPT_ON_TOOLS=write_remote") || !strings.Contains(handoff, "Review .mcp.json.setup") {
		t.Fatalf("handoff = %s", handoff)
	}
}

func TestImportRepeatedRunRefreshesOnlyManagedPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := filepath.Join(root, "first.zip")
	second := filepath.Join(root, "second.zip")
	target := filepath.Join(root, "assistant")
	writeZip(t, first, []zipEntry{
		{name: "AGENTS.md", body: "first"},
		{name: "tools.json", body: toolManifest("https://tools.example/mcp", "sample", "read_remote")},
		{name: "skills/first/SKILL.md", body: "first skill"},
		{name: "subagents/first/AGENTS.md", body: "first agent"},
	})
	writeZip(t, second, []zipEntry{
		{name: "AGENTS.md", body: "second"},
		{name: "skills/second/SKILL.md", body: "second skill"},
		{name: "subagents/second/AGENTS.md", body: "second agent"},
	})
	if _, err := fleet.Import(t.Context(), first, target, fleet.Options{}); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(target, "cron", "keep"), "keep")
	result, err := fleet.Import(t.Context(), second, target, fleet.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.MCPConfigWritten || result.MCPSetupWritten {
		t.Fatalf("unexpected MCP outputs: %+v", result)
	}
	assertFile(t, filepath.Join(target, "AGENTS.md"), "second", 0o600)
	assertFile(t, filepath.Join(target, "cron", "keep"), "keep", 0)
	for _, path := range []string{
		filepath.Join(target, ".mcp.json"), filepath.Join(target, ".mcp.json.setup"),
		filepath.Join(target, "skills", "first"), filepath.Join(target, "agents", "first"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale managed path %s remains: %v", path, err)
		}
	}
	assertFile(t, filepath.Join(target, "skills", "second", "SKILL.md"), "second skill", 0o600)
	assertFile(t, filepath.Join(target, "agents", "second", "AGENTS.md"), "second agent", 0o600)
}

func TestImportRejectsUnsafeArchiveBeforeChangingTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []zipEntry
		want    error
	}{
		{name: "missing root", entries: []zipEntry{{name: "skills/x/SKILL.md", body: "x"}}, want: fleet.ErrInvalidArchive},
		{name: "parent traversal", entries: []zipEntry{{name: "AGENTS.md", body: "new"}, {name: "../escape", body: "bad"}}, want: fleet.ErrUnsafeArchive},
		{name: "absolute", entries: []zipEntry{{name: "AGENTS.md", body: "new"}, {name: "/escape", body: "bad"}}, want: fleet.ErrUnsafeArchive},
		{name: "windows drive", entries: []zipEntry{{name: "AGENTS.md", body: "new"}, {name: `C:\escape`, body: "bad"}}, want: fleet.ErrUnsafeArchive},
		{name: "windows alternate stream", entries: []zipEntry{{name: "AGENTS.md", body: "new"}, {name: "skills/review:escape/SKILL.md", body: "bad"}}, want: fleet.ErrUnsafeArchive},
		{name: "windows device", entries: []zipEntry{{name: "AGENTS.md", body: "new"}, {name: "skills/NUL/SKILL.md", body: "bad"}}, want: fleet.ErrUnsafeArchive},
		{name: "empty component", entries: []zipEntry{{name: "AGENTS.md", body: "new"}, {name: "skills//SKILL.md", body: "bad"}}, want: fleet.ErrUnsafeArchive},
		{name: "symlink", entries: []zipEntry{{name: "AGENTS.md", body: "new"}, {name: "skills/x/SKILL.md", body: "../../escape", mode: os.ModeSymlink | 0o777}}, want: fleet.ErrUnsafeArchive},
		{name: "special file", entries: []zipEntry{{name: "AGENTS.md", body: "new"}, {name: "skills/x/SKILL.md", body: "bad", mode: os.ModeNamedPipe | 0o600}}, want: fleet.ErrUnsafeArchive},
		{name: "unsafe subagent", entries: []zipEntry{{name: "AGENTS.md", body: "new"}, {name: "subagents/bad name/AGENTS.md", body: "bad"}}, want: fleet.ErrUnsafeArchive},
		{name: "duplicate", entries: []zipEntry{{name: "AGENTS.md", body: "new"}, {name: "AGENTS.md", body: "other"}}, want: fleet.ErrUnsafeArchive},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			archive := filepath.Join(root, "fleet.zip")
			target := filepath.Join(root, "assistant")
			mustWriteFile(t, filepath.Join(target, "AGENTS.md"), "existing")
			writeZip(t, archive, test.entries)
			_, err := fleet.Import(t.Context(), archive, target, fleet.Options{})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			assertFile(t, filepath.Join(target, "AGENTS.md"), "existing", 0)
			if _, err := os.Lstat(filepath.Join(root, "escape")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("escape created: %v", err)
			}
		})
	}
}

func TestImportEnforcesFiniteLimits(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	compressed := filepath.Join(root, "compressed.zip")
	writeZip(t, compressed, []zipEntry{
		{name: "AGENTS.md", body: "root"},
		{name: "skills/bomb/SKILL.md", body: strings.Repeat("0", 1_000_000), method: zip.Deflate},
	})
	if _, err := fleet.Import(t.Context(), compressed, filepath.Join(root, "compressed-target"), fleet.Options{}); !errors.Is(err, fleet.ErrLimitExceeded) {
		t.Fatalf("compression error = %v", err)
	}

	archive := filepath.Join(root, "bounded.zip")
	writeZip(t, archive, []zipEntry{{name: "AGENTS.md", body: "root"}, {name: "skills/x/SKILL.md", body: "skill"}})
	tests := []fleet.Options{
		{Limits: fleet.Limits{MaxArchiveBytes: 1}},
		{Limits: fleet.Limits{MaxEntries: 1}},
		{Limits: fleet.Limits{MaxFileBytes: 3}},
		{Limits: fleet.Limits{MaxUncompressedBytes: 5}},
	}
	for index, options := range tests {
		if _, err := fleet.Import(t.Context(), archive, filepath.Join(root, "target"), options); !errors.Is(err, fleet.ErrLimitExceeded) {
			t.Fatalf("limit %d error = %v", index, err)
		}
	}
	toolsArchive := filepath.Join(root, "tools.zip")
	writeZip(t, toolsArchive, []zipEntry{
		{name: "AGENTS.md", body: "root"},
		{name: "tools.json", body: toolManifest("https://tools.example/mcp", "sample", "one", "two")},
	})
	if _, err := fleet.Import(t.Context(), toolsArchive, filepath.Join(root, "tools-target"), fleet.Options{Limits: fleet.Limits{MaxTools: 1}}); !errors.Is(err, fleet.ErrLimitExceeded) {
		t.Fatalf("tool limit error = %v", err)
	}
}

func TestImportPanicsOnNegativeSignedLimitsBeforeFilesystemAccess(t *testing.T) {
	t.Parallel()
	tests := []fleet.Limits{
		{MaxArchiveBytes: -1},
		{MaxEntries: -1},
		{MaxTools: -1},
	}
	for _, limits := range tests {
		limits := limits
		t.Run("negative", func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatal("Import did not panic")
				}
			}()
			_, _ = fleet.Import(t.Context(), "missing-archive.zip", "missing-target", fleet.Options{Limits: limits})
		})
	}
}

func TestImportRejectsMalformedOrSecretBearingToolInputWithoutLeakingIt(t *testing.T) {
	t.Parallel()
	tests := []string{
		`{bad`,
		`{"tools":{}}`,
		`{"tools":[null]}`,
		`{"tools":[{"name":"bad,env","mcp_server_url":"https://tools.example","mcp_server_name":"x"}]}`,
		`{"tools":[{"name":"read","mcp_server_url":"file:///tmp/token","mcp_server_name":"x"}]}`,
		`{"tools":[{"name":"read","mcp_server_url":"https://tools.example:99999/mcp","mcp_server_name":"x"}]}`,
		`{"tools":[{"name":"read","mcp_server_url":"https://` + "user" + ":" + "super-secret" + `@[bad","mcp_server_name":"x"}]}`,
	}
	for index, manifest := range tests {
		root := t.TempDir()
		archive := filepath.Join(root, "fleet.zip")
		writeZip(t, archive, []zipEntry{{name: "AGENTS.md", body: "root"}, {name: "tools.json", body: manifest}})
		_, err := fleet.Import(t.Context(), archive, filepath.Join(root, "target"), fleet.Options{})
		if !errors.Is(err, fleet.ErrInvalidTools) {
			t.Fatalf("case %d error = %v", index, err)
		}
		if strings.Contains(err.Error(), "super-secret") {
			t.Fatalf("case %d leaked secret in %q", index, err)
		}
	}
}

func TestImportRejectsSourceAndTargetSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	t.Parallel()
	root := t.TempDir()
	realArchive := filepath.Join(root, "real.zip")
	writeZip(t, realArchive, []zipEntry{{name: "AGENTS.md", body: "root"}})
	archiveLink := filepath.Join(root, "archive.zip")
	if err := os.Symlink(realArchive, archiveLink); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.Import(t.Context(), archiveLink, filepath.Join(root, "target"), fleet.Options{}); !errors.Is(err, fleet.ErrUnsafeArchive) {
		t.Fatalf("source symlink error = %v", err)
	}

	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	targetLink := filepath.Join(root, "target-link")
	if err := os.Symlink(outside, targetLink); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.Import(t.Context(), realArchive, targetLink, fleet.Options{}); !errors.Is(err, fleet.ErrUnsafeTarget) {
		t.Fatalf("target symlink error = %v", err)
	}

	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "prompt")
	mustWriteFile(t, outsideFile, "outside")
	if err := os.Symlink(outsideFile, filepath.Join(target, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.Import(t.Context(), realArchive, target, fleet.Options{}); !errors.Is(err, fleet.ErrUnsafeTarget) {
		t.Fatalf("managed symlink error = %v", err)
	}
	assertFile(t, outsideFile, "outside", 0)
}

func TestImportCancellationDoesNotChangeTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	archive := filepath.Join(root, "fleet.zip")
	target := filepath.Join(root, "target")
	writeZip(t, archive, []zipEntry{{name: "AGENTS.md", body: "new"}})
	mustWriteFile(t, filepath.Join(target, "AGENTS.md"), "existing")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := fleet.Import(ctx, archive, target, fleet.Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	assertFile(t, filepath.Join(target, "AGENTS.md"), "existing", 0)
}

func toolManifest(serverURL, serverName string, tools ...string) string {
	items := make([]map[string]any, 0, len(tools))
	interrupts := make(map[string]any)
	for _, tool := range tools {
		items = append(items, map[string]any{
			"name": tool, "mcp_server_url": serverURL, "mcp_server_name": serverName,
		})
		if strings.HasPrefix(tool, "write") {
			interrupts[serverURL+"::"+tool+"::"+serverName] = true
		}
	}
	data, err := json.Marshal(map[string]any{"tools": items, "interrupt_config": interrupts})
	if err != nil {
		panic(err)
	}
	return string(data)
}

func writeZip(t *testing.T, path string, entries []zipEntry) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: entry.method}
		if entry.method == 0 {
			header.Method = zip.Store
		}
		mode := entry.mode
		if mode == 0 {
			mode = 0o600
		}
		header.SetMode(mode)
		writer, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertFile(t *testing.T, path, content string, permission os.FileMode) {
	t.Helper()
	if got := readFile(t, path); got != content {
		t.Fatalf("%s = %q, want %q", path, got, content)
	}
	if permission != 0 {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != permission {
			t.Fatalf("%s permission = %o, want %o", path, info.Mode().Perm(), permission)
		}
	}
}

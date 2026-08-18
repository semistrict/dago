package main

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunImportFleetUsesPositionalArchiveAndOutput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	archivePath := filepath.Join(root, "fleet.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	entry, err := archive.Create("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("root prompt")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "assistant")
	var stdout bytes.Buffer
	if err := run(t.Context(), []string{"import-fleet", archivePath, target}, &stdout); err != nil {
		t.Fatal(err)
	}
	if text := stdout.String(); !strings.Contains(text, "Fleet import complete.") || !strings.Contains(text, target) {
		t.Fatalf("stdout = %q", text)
	}
	data, err := os.ReadFile(filepath.Join(target, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "root prompt" {
		t.Fatalf("prompt = %q", data)
	}
}

func TestRunImportFleetUsageAndCancellation(t *testing.T) {
	t.Parallel()
	for _, arguments := range [][]string{nil, {"import-fleet"}, {"import-fleet", "only.zip"}, {"unknown", "a", "b"}} {
		if err := run(t.Context(), arguments, &bytes.Buffer{}); err == nil || err.Error() != importFleetUsage {
			t.Fatalf("arguments %v error = %v", arguments, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := run(ctx, []string{"import-fleet", "missing.zip", filepath.Join(t.TempDir(), "target")}, &bytes.Buffer{})
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("cancelled import error = %v", err)
	}
}

func TestRunMCPConfigAndLoginValidation(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(configPath, []byte(`{"mcpServers":{"plain":{"url":"https://example.test/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPAGENTS_TALON_MCP_CONFIG", configPath)
	t.Setenv("MCP_CONFIG", "")
	var stdout bytes.Buffer
	if err := runWithIO(t.Context(), []string{"mcp", "config"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatal(err)
	}
	if text := stdout.String(); !strings.Contains(text, configPath) || !strings.Contains(text, "selected") {
		t.Fatalf("config output = %q", text)
	}
	if err := runWithIO(t.Context(), []string{"mcp", "login", "missing"}, strings.NewReader(""), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing server error = %v", err)
	}
	if err := runWithIO(t.Context(), []string{"mcp", "login", "plain"}, strings.NewReader(""), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not configured for OAuth") {
		t.Fatalf("plain server error = %v", err)
	}
	if err := runWithIO(t.Context(), []string{"mcp"}, strings.NewReader(""), &bytes.Buffer{}); err == nil || err.Error() != mcpUsage {
		t.Fatalf("usage error = %v", err)
	}
}

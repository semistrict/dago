package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveUsesExplicitPrecedenceAndUserDefault(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	defaultPath := filepath.Join(home, ".deepagents", ".mcp.json")
	if err := os.MkdirAll(filepath.Dir(defaultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := Resolve(map[string]string{configEnvironment: "~/explicit.json", talonConfigEnvironment: filepath.Join(home, "highest.json")}, home)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "highest.json"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	path, err = Resolve(map[string]string{}, home)
	if err != nil {
		t.Fatal(err)
	}
	if path != defaultPath {
		t.Fatalf("path = %q, want %q", path, defaultPath)
	}
}

func TestLoadConfigValidatesExternalShape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tests := []struct{ name, body, contains string }{
		{"unknown", `{"mcpServers":{},"extra":true}`, "unknown field"},
		{"oauth header", `{"mcpServers":{"x":{"url":"https://example.test/mcp","auth":"oauth","headers":{"authorization":"secret"}}}}`, "cannot combine"},
		{"stdio oauth", `{"mcpServers":{"x":{"command":"server","auth":"oauth"}}}`, "stdio"},
		{"both filters", `{"mcpServers":{"x":{"url":"https://example.test/mcp","allowedTools":["a"],"disabledTools":["b"]}}}`, "both"},
		{"multiple JSON", `{"mcpServers":{}} {}`, "trailing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(root, strings.ReplaceAll(test.name, " ", "_")+".json")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestConfigDefaultsTransportAndAcceptsUsefulEmptyConfig(t *testing.T) {
	t.Parallel()
	if err := (Config{}).Validate(); err != nil {
		t.Fatal(err)
	}
	config := Config{Servers: map[string]Server{
		"remote": {URL: "https://example.test/mcp"},
		"local":  {Command: "example-server"},
	}}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	if normalizedType(config.Servers["remote"]) != "http" || normalizedType(config.Servers["local"]) != "stdio" {
		t.Fatal("transport defaults were not useful")
	}
}

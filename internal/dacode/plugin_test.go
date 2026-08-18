package dacode

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginCLIHasBoundedLifecycleAndStableJSON(t *testing.T) {
	catalog := t.TempDir()
	plugin := filepath.Join(catalog, "plugins", "review")
	writePluginCLIFile(t, plugin, "plugin.json", `{"name":"review","version":"1"}`)
	writePluginCLIFile(t, catalog, ".agents/plugins/marketplace.json", `{"name":"market","plugins":[{"name":"review","source":"./plugins/review"}]}`)
	store := t.TempDir()
	run := func(arguments ...string) string {
		t.Helper()
		var output bytes.Buffer
		args := append([]string{"--store", store}, arguments...)
		if err := runPluginCommand(context.Background(), args, &output); err != nil {
			t.Fatalf("run %v: %v", arguments, err)
		}
		return output.String()
	}
	if got := run("marketplace", "add", catalog); !strings.Contains(got, "Added marketplace market") {
		t.Fatalf("add=%q", got)
	}
	if got := run("install", "review@market"); !strings.Contains(got, "Reload required") {
		t.Fatalf("install=%q", got)
	}
	var envelope struct {
		Version int    `json:"version"`
		Command string `json:"command"`
		Data    struct {
			Plugins []struct {
				ID      string `json:"id"`
				Enabled bool   `json:"enabled"`
			} `json:"plugins"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(run("list", "--json")), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != 1 || envelope.Command != "plugin" || len(envelope.Data.Plugins) != 1 || !envelope.Data.Plugins[0].Enabled {
		t.Fatalf("envelope=%#v", envelope)
	}
	run("disable", "review@market")
	if got := run("list"); !strings.Contains(got, "disabled") {
		t.Fatalf("list=%q", got)
	}
	run("marketplace", "remove", "market")
	if got := run("list"); got != "" {
		t.Fatalf("removed plugin remains: %q", got)
	}
}

func TestPluginCLIRejectsMissingRequiredPositionals(t *testing.T) {
	for _, args := range [][]string{{}, {"install"}, {"marketplace"}, {"enable", "bad"}} {
		if err := runPluginCommand(context.Background(), append([]string{"--store", t.TempDir()}, args...), &bytes.Buffer{}); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}

func writePluginCLIFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

package dadev

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigAcceptsLangGraphStyleGraphRecordsAndExtensions(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "dago.json")
	data := `{
		"graphs": {
			"simple": ".:NewSimple",
			"described": {"path": "./agent:NewAgent", "description": "An agent"}
		},
		"dependencies": ["."],
		"http": {"disable_store": false}
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Graphs["simple"].Path != ".:NewSimple" || config.Graphs["described"].Description != "An agent" {
		t.Fatalf("graphs = %#v", config.Graphs)
	}
}

func TestLoadEnvironmentOverlaysProcessAndParsesDotenv(t *testing.T) {
	t.Setenv("DADEV_EXISTING", "old")
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, ".env.dev"), []byte("export DADEV_EXISTING=new\nQUOTED='hello world'\n# ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(".env.dev")
	environment, watched, err := loadEnvironment(projectConfig{Env: raw}, directory)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	if strings.Count(joined, "DADEV_EXISTING=") != 1 || !strings.Contains(joined, "DADEV_EXISTING=new") || !strings.Contains(joined, "QUOTED=hello world") {
		t.Fatalf("environment overlay was not applied: %s", joined)
	}
	if len(watched) != 1 || watched[0] != filepath.Join(directory, ".env.dev") {
		t.Fatalf("watched = %#v", watched)
	}
}

func TestOverlayEnvironmentReplacesDevelopmentValues(t *testing.T) {
	result := overlayEnvironment([]string{"A=one", "DAGO_DEV_ADDRESS=old"}, map[string]string{"DAGO_DEV_ADDRESS": "new", "B": "two"})
	joined := strings.Join(result, "\n")
	if strings.Count(joined, "DAGO_DEV_ADDRESS=") != 1 || !strings.Contains(joined, "DAGO_DEV_ADDRESS=new") {
		t.Fatalf("result = %#v", result)
	}
}

func TestDevelopmentDefaultsUseStudioCompatibleLocalhost(t *testing.T) {
	options := applyDefaults(Options{})
	if options.Host != "localhost" || options.Port != 2024 || options.Workers != 10 {
		t.Fatalf("defaults = %#v", options)
	}
}

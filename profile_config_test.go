package dago

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/semistrict/dago/dagent"
	"gopkg.in/yaml.v3"
)

func TestHarnessProfileConfigJSONAndYAMLRoundTrip(t *testing.T) {
	enabled := false
	description := "Focused worker."
	config := HarnessProfileConfig{
		BaseSystemPrompt: new("base"), SystemPromptSuffix: new("suffix"),
		ToolDescriptionOverrides: map[string]string{"ls": "List files."},
		ExcludedTools:            []string{"z", "a", "a"}, ExcludedMiddleware: []string{"summary", "memory"},
		GeneralPurposeSubagent: &GeneralPurposeSubagentProfile{Enabled: &enabled, Description: &description},
	}
	want, err := config.ToMap()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want["excluded_tools"], []string{"a", "z"}) || !reflect.DeepEqual(want["excluded_middleware"], []string{"memory", "summary"}) {
		t.Fatalf("deterministic config = %#v", want)
	}
	encodedJSON, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON HarnessProfileConfig
	if err := json.Unmarshal(encodedJSON, &fromJSON); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := fromJSON.ToMap()
	if !reflect.DeepEqual(gotJSON, want) {
		t.Fatalf("JSON round trip = %#v, want %#v", gotJSON, want)
	}
	encodedYAML, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedYAML), "!!") {
		t.Fatalf("YAML contains unsafe tags: %s", encodedYAML)
	}
	var fromYAML HarnessProfileConfig
	if err := yaml.Unmarshal(encodedYAML, &fromYAML); err != nil {
		t.Fatal(err)
	}
	gotYAML, _ := fromYAML.ToMap()
	if !reflect.DeepEqual(gotYAML, want) {
		t.Fatalf("YAML round trip = %#v, want %#v", gotYAML, want)
	}
}

func TestHarnessProfileConfigPreservesExplicitEmptySubprofile(t *testing.T) {
	config, err := HarnessProfileConfigFromMap(map[string]any{"general_purpose_subagent": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if config.GeneralPurposeSubagent == nil {
		t.Fatal("explicit empty subprofile was lost")
	}
	plain, err := config.ToMap()
	if err != nil {
		t.Fatal(err)
	}
	if value, exists := plain["general_purpose_subagent"]; !exists || !reflect.DeepEqual(value, map[string]any{}) {
		t.Fatalf("plain config = %#v", plain)
	}
	profile, err := config.ToProfile()
	if err != nil || profile.GeneralPurpose == nil {
		t.Fatalf("runtime profile = %#v, %v", profile, err)
	}
	restored, err := HarnessProfileConfigFromProfile(profile)
	if err != nil || restored.GeneralPurposeSubagent == nil {
		t.Fatalf("restored config = %#v, %v", restored, err)
	}
}

func TestHarnessProfileConfigRejectsInvalidAndRuntimeOnlyValues(t *testing.T) {
	invalid := []map[string]any{
		{"unknown": true},
		{"excluded_tools": []any{1}},
		{"excluded_middleware": []any{"_private"}},
		{"excluded_middleware": []any{"package:Type"}},
		{"excluded_middleware": []any{"filesystem"}},
		{"excluded_middleware": []any{"FilesystemMiddleware"}},
		{"excluded_middleware": []any{"SubAgentMiddleware"}},
		{"general_purpose_subagent": map[string]any{"enabled": "yes"}},
	}
	for _, value := range invalid {
		if _, err := HarnessProfileConfigFromMap(value); err == nil {
			t.Fatalf("invalid config passed: %#v", value)
		}
	}
	if _, err := HarnessProfileConfigFromProfile(Profile{
		Kind: ProfileHarness, Middleware: []dagent.Middleware{{Name: "runtime"}},
	}); err == nil {
		t.Fatal("runtime middleware should not serialize")
	}
}

func TestRegisterHarnessProfileConfig(t *testing.T) {
	name := "config-registration-test"
	if err := RegisterHarnessProfileConfig(name, HarnessProfileConfig{SystemPromptSuffix: new("configured")}); err != nil {
		t.Fatal(err)
	}
	profile, exists := LookupProfile(name)
	if !exists || profile.SystemPromptSuffix == nil || *profile.SystemPromptSuffix != "configured" {
		t.Fatalf("registered profile = %#v, %v", profile, exists)
	}
}

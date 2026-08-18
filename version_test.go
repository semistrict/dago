package dago

import (
	"encoding/json"
	"runtime/debug"
	"testing"
)

func TestVersionFromBuildInfoFindsMainDependencyAndReplacement(t *testing.T) {
	tests := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{name: "main", info: &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v1.2.3"}}, want: "v1.2.3"},
		{name: "dependency", info: &debug.BuildInfo{Main: debug.Module{Path: "example.test/app"}, Deps: []*debug.Module{{Path: modulePath, Version: "v2.0.0"}}}, want: "v2.0.0"},
		{name: "versioned replacement", info: &debug.BuildInfo{Deps: []*debug.Module{{Path: modulePath, Version: "v1.0.0", Replace: &debug.Module{Path: "example.test/fork", Version: "v1.1.0"}}}}, want: "v1.1.0"},
		{name: "local replacement", info: &debug.BuildInfo{Deps: []*debug.Module{{Path: modulePath, Version: "v1.0.0", Replace: &debug.Module{Path: "../dago"}}}}, want: "v1.0.0+local"},
		{name: "development", info: &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}}, want: "development"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := versionFromBuildInfo(test.info, true); got != test.want {
				t.Fatalf("version = %q, want %q", got, test.want)
			}
		})
	}
	if got := versionFromBuildInfo(nil, false); got != "development" {
		t.Fatalf("missing build info version = %q", got)
	}
}

func TestWithVersionMetadataPreservesCallerMetadataAndRepairsVersions(t *testing.T) {
	input := map[string]json.RawMessage{
		"tenant":      json.RawMessage(`"alpha"`),
		"lc_versions": json.RawMessage(`{"other":"v2"}`),
	}
	got := withVersionMetadata(input)
	if string(got["tenant"]) != `"alpha"` {
		t.Fatalf("tenant = %s", got["tenant"])
	}
	var versions map[string]string
	if err := json.Unmarshal(got["lc_versions"], &versions); err != nil {
		t.Fatal(err)
	}
	if versions["other"] != "v2" || versions["dago"] != Version() {
		t.Fatalf("versions = %#v", versions)
	}
	got["tenant"][0] = 'x'
	if string(input["tenant"]) != `"alpha"` {
		t.Fatal("metadata result aliases caller input")
	}
}

package dacode

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateTUIUpdateOptionsRequiresCompleteExplicitProfile(t *testing.T) {
	complete := cliOptions{
		updateChannel: "stable", updateArtifact: "dacode-test",
		updateManifestBase: "https://releases.example/channels/", updatePublicKey: filepath.Join(t.TempDir(), "release.pub"),
	}
	if err := validateTUIUpdateOptions(complete); err != nil {
		t.Fatal(err)
	}
	for _, partial := range []cliOptions{
		{updateChannel: "stable"},
		{updateArtifact: "artifact"},
		{updateManifestBase: "https://releases.example/"},
		{updatePublicKey: "/release.pub"},
	} {
		if err := validateTUIUpdateOptions(partial); err == nil || !strings.Contains(err.Error(), "provided together") {
			t.Fatalf("partial profile error = %v", err)
		}
	}
	if err := validateTUIUpdateOptions(cliOptions{updateTarget: filepath.Join(t.TempDir(), "target")}); err == nil {
		t.Fatal("target without profile was accepted")
	}
}

func TestValidateTUIUpdateOptionsRequiresAbsoluteTarget(t *testing.T) {
	options := cliOptions{
		updateChannel: "stable", updateArtifact: "dacode-test",
		updateManifestBase: "https://releases.example/channels/", updatePublicKey: "/release.pub",
		updateTarget: "relative-target",
	}
	if err := validateTUIUpdateOptions(options); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("target error = %v", err)
	}
}

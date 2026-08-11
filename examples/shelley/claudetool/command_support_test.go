package claudetool

import (
	"reflect"
	"testing"
)

func TestPackageInstallCommandsUseArgumentVectors(t *testing.T) {
	update, install, err := packageInstallCommands("apt-get", "ripgrep")
	if err != nil {
		t.Fatal(err)
	}
	if update.name != "sudo" || !reflect.DeepEqual(update.args, []string{"apt-get", "update"}) {
		t.Fatalf("update = %#v", update)
	}
	if install.name != "sudo" || !reflect.DeepEqual(install.args, []string{"apt-get", "install", "-y", "ripgrep"}) {
		t.Fatalf("install = %#v", install)
	}
}

func TestPackageInstallCommandsRejectShellSyntaxAndOptions(t *testing.T) {
	for _, name := range []string{"pkg; touch /tmp/pwned", "pkg $(whoami)", "pkg name", "--option", "pkg\nother", ""} {
		if _, _, err := packageInstallCommands("brew", name); err == nil {
			t.Errorf("packageInstallCommands accepted %q", name)
		}
	}
}

func TestValidPackageNameAllowsCommonManagerSyntax(t *testing.T) {
	for _, name := range []string{"ripgrep", "libssl-dev", "owner/tap/formula", "@scope/package@1.2.3", "nixpkgs.ripgrep", "category/package", "pkg:amd64"} {
		if !validPackageName(name) {
			t.Errorf("validPackageName(%q) = false", name)
		}
	}
}

//go:build unix

package dacode

import (
	"os/exec"
	"testing"
)

func TestConfigureLocalDevCommandCreatesDedicatedProcessGroup(t *testing.T) {
	command := exec.Command("/usr/bin/true")
	configureLocalDevCommand(command)
	if command.SysProcAttr == nil || !command.SysProcAttr.Setpgid {
		t.Fatalf("sys proc attr = %#v", command.SysProcAttr)
	}
}

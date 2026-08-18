package dacode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorCommandRunsBeforeConfigParsingAndRedactsSecrets(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config.json")
	if err := os.WriteFile(config, []byte("corrupt config containing secret-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DACODE_CONFIG", config)
	t.Setenv("OPENAI_API_KEY", "private-provider-value")
	t.Setenv("OPENAI_BASE_URL", fmt.Sprintf("https://%s:%s@%s/v1?token=private-query", "user", "private-userinfo", "example.com"))
	var stdout, stderr bytes.Buffer
	if err := Run(t.Context(), []string{"doctor", "--json", "--state-dir", root, "--cwd", root}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("Run: %v; stderr=%q", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	for _, secret := range []string{"private-provider-value", "private-userinfo", "token=private-query", "secret-value"} {
		if strings.Contains(stdout.String(), secret) {
			t.Fatalf("doctor output leaked %q: %s", secret, stdout.String())
		}
	}
	var envelope struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Healthy  bool `json:"healthy"`
			Sections []struct {
				Title string `json:"title"`
				OK    bool   `json:"ok"`
			} `json:"sections"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 || envelope.Command != "doctor" || !envelope.Data.Healthy || len(envelope.Data.Sections) != 4 {
		t.Fatalf("envelope = %#v", envelope)
	}
}

func TestDoctorCommandHelpAndArgumentErrors(t *testing.T) {
	var output bytes.Buffer
	if err := Run(t.Context(), []string{"doctor", "--help"}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "dacode doctor") || !strings.Contains(output.String(), "--json") {
		t.Fatalf("help = %q", output.String())
	}
	for _, arguments := range [][]string{{"doctor", "extra"}, {"doctor", "--config="}, {"doctor", "--state-dir"}} {
		err := Run(t.Context(), arguments, strings.NewReader(""), &output, &output)
		if err == nil || ExitCode(err) != 2 {
			t.Errorf("Run(%#v) error=%v code=%d", arguments, err, ExitCode(err))
		}
	}
}

func TestUsageIncludesDoctor(t *testing.T) {
	var output bytes.Buffer
	printUsage(&output)
	if !strings.Contains(output.String(), "dacode doctor [OPTIONS]") {
		t.Fatalf("usage missing doctor:\n%s", output.String())
	}
}

func TestDoctorUnhealthyStatusIsSilent(t *testing.T) {
	err := &silentCommandExitError{commandExitError{code: 1, err: os.ErrPermission}}
	if ExitCode(err) != 1 || !SilentExit(err) {
		t.Fatalf("code=%d silent=%t", ExitCode(err), SilentExit(err))
	}
}

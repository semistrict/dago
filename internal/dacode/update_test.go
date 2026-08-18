package dacode

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/semistrict/dago/daupdate"
)

type fakeUpdateService struct {
	result                   daupdate.Result
	err                      error
	checks, dryRuns, applies int
	current, target          string
	authorization            daupdate.Authorization
}

func (service *fakeUpdateService) Check(_ context.Context, current string) (daupdate.Result, error) {
	service.checks++
	service.current = current
	return service.result, service.err
}

func (service *fakeUpdateService) DryRun(_ context.Context, current string) (daupdate.Result, error) {
	service.dryRuns++
	service.current = current
	return service.result, service.err
}

func (service *fakeUpdateService) Apply(_ context.Context, current, target string, authorization daupdate.Authorization) (daupdate.Result, error) {
	service.applies++
	service.current, service.target, service.authorization = current, target, authorization
	return service.result, service.err
}

func fakeUpdateFactory(service updateService, captured *updateCommandOptions) updateServiceFactory {
	return func(options updateCommandOptions) (updateService, error) {
		*captured = options
		return service, nil
	}
}

func updateArguments(mode ...string) []string {
	arguments := []string{"stable", "dacode-darwin-arm64", "--manifest-base", "https://releases.example/channels/", "--public-key", "/trusted/release.pub", "--current", "v1.0.0"}
	return append(arguments, mode...)
}

func TestUpdateCommandDefaultsToSignedCheckOnly(t *testing.T) {
	service := &fakeUpdateService{result: daupdate.Result{Status: daupdate.UpdateAvailable, CurrentVersion: "v1.0.0", LatestVersion: "v1.1.0"}}
	var captured updateCommandOptions
	var output bytes.Buffer
	if err := executeUpdateCommand(t.Context(), updateArguments(), &output, ioDiscard{}, fakeUpdateFactory(service, &captured)); err != nil {
		t.Fatal(err)
	}
	if service.checks != 1 || service.dryRuns != 0 || service.applies != 0 || captured.channel != "stable" || captured.artifact != "dacode-darwin-arm64" {
		t.Fatalf("service=%#v options=%#v", service, captured)
	}
	if !strings.Contains(output.String(), "update available") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestUpdateDryRunAndApplyAreSeparateExplicitModes(t *testing.T) {
	result := daupdate.Result{Channel: "stable", Artifact: "dacode-darwin-arm64", CurrentVersion: "v1.0.0", LatestVersion: "v1.1.0", Status: daupdate.UpdateAvailable, Verified: true}
	service := &fakeUpdateService{result: result}
	var captured updateCommandOptions
	if err := executeUpdateCommand(t.Context(), updateArguments("--dry-run"), ioDiscard{}, ioDiscard{}, fakeUpdateFactory(service, &captured)); err != nil {
		t.Fatal(err)
	}
	if service.dryRuns != 1 || service.applies != 0 {
		t.Fatalf("service = %#v", service)
	}
	target := filepath.Join(t.TempDir(), "dacode")
	service.result.Applied = true
	if err := executeUpdateCommand(t.Context(), updateArguments("--apply", "--target", target), ioDiscard{}, ioDiscard{}, fakeUpdateFactory(service, &captured)); err != nil {
		t.Fatal(err)
	}
	if service.applies != 1 || service.target != target || service.authorization != daupdate.AuthorizationGranted {
		t.Fatalf("service = %#v", service)
	}
}

func TestUpdateJSONEnvelopeIsStableAndClean(t *testing.T) {
	service := &fakeUpdateService{result: daupdate.Result{Channel: "stable", Artifact: "dacode", Status: daupdate.UpToDate, CurrentVersion: "v1.0.0", LatestVersion: "v1.0.0"}}
	var captured updateCommandOptions
	var output bytes.Buffer
	if err := executeUpdateCommand(t.Context(), updateArguments("--json"), &output, ioDiscard{}, fakeUpdateFactory(service, &captured)); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		SchemaVersion int             `json:"schema_version"`
		Command       string          `json:"command"`
		Mode          updateMode      `json:"mode"`
		Data          daupdate.Result `json:"data"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 || envelope.Command != "update" || envelope.Mode != updateCheck || envelope.Data.Status != daupdate.UpToDate {
		t.Fatalf("envelope = %#v", envelope)
	}
}

func TestUpdateArgumentErrorsMakeNoService(t *testing.T) {
	tests := [][]string{
		{},
		{"stable"},
		{"stable", "artifact"},
		updateArguments("--check", "--apply"),
		updateArguments("--dry-run", "--target", "/tmp/target"),
		updateArguments("--unknown"),
	}
	for _, arguments := range tests {
		called := false
		err := executeUpdateCommand(t.Context(), arguments, ioDiscard{}, ioDiscard{}, func(updateCommandOptions) (updateService, error) {
			called = true
			return &fakeUpdateService{}, nil
		})
		if err == nil || ExitCode(err) != 2 || called {
			t.Errorf("arguments=%#v error=%v code=%d called=%v", arguments, err, ExitCode(err), called)
		}
	}
}

func TestUpdateHelpNeedsNoTrustOrNetworkInputs(t *testing.T) {
	var output bytes.Buffer
	called := false
	if err := executeUpdateCommand(t.Context(), []string{"--help"}, &output, ioDiscard{}, func(updateCommandOptions) (updateService, error) {
		called = true
		return nil, errors.New("unexpected")
	}); err != nil {
		t.Fatal(err)
	}
	if called || !strings.Contains(output.String(), "signed Ed25519 manifest") || !strings.Contains(output.String(), "--dry-run") {
		t.Fatalf("called=%v output=%q", called, output.String())
	}
}

func TestDacodeUpdateHelpDispatchesBeforeAuthentication(t *testing.T) {
	var output bytes.Buffer
	if err := Run(t.Context(), []string{"update", "--help"}, strings.NewReader(""), &output, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Usage: dacode update") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestUpdatePublicKeyReaderRejectsLinksAndMalformedKeys(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "release.pub")
	key := bytes.Repeat([]byte{0x42}, ed25519.PublicKeySize)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := readUpdatePublicKey(path)
	if err != nil || !bytes.Equal(loaded, key) {
		t.Fatalf("loaded=%x err=%v", loaded, err)
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(directory, "linked.pub")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		if _, err := readUpdatePublicKey(link); err == nil {
			t.Fatal("symlinked public key accepted")
		}
	}
	if err := os.WriteFile(path, []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readUpdatePublicKey(path); err == nil {
		t.Fatal("malformed public key accepted")
	}
	if runtime.GOOS != "windows" {
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o666); err != nil {
			t.Fatal(err)
		}
		if _, err := readUpdatePublicKey(path); err == nil {
			t.Fatal("writable public key accepted")
		}
	}
}

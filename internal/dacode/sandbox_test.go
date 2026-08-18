package dacode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dasandbox"
)

type cliSandbox struct {
	dabackend.Backend
	id string
}

func (sandbox cliSandbox) ID() string { return sandbox.id }
func (sandbox cliSandbox) Execute(context.Context, string, time.Duration) (dabackend.ExecuteResult, error) {
	exit := 0
	return dabackend.ExecuteResult{ExitCode: &exit}, nil
}
func (sandbox cliSandbox) Upload(_ context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	results := make([]dabackend.UploadResult, len(uploads))
	for index, upload := range uploads {
		results[index].Path = upload.Path
	}
	return results
}
func (sandbox cliSandbox) Delete(_ context.Context, path string) (dabackend.DeleteResult, error) {
	return dabackend.DeleteResult{Path: path}, nil
}

func TestParseCLISandboxSelectionIsExplicit(t *testing.T) {
	options, err := parseCLI([]string{"--sandbox", "--sandbox-id", "existing"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.sandbox != configuredSandboxSentinel || options.sandboxID != "existing" {
		t.Fatalf("options = %#v", options)
	}
	if _, err := parseCLI([]string{"--sandbox-id", "existing"}, io.Discard); err == nil {
		t.Fatal("sandbox ID without explicit sandbox selection was accepted")
	}
	if _, err := parseCLI([]string{"--sandbox", "runloop", "--sandbox-id", "id", "--sandbox-snapshot-name", "snap"}, io.Discard); err == nil {
		t.Fatal("mutually exclusive attach and snapshot were accepted")
	}
}

func TestOpenSandboxSessionUsesConfiguredDefaultAndConfinesSetup(t *testing.T) {
	provider := &recordingCLIProvider{sandbox: cliSandbox{id: "remote"}}
	registry := dasandbox.NewRegistry(map[string]dasandbox.Definition{
		"runloop": {Factory: func(context.Context, dasandbox.ProviderConfig) (dasandbox.Provider, error) { return provider, nil }},
	}, dasandbox.RegistryOptions{})
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "setup.sh"), []byte("true"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := openSandboxSession(context.Background(), registry, workspace, cliOptions{
		sandbox: configuredSandboxSentinel, sandboxDefault: "runloop", sandboxSetup: "setup.sh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.WorkingDir() != "/home/user" || !session.Owned() {
		t.Fatalf("session = working dir %q, owned %v", session.WorkingDir(), session.Owned())
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.sh")
	if err := os.WriteFile(outside, []byte("true"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSandboxSession(context.Background(), registry, workspace, cliOptions{
		sandbox: "runloop", sandboxSetup: outside,
	}); err == nil {
		t.Fatal("out-of-workspace setup file was accepted")
	}
}

func TestOpenSandboxSessionRequiresConfiguredBareDefault(t *testing.T) {
	registry := dasandbox.NewRegistry(nil, dasandbox.RegistryOptions{})
	_, err := openSandboxSession(context.Background(), registry, t.TempDir(), cliOptions{sandbox: configuredSandboxSentinel})
	if err == nil || errors.Is(err, dasandbox.ErrProviderUnavailable) {
		t.Fatalf("error = %v, want missing explicit default", err)
	}
}

type recordingCLIProvider struct {
	sandbox dabackend.Sandbox
	deleted []string
}

func (provider *recordingCLIProvider) GetOrCreate(context.Context, dasandbox.ProviderRequest) (dabackend.Sandbox, error) {
	return provider.sandbox, nil
}

func (provider *recordingCLIProvider) Delete(_ context.Context, id string) error {
	provider.deleted = append(provider.deleted, id)
	return nil
}

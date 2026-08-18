package daupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeSource struct {
	manifest      []byte
	artifact      []byte
	manifestErr   error
	artifactErr   error
	manifestCalls int
	artifactCalls int
	channel       string
	url           string
	artifactHook  func()
}

func (source *fakeSource) FetchManifest(_ context.Context, channel string, _ int64) (io.ReadCloser, error) {
	source.manifestCalls++
	source.channel = channel
	if source.manifestErr != nil {
		return nil, source.manifestErr
	}
	return io.NopCloser(bytes.NewReader(source.manifest)), nil
}

func (source *fakeSource) FetchArtifact(_ context.Context, value string, _ int64) (io.ReadCloser, error) {
	source.artifactCalls++
	source.url = value
	if source.artifactHook != nil {
		source.artifactHook()
	}
	if source.artifactErr != nil {
		return nil, source.artifactErr
	}
	return io.NopCloser(bytes.NewReader(source.artifact)), nil
}

func testTrustRoot(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	seed := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	private := ed25519.NewKeyFromSeed(seed)
	return private.Public().(ed25519.PublicKey), private
}

func signedManifest(t *testing.T, private ed25519.PrivateKey, channel, version, name string, artifact []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(artifact)
	payload, err := json.Marshal(manifestPayload{
		SchemaVersion: 1,
		Channel:       channel,
		Version:       version,
		PublishedAt:   time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
		Artifacts: []manifestArtifact{{
			Name: name, URL: "https://releases.example/dacode", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(artifact)),
			GoPackage: "github.com/semistrict/dago/cmd/dacode", GoModule: "github.com/semistrict/dago",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(signedEnvelope{
		SchemaVersion: 1,
		Payload:       base64.StdEncoding.EncodeToString(payload),
		Signature:     base64.StdEncoding.EncodeToString(ed25519.Sign(private, payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func testUpdater(t *testing.T, source *fakeSource) *Updater {
	t.Helper()
	public, _ := testTrustRoot(t)
	updater := New("stable", "dacode-darwin-arm64", public, source, Options{LockPath: filepath.Join(t.TempDir(), "update.lock")})
	updater.validateExecutable = func(string, manifestPayload, manifestArtifact) error { return nil }
	updater.validateCurrent = func(string, string, manifestArtifact) error { return nil }
	return updater
}

func TestCheckVerifiesSignedManifestAndComparesVersions(t *testing.T) {
	public, private := testTrustRoot(t)
	artifact := []byte("new executable")
	source := &fakeSource{artifact: artifact, manifest: signedManifest(t, private, "stable", "v1.3.0", "dacode-darwin-arm64", artifact)}
	updater := New("stable", "dacode-darwin-arm64", public, source, Options{})
	result, err := updater.Check(t.Context(), "v1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != UpdateAvailable || result.LatestVersion != "v1.3.0" || result.Verified || source.artifactCalls != 0 || source.channel != "stable" {
		t.Fatalf("result=%#v source=%#v", result, source)
	}
	for current, want := range map[string]Status{"v1.3.0": UpToDate, "v2.0.0": CurrentNewer, "v1.3.0-rc.1": UpdateAvailable} {
		result, err = updater.Check(t.Context(), current)
		if err != nil || result.Status != want {
			t.Errorf("Check(%q) result=%#v err=%v", current, result, err)
		}
	}
}

func TestCheckRejectsUnsignedTamperedAndWrongChannelMetadata(t *testing.T) {
	public, private := testTrustRoot(t)
	artifact := []byte("artifact")
	valid := signedManifest(t, private, "stable", "v1.1.0", "dacode-darwin-arm64", artifact)
	for _, test := range []struct {
		name     string
		manifest []byte
		want     error
	}{
		{"malformed", []byte("not-json"), ErrInvalidManifest},
		{"wrong channel", signedManifest(t, private, "preview", "v1.1.0", "dacode-darwin-arm64", artifact), ErrInvalidManifest},
		{"tampered", bytes.Replace(valid, []byte(`"signature":"`), []byte(`"signature":"AAAA`), 1), ErrUntrustedManifest},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &fakeSource{manifest: test.manifest}
			updater := New("stable", "dacode-darwin-arm64", public, source, Options{})
			if _, err := updater.Check(t.Context(), "v1.0.0"); !errors.Is(err, test.want) {
				t.Fatalf("error = %v", err)
			}
			if source.artifactCalls != 0 {
				t.Fatal("untrusted metadata fetched an artifact")
			}
		})
	}
}

func TestDryRunVerifiesArtifactWithoutFilesystemMutation(t *testing.T) {
	_, private := testTrustRoot(t)
	artifact := []byte("verified executable")
	source := &fakeSource{artifact: artifact, manifest: signedManifest(t, private, "stable", "v1.1.0", "dacode-darwin-arm64", artifact)}
	result, err := testUpdater(t, source).DryRun(t.Context(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || result.Applied || source.artifactCalls != 1 || result.SHA256 == "" {
		t.Fatalf("result=%#v calls=%d", result, source.artifactCalls)
	}
	source.artifact = append(source.artifact, 'x')
	if _, err := testUpdater(t, source).DryRun(t.Context(), "v1.0.0"); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestApplyRequiresAuthorityBeforeIOAndAtomicallyReplacesExecutable(t *testing.T) {
	_, private := testTrustRoot(t)
	artifact := []byte("new executable")
	source := &fakeSource{artifact: artifact, manifest: signedManifest(t, private, "stable", "v1.1.0", "dacode-darwin-arm64", artifact)}
	updater := testUpdater(t, source)
	target := filepath.Join(t.TempDir(), "dacode")
	if err := os.WriteFile(target, []byte("old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := updater.Apply(t.Context(), "v1.0.0", target, AuthorizationDenied); !errors.Is(err, ErrAuthorization) {
		t.Fatalf("denied error = %v", err)
	}
	if source.manifestCalls != 0 || source.artifactCalls != 0 {
		t.Fatal("denied apply performed I/O")
	}
	result, err := updater.Apply(t.Context(), "v1.0.0", target, AuthorizationGranted)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(artifact) || !result.Applied || !result.Verified || info.Mode().Perm() != 0o755 {
		t.Fatalf("content=%q result=%#v mode=%#o", content, result, info.Mode().Perm())
	}
}

func TestApplyRejectsLinksAndNonExecutableTargets(t *testing.T) {
	_, private := testTrustRoot(t)
	artifact := []byte("new")
	source := &fakeSource{artifact: artifact, manifest: signedManifest(t, private, "stable", "v1.1.0", "dacode-darwin-arm64", artifact)}
	updater := testUpdater(t, source)
	nonExecutable := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(nonExecutable, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := updater.Apply(t.Context(), "v1.0.0", nonExecutable, AuthorizationGranted); !errors.Is(err, ErrApplyFailed) {
		t.Fatalf("non-executable error = %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	real := filepath.Join(t.TempDir(), "real")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.WriteFile(real, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := updater.Apply(t.Context(), "v1.0.0", link, AuthorizationGranted); !errors.Is(err, ErrApplyFailed) {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestApplyRejectsTargetReplacementDuringDownload(t *testing.T) {
	_, private := testTrustRoot(t)
	artifact := []byte("new")
	target := filepath.Join(t.TempDir(), "dacode")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := &fakeSource{artifact: artifact, manifest: signedManifest(t, private, "stable", "v1.1.0", "dacode-darwin-arm64", artifact)}
	source.artifactHook = func() {
		replacement := target + ".replacement"
		if err := os.WriteFile(replacement, []byte("external replacement"), 0o755); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(replacement, target); err != nil {
			t.Error(err)
		}
	}
	updater := testUpdater(t, source)
	if _, err := updater.Apply(t.Context(), "v1.0.0", target, AuthorizationGranted); !errors.Is(err, ErrApplyFailed) {
		t.Fatalf("error = %v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "external replacement" {
		t.Fatalf("target=%q err=%v", content, err)
	}
}

func TestApplyLockWaitAndCancellationAreFinite(t *testing.T) {
	_, private := testTrustRoot(t)
	artifact := []byte("new")
	lockPath := filepath.Join(t.TempDir(), "update.lock")
	unlock, err := lockUpdate(t.Context(), lockPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	public, _ := testTrustRoot(t)
	source := &fakeSource{artifact: artifact, manifest: signedManifest(t, private, "stable", "v1.1.0", "dacode-darwin-arm64", artifact)}
	updater := New("stable", "dacode-darwin-arm64", public, source, Options{LockPath: lockPath, LockWait: 25 * time.Millisecond})
	updater.validateExecutable = func(string, manifestPayload, manifestArtifact) error { return nil }
	updater.validateCurrent = func(string, string, manifestArtifact) error { return nil }
	target := filepath.Join(t.TempDir(), "dacode")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := updater.Apply(t.Context(), "v1.0.0", target, AuthorizationGranted); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := updater.Check(ctx, "v1.0.0"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestApplyRejectsArtifactWithoutMatchingGoBuildProvenance(t *testing.T) {
	public, private := testTrustRoot(t)
	artifact := []byte("signed and checksummed but not the declared Go executable")
	source := &fakeSource{artifact: artifact, manifest: signedManifest(t, private, "stable", "v1.1.0", "dacode-darwin-arm64", artifact)}
	updater := New("stable", "dacode-darwin-arm64", public, source, Options{LockPath: filepath.Join(t.TempDir(), "update.lock")})
	updater.validateCurrent = func(string, string, manifestArtifact) error { return nil }
	target := filepath.Join(t.TempDir(), "dacode")
	if err := os.WriteFile(target, []byte("old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := updater.Apply(t.Context(), "v1.0.0", target, AuthorizationGranted); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("error = %v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "old executable" {
		t.Fatalf("target changed: content=%q err=%v", content, err)
	}
}

func TestSourceErrorsAreBoundedAndDoNotLeak(t *testing.T) {
	source := &fakeSource{manifestErr: errors.New("source-detail-must-not-escape")}
	_, err := testUpdater(t, source).Check(t.Context(), "v1.0.0")
	if !errors.Is(err, ErrUpdateCheckFailed) || strings.Contains(err.Error(), "source-detail") {
		t.Fatalf("error = %v", err)
	}
}

func TestSourceCancellationAndDeadlineArePreserved(t *testing.T) {
	for _, sourceErr := range []error{context.Canceled, context.DeadlineExceeded} {
		source := &fakeSource{manifestErr: sourceErr}
		_, err := testUpdater(t, source).Check(t.Context(), "v1.0.0")
		if !errors.Is(err, sourceErr) {
			t.Errorf("source error %v became %v", sourceErr, err)
		}
	}
}

func TestNewRejectsInvalidStaticConfiguration(t *testing.T) {
	public, _ := testTrustRoot(t)
	var typedNil *fakeSource
	tests := []func(){
		func() { New("bad channel", "artifact", public, &fakeSource{}, Options{}) },
		func() { New("stable", "../artifact", public, &fakeSource{}, Options{}) },
		func() { New("stable", "artifact", public[:3], &fakeSource{}, Options{}) },
		func() { New("stable", "artifact", public, nil, Options{}) },
		func() { New("stable", "artifact", public, typedNil, Options{}) },
		func() { New("stable", "artifact", public, &fakeSource{}, Options{LockPath: "relative"}) },
	}
	for index, test := range tests {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("case %d did not panic", index)
				}
			}()
			test()
		}()
	}
}

func TestSemverComparisonIsStrict(t *testing.T) {
	for _, invalid := range []string{"1.2.3", "v1.2", "v01.2.3", "v1.2.3-01", "v1.2.3+", "v1.2.3+bad!"} {
		if _, ok := parseVersion(invalid); ok {
			t.Errorf("parseVersion(%q) succeeded", invalid)
		}
	}
	for _, pair := range [][2]string{{"v1.0.0-alpha", "v1.0.0"}, {"v1.0.0-alpha.2", "v1.0.0-alpha.10"}, {"v1.0.0+one", "v1.0.0+two"}} {
		comparison, ok := compareVersions(pair[0], pair[1])
		if !ok || comparison >= 0 && pair[0] != "v1.0.0+one" || pair[0] == "v1.0.0+one" && comparison != 0 {
			t.Errorf("compareVersions(%q,%q) = %d,%v", pair[0], pair[1], comparison, ok)
		}
	}
}

func TestGoExecutableValidationReadsExactBuildProvenance(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := buildinfo.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	manifest := manifestPayload{Version: info.Main.Version}
	artifact := manifestArtifact{GoPackage: info.Path, GoModule: info.Main.Path}
	if err := validateGoExecutable(executable, manifest, artifact); err != nil {
		t.Fatal(err)
	}
	if err := validateCurrentGoExecutable(executable, info.Main.Version, artifact); err != nil {
		t.Fatal(err)
	}
	artifact.GoPackage = "example.invalid/wrong"
	if err := validateGoExecutable(executable, manifest, artifact); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("wrong package error = %v", err)
	}
}

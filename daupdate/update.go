// Package daupdate verifies and atomically activates signed release artifacts.
// It performs no network or filesystem work during construction and never
// derives process or shell authority.
package daupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultManifestBytes = int64(256 << 10)
	defaultArtifactBytes = int64(256 << 20)
	defaultTimeout       = 2 * time.Minute
	defaultLockWait      = 5 * time.Second
	maxManifestBytes     = int64(2 << 20)
	maxArtifactBytes     = int64(1 << 30)
	maxTimeout           = 30 * time.Minute
	maxLockWait          = time.Minute
)

var (
	ErrInvalidManifest   = errors.New("invalid update manifest")
	ErrUntrustedManifest = errors.New("update manifest signature is not trusted")
	ErrUpdateCheckFailed = errors.New("update check failed")
	ErrArtifactMismatch  = errors.New("update artifact does not match the signed manifest")
	ErrAuthorization     = errors.New("update activation requires explicit authorization")
	ErrApplyFailed       = errors.New("update activation failed")
	ErrInvalidVersion    = errors.New("invalid release version")
)

var (
	channelPattern  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,31}[a-z0-9])?$`)
	artifactPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,127}[a-z0-9])?$`)
)

// Authorization is the explicit capability required to replace an executable.
type Authorization string

const (
	AuthorizationDenied  Authorization = ""
	AuthorizationGranted Authorization = "update-approved"
)

// Status compares the current binary with the signed channel release.
type Status string

const (
	UpdateAvailable Status = "update_available"
	UpToDate        Status = "up_to_date"
	CurrentNewer    Status = "current_newer"
)

// Result is the authority-free update outcome. It deliberately omits release URLs.
type Result struct {
	Channel        string `json:"channel"`
	Artifact       string `json:"artifact"`
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	Status         Status `json:"status"`
	Verified       bool   `json:"verified"`
	Applied        bool   `json:"applied"`
	SHA256         string `json:"sha256,omitempty"`
}

// Source supplies bounded readers. Implementations must honor context and
// should enforce maxBytes before allocating; Updater independently caps reads.
type Source interface {
	FetchManifest(context.Context, string, int64) (io.ReadCloser, error)
	FetchArtifact(context.Context, string, int64) (io.ReadCloser, error)
}

// Options configures finite work. Zero values select conservative defaults.
type Options struct {
	MaxManifestBytes int64
	MaxArtifactBytes int64
	Timeout          time.Duration
	LockWait         time.Duration
	LockPath         string
}

// Updater binds one release channel and platform artifact to a trust root.
type Updater struct {
	channel            string
	artifact           string
	key                ed25519.PublicKey
	source             Source
	options            Options
	validateExecutable func(string, manifestPayload, manifestArtifact) error
	validateCurrent    func(string, string, manifestArtifact) error
}

// New constructs an Updater. Channel, artifact, trust root, and source are
// mandatory positional inputs. Static invalid configuration panics.
func New(channel, artifact string, publicKey ed25519.PublicKey, source Source, options Options) *Updater {
	channel = strings.ToLower(strings.TrimSpace(channel))
	artifact = strings.ToLower(strings.TrimSpace(artifact))
	if !channelPattern.MatchString(channel) {
		panic("update channel is invalid")
	}
	if !artifactPattern.MatchString(artifact) {
		panic("update artifact is invalid")
	}
	if len(publicKey) != ed25519.PublicKeySize {
		panic("update Ed25519 public key is required")
	}
	if nilSource(source) {
		panic("update source is required")
	}
	options = normalizeOptions(options)
	return &Updater{
		channel: channel, artifact: artifact, key: slices.Clone(publicKey), source: source, options: options,
		validateExecutable: validateGoExecutable,
		validateCurrent:    validateCurrentGoExecutable,
	}
}

func nilSource(source Source) bool {
	if source == nil {
		return true
	}
	value := reflect.ValueOf(source)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func normalizeOptions(options Options) Options {
	if options.MaxManifestBytes < 0 || options.MaxManifestBytes > maxManifestBytes ||
		options.MaxArtifactBytes < 0 || options.MaxArtifactBytes > maxArtifactBytes ||
		options.Timeout < 0 || options.Timeout > maxTimeout ||
		options.LockWait < 0 || options.LockWait > maxLockWait {
		panic("update options exceed finite bounds")
	}
	if options.MaxManifestBytes == 0 {
		options.MaxManifestBytes = defaultManifestBytes
	}
	if options.MaxArtifactBytes == 0 {
		options.MaxArtifactBytes = defaultArtifactBytes
	}
	if options.Timeout == 0 {
		options.Timeout = defaultTimeout
	}
	if options.LockWait == 0 {
		options.LockWait = defaultLockWait
	}
	if options.LockPath == "" {
		options.LockPath = defaultUpdateLockPath()
	}
	if !filepath.IsAbs(options.LockPath) {
		panic("update lock path must be absolute")
	}
	return options
}

type signedEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	Payload       string `json:"payload"`
	Signature     string `json:"signature"`
}

type manifestPayload struct {
	SchemaVersion int                `json:"schema_version"`
	Channel       string             `json:"channel"`
	Version       string             `json:"version"`
	PublishedAt   time.Time          `json:"published_at"`
	Artifacts     []manifestArtifact `json:"artifacts"`
}

type manifestArtifact struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	GoPackage string `json:"go_package"`
	GoModule  string `json:"go_module"`
}

// Check verifies signed release metadata without downloading or changing the artifact.
func (updater *Updater) Check(ctx context.Context, currentVersion string) (Result, error) {
	_, _, result, err := updater.resolve(ctx, currentVersion)
	return result, err
}

// DryRun verifies both signed metadata and artifact bytes without filesystem mutation.
func (updater *Updater) DryRun(ctx context.Context, currentVersion string) (Result, error) {
	_, artifact, result, err := updater.resolve(ctx, currentVersion)
	if err != nil || result.Status != UpdateAvailable {
		return result, err
	}
	if err := updater.verifyArtifact(ctx, artifact, io.Discard); err != nil {
		return Result{}, err
	}
	result.Verified = true
	result.SHA256 = artifact.SHA256
	return result, nil
}

// Apply verifies and atomically replaces target. A current or newer target is a
// successful no-op. Authorization is checked before any network or filesystem I/O.
func (updater *Updater) Apply(ctx context.Context, currentVersion, target string, authorization Authorization) (Result, error) {
	if authorization != AuthorizationGranted {
		return Result{}, ErrAuthorization
	}
	if !filepath.IsAbs(target) {
		return Result{}, fmt.Errorf("%w: target must be absolute", ErrApplyFailed)
	}
	runCtx, cancel := context.WithTimeout(ctx, updater.options.Timeout)
	defer cancel()
	unlock, err := lockUpdate(runCtx, updater.options.LockPath, updater.options.LockWait)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Result{}, err
		}
		return Result{}, preserveContext(runCtx, ErrApplyFailed)
	}
	defer unlock()
	manifest, artifact, result, err := updater.resolve(runCtx, currentVersion)
	if err != nil || result.Status != UpdateAvailable {
		return result, err
	}
	if err := updater.activate(runCtx, currentVersion, target, manifest, artifact); err != nil {
		return Result{}, err
	}
	result.Verified, result.Applied, result.SHA256 = true, true, artifact.SHA256
	return result, nil
}

func (updater *Updater) resolve(ctx context.Context, currentVersion string) (manifestPayload, manifestArtifact, Result, error) {
	if err := ctx.Err(); err != nil {
		return manifestPayload{}, manifestArtifact{}, Result{}, err
	}
	if _, ok := parseVersion(currentVersion); !ok {
		return manifestPayload{}, manifestArtifact{}, Result{}, ErrInvalidVersion
	}
	runCtx, cancel := context.WithTimeout(ctx, updater.options.Timeout)
	defer cancel()
	manifest, err := updater.fetchManifest(runCtx)
	if err != nil {
		return manifestPayload{}, manifestArtifact{}, Result{}, err
	}
	var selected manifestArtifact
	for _, candidate := range manifest.Artifacts {
		if candidate.Name == updater.artifact {
			selected = candidate
			break
		}
	}
	if selected.Name == "" {
		return manifestPayload{}, manifestArtifact{}, Result{}, fmt.Errorf("%w: selected artifact is absent", ErrInvalidManifest)
	}
	comparison, _ := compareVersions(currentVersion, manifest.Version)
	status := UpdateAvailable
	if comparison == 0 {
		status = UpToDate
	} else if comparison > 0 {
		status = CurrentNewer
	}
	result := Result{Channel: updater.channel, Artifact: updater.artifact, CurrentVersion: currentVersion, LatestVersion: manifest.Version, Status: status}
	return manifest, selected, result, nil
}

func (updater *Updater) fetchManifest(ctx context.Context) (manifestPayload, error) {
	reader, err := updater.source.FetchManifest(ctx, updater.channel, updater.options.MaxManifestBytes)
	if err != nil {
		return manifestPayload{}, preserveSourceError(ctx, err, ErrUpdateCheckFailed)
	}
	if reader == nil {
		return manifestPayload{}, ErrUpdateCheckFailed
	}
	defer reader.Close()
	raw, err := readBounded(reader, updater.options.MaxManifestBytes)
	if err != nil {
		return manifestPayload{}, ErrInvalidManifest
	}
	var envelope signedEnvelope
	if err := decodeStrict(raw, &envelope); err != nil || envelope.SchemaVersion != 1 {
		return manifestPayload{}, ErrInvalidManifest
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(envelope.Payload)
	if err != nil || int64(len(payload)) > updater.options.MaxManifestBytes {
		return manifestPayload{}, ErrInvalidManifest
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(updater.key, payload, signature) {
		return manifestPayload{}, ErrUntrustedManifest
	}
	var manifest manifestPayload
	if err := decodeStrict(payload, &manifest); err != nil || updater.validateManifest(manifest) != nil {
		return manifestPayload{}, ErrInvalidManifest
	}
	return manifest, nil
}

func (updater *Updater) validateManifest(manifest manifestPayload) error {
	if manifest.SchemaVersion != 1 || manifest.Channel != updater.channel || manifest.PublishedAt.IsZero() || len(manifest.Artifacts) == 0 || len(manifest.Artifacts) > 64 {
		return ErrInvalidManifest
	}
	if _, ok := parseVersion(manifest.Version); !ok {
		return ErrInvalidManifest
	}
	seen := map[string]struct{}{}
	for _, artifact := range manifest.Artifacts {
		if !artifactPattern.MatchString(artifact.Name) || artifact.Size <= 0 || artifact.Size > updater.options.MaxArtifactBytes {
			return ErrInvalidManifest
		}
		if _, duplicate := seen[artifact.Name]; duplicate {
			return ErrInvalidManifest
		}
		seen[artifact.Name] = struct{}{}
		if len(artifact.URL) == 0 || len(artifact.URL) > 2048 || !validArtifactURL(artifact.URL) ||
			!validGoImportPath(artifact.GoPackage) || !validGoImportPath(artifact.GoModule) {
			return ErrInvalidManifest
		}
		digest, err := hex.DecodeString(artifact.SHA256)
		if err != nil || len(digest) != sha256.Size || artifact.SHA256 != strings.ToLower(artifact.SHA256) {
			return ErrInvalidManifest
		}
	}
	return nil
}

func (updater *Updater) verifyArtifact(ctx context.Context, artifact manifestArtifact, output io.Writer) error {
	reader, err := updater.source.FetchArtifact(ctx, artifact.URL, artifact.Size)
	if err != nil {
		return preserveSourceError(ctx, err, ErrUpdateCheckFailed)
	}
	if reader == nil {
		return ErrUpdateCheckFailed
	}
	defer reader.Close()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(reader, artifact.Size+1))
	if err != nil {
		return preserveSourceError(ctx, err, ErrUpdateCheckFailed)
	}
	if written != artifact.Size || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return ErrArtifactMismatch
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (updater *Updater) activate(ctx context.Context, currentVersion, target string, manifest manifestPayload, artifact manifestArtifact) error {
	before, err := os.Lstat(target)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o111 == 0 || !ownedUpdateTarget(before) {
		return ErrApplyFailed
	}
	if err := updater.validateCurrent(target, currentVersion, artifact); err != nil {
		return ErrApplyFailed
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".dago-update-*")
	if err != nil {
		return ErrApplyFailed
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return ErrApplyFailed
	}
	if err := updater.verifyArtifact(ctx, artifact, temporary); err != nil {
		temporary.Close()
		return err
	}
	mode := before.Mode().Perm()
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return ErrApplyFailed
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return ErrApplyFailed
	}
	if err := temporary.Close(); err != nil {
		return ErrApplyFailed
	}
	if err := updater.validateExecutable(temporaryName, manifest, artifact); err != nil {
		return ErrArtifactMismatch
	}
	after, err := os.Lstat(target)
	if err != nil || !os.SameFile(before, after) {
		return ErrApplyFailed
	}
	if err := replaceUpdateFile(temporaryName, target); err != nil {
		return ErrApplyFailed
	}
	return syncUpdateDirectory(filepath.Dir(target))
}

func validGoImportPath(value string) bool {
	if value == "" || len(value) > 256 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.HasPrefix(segment, ".") {
			return false
		}
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && !strings.ContainsRune(".-_/", character) {
			return false
		}
	}
	return true
}

func validateGoExecutable(path string, manifest manifestPayload, artifact manifestArtifact) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil || info == nil || info.Path != artifact.GoPackage || info.Main.Path != artifact.GoModule ||
		info.Main.Version != manifest.Version || info.Main.Replace != nil {
		return ErrArtifactMismatch
	}
	return nil
}

func validateCurrentGoExecutable(path, currentVersion string, artifact manifestArtifact) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil || info == nil || info.Path != artifact.GoPackage || info.Main.Path != artifact.GoModule ||
		info.Main.Version != currentVersion || info.Main.Replace != nil {
		return ErrApplyFailed
	}
	return nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(value)) > limit {
		return nil, ErrInvalidManifest
	}
	return value, nil
}

func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrInvalidManifest
	}
	return nil
}

func preserveContext(ctx context.Context, fallback error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fallback
}

func preserveSourceError(ctx context.Context, err, fallback error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fallback
}

func defaultUpdateLockPath() string {
	if cache, err := os.UserCacheDir(); err == nil && filepath.IsAbs(cache) {
		return filepath.Join(cache, "dago", "self-update.lock")
	}
	return filepath.Join(os.TempDir(), "dago-self-update.lock")
}

type parsedVersion struct {
	core       [3]int64
	prerelease []string
}

func parseVersion(value string) (parsedVersion, bool) {
	if !strings.HasPrefix(value, "v") || len(value) > 128 {
		return parsedVersion{}, false
	}
	buildParts := strings.Split(value[1:], "+")
	if len(buildParts) > 2 || (len(buildParts) == 2 && !validVersionIdentifiers(buildParts[1])) {
		return parsedVersion{}, false
	}
	withoutBuild := buildParts[0]
	parts := strings.SplitN(withoutBuild, "-", 2)
	core := strings.Split(parts[0], ".")
	if len(core) != 3 {
		return parsedVersion{}, false
	}
	parsed := parsedVersion{}
	for index, component := range core {
		if component == "" || (len(component) > 1 && component[0] == '0') || len(component) > 9 {
			return parsedVersion{}, false
		}
		number, err := strconv.ParseInt(component, 10, 64)
		if err != nil || number < 0 {
			return parsedVersion{}, false
		}
		parsed.core[index] = number
	}
	if len(parts) == 2 {
		if parts[1] == "" {
			return parsedVersion{}, false
		}
		parsed.prerelease = strings.Split(parts[1], ".")
		for _, identifier := range parsed.prerelease {
			if identifier == "" || len(identifier) > 32 || (isDigits(identifier) && len(identifier) > 1 && identifier[0] == '0') {
				return parsedVersion{}, false
			}
			for _, character := range identifier {
				if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') && character != '-' {
					return parsedVersion{}, false
				}
			}
		}
	}
	return parsed, true
}

func validVersionIdentifiers(value string) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" || len(identifier) > 32 {
			return false
		}
		for _, character := range identifier {
			if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') && character != '-' {
				return false
			}
		}
	}
	return true
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func compareVersions(left, right string) (int, bool) {
	a, ok := parseVersion(left)
	if !ok {
		return 0, false
	}
	b, ok := parseVersion(right)
	if !ok {
		return 0, false
	}
	for index := range a.core {
		if a.core[index] < b.core[index] {
			return -1, true
		}
		if a.core[index] > b.core[index] {
			return 1, true
		}
	}
	if len(a.prerelease) == 0 && len(b.prerelease) == 0 {
		return 0, true
	}
	if len(a.prerelease) == 0 {
		return 1, true
	}
	if len(b.prerelease) == 0 {
		return -1, true
	}
	for index := 0; index < min(len(a.prerelease), len(b.prerelease)); index++ {
		leftID, rightID := a.prerelease[index], b.prerelease[index]
		leftNumeric, rightNumeric := isDigits(leftID), isDigits(rightID)
		switch {
		case leftNumeric && rightNumeric:
			if len(leftID) < len(rightID) {
				return -1, true
			}
			if len(leftID) > len(rightID) {
				return 1, true
			}
			if comparison := strings.Compare(leftID, rightID); comparison != 0 {
				return comparison, true
			}
		case leftNumeric:
			return -1, true
		case rightNumeric:
			return 1, true
		default:
			if comparison := strings.Compare(leftID, rightID); comparison != 0 {
				return comparison, true
			}
		}
	}
	if len(a.prerelease) < len(b.prerelease) {
		return -1, true
	}
	if len(a.prerelease) > len(b.prerelease) {
		return 1, true
	}
	return 0, true
}

// Package dacredential stores provider and service credentials in an
// owner-private, versioned file without owning provider SDKs or login flows.
package dacredential

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const storageVersion = 1

// DefaultPath returns the pinned user-level auth.json location. The home
// directory is required and positional so callers can select their identity
// boundary without hidden environment access.
func DefaultPath(homeDirectory string) string {
	if strings.TrimSpace(homeDirectory) == "" || strings.ContainsRune(homeDirectory, 0) {
		panic("dacredential: home directory is required")
	}
	return filepath.Join(filepath.Clean(homeDirectory), ".deepagents", ".state", "auth.json")
}

var (
	ErrInvalidCredential = errors.New("invalid credential")
	ErrInvalidStore      = errors.New("invalid credential store")
	ErrCredentialLimit   = errors.New("credential store limit exceeded")
)

// Type discriminates the persisted credential shapes.
type Type string

const (
	APIKeyType Type = "api_key"
	OAuthType  Type = "oauth"
)

// Options contains optional finite store limits.
type Options struct {
	MaxFileBytes     int64
	MaxCredentials   int
	MaxSecretBytes   int
	MaxMetadataBytes int
}

// DefaultOptions returns useful finite production limits.
func DefaultOptions() Options {
	return Options{
		MaxFileBytes:     1 << 20,
		MaxCredentials:   256,
		MaxSecretBytes:   64 << 10,
		MaxMetadataBytes: 4096,
	}
}

// Store manages one credential file. The path and clock are required
// positional dependencies; construction performs no I/O.
type Store struct {
	path    string
	now     func() time.Time
	options Options
	lock    chan struct{}
}

// NewStore constructs a credential store. Invalid static inputs panic and a
// zero Options value selects useful finite defaults.
func NewStore(path string, now func() time.Time, options Options) *Store {
	if path == "" || strings.ContainsRune(path, 0) || len(path) > 4096 {
		panic("dacredential: bounded store path is required")
	}
	if now == nil {
		panic("dacredential: clock is required")
	}
	defaults := DefaultOptions()
	if options.MaxFileBytes == 0 {
		options.MaxFileBytes = defaults.MaxFileBytes
	}
	if options.MaxCredentials == 0 {
		options.MaxCredentials = defaults.MaxCredentials
	}
	if options.MaxSecretBytes == 0 {
		options.MaxSecretBytes = defaults.MaxSecretBytes
	}
	if options.MaxMetadataBytes == 0 {
		options.MaxMetadataBytes = defaults.MaxMetadataBytes
	}
	if options.MaxFileBytes < 1 || options.MaxFileBytes > 16<<20 ||
		options.MaxCredentials < 1 || options.MaxCredentials > 4096 ||
		options.MaxSecretBytes < 1 || options.MaxSecretBytes > 1<<20 ||
		options.MaxMetadataBytes < 1 || options.MaxMetadataBytes > 64<<10 {
		panic("dacredential: limits are outside their finite ranges")
	}
	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	return &Store{path: filepath.Clean(path), now: now, options: options, lock: lock}
}

// Path returns the configured path without filesystem access.
func (store *Store) Path() string { return store.path }

// APIKeyCredential is a stored API key and its non-secret paired metadata.
type APIKeyCredential struct {
	Key     string
	AddedAt time.Time
	BaseURL string
	Project string
}

func (APIKeyCredential) String() string   { return "APIKeyCredential(<redacted>)" }
func (APIKeyCredential) GoString() string { return "APIKeyCredential(<redacted>)" }

// OAuthCredential is a stored OAuth token pair. OAuth refresh and login remain
// the responsibility of the provider integration.
type OAuthCredential struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

func (OAuthCredential) String() string   { return "OAuthCredential(<redacted>)" }
func (OAuthCredential) GoString() string { return "OAuthCredential(<redacted>)" }

// Credential is one discriminated stored record.
type Credential struct {
	Type   Type
	APIKey *APIKeyCredential
	OAuth  *OAuthCredential
}

func (credential Credential) String() string {
	return fmt.Sprintf("Credential(type=%s,<redacted>)", credential.Type)
}

func (credential Credential) GoString() string { return credential.String() }

// Snapshot is an immutable copy of one successfully decoded store.
type Snapshot struct {
	credentials map[string]Credential
	malformed   int
}

// Malformed reports how many fail-closed credential records were ignored.
func (snapshot Snapshot) Malformed() int { return snapshot.malformed }

// Providers returns sorted stored provider and service identifiers.
func (snapshot Snapshot) Providers() []string {
	providers := make([]string, 0, len(snapshot.credentials))
	for provider := range snapshot.credentials {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	return providers
}

// Credential returns a defensive copy of one stored record.
func (snapshot Snapshot) Credential(provider string) (Credential, bool) {
	credential, ok := snapshot.credentials[provider]
	if !ok {
		return Credential{}, false
	}
	return cloneCredential(credential), true
}

// APIKey returns one stored API-key record and false for missing or OAuth data.
func (snapshot Snapshot) APIKey(provider string) (APIKeyCredential, bool) {
	credential, ok := snapshot.credentials[provider]
	if !ok || credential.Type != APIKeyType || credential.APIKey == nil {
		return APIKeyCredential{}, false
	}
	return *credential.APIKey, true
}

// Load reads the store. A missing file produces a useful empty snapshot.
func (store *Store) Load(ctx context.Context) (Snapshot, error) {
	if ctx == nil {
		panic("dacredential: nil context")
	}
	if err := store.acquire(ctx); err != nil {
		return Snapshot{}, err
	}
	defer store.release()
	return store.loadLocked(ctx)
}

// SetAPIKey stores an API key, replacing the whole provider record. Required
// provider and key values are positional; endpoint and project are optional.
func (store *Store) SetAPIKey(ctx context.Context, provider, key, baseURL, project string) error {
	if ctx == nil {
		panic("dacredential: nil context")
	}
	provider, err := validateProvider(provider)
	if err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if err := validateSecret(key, store.options.MaxSecretBytes); err != nil {
		return err
	}
	baseURL, err = validateBaseURL(baseURL, store.options.MaxMetadataBytes)
	if err != nil {
		return err
	}
	project = strings.TrimSpace(project)
	if len(project) > store.options.MaxMetadataBytes || strings.ContainsAny(project, "\x00\r\n") {
		return fmt.Errorf("%w: project metadata is invalid", ErrInvalidCredential)
	}
	if project != "" && provider != "langsmith" {
		return fmt.Errorf("%w: project is only valid for langsmith", ErrInvalidCredential)
	}
	entry := Credential{Type: APIKeyType, APIKey: &APIKeyCredential{
		Key: key, AddedAt: store.now().UTC().Truncate(time.Second), BaseURL: baseURL, Project: project,
	}}
	return store.update(ctx, func(credentials map[string]Credential) (bool, error) {
		credentials[provider] = entry
		return true, nil
	})
}

// SetOAuth stores an OAuth token record. All required values are positional.
func (store *Store) SetOAuth(ctx context.Context, provider, accessToken, refreshToken string, expiresAt time.Time) error {
	if ctx == nil {
		panic("dacredential: nil context")
	}
	provider, err := validateProvider(provider)
	if err != nil {
		return err
	}
	accessToken, refreshToken = strings.TrimSpace(accessToken), strings.TrimSpace(refreshToken)
	if err := validateSecret(accessToken, store.options.MaxSecretBytes); err != nil {
		return err
	}
	if err := validateSecret(refreshToken, store.options.MaxSecretBytes); err != nil {
		return err
	}
	if expiresAt.IsZero() {
		return fmt.Errorf("%w: OAuth expiry is required", ErrInvalidCredential)
	}
	entry := Credential{Type: OAuthType, OAuth: &OAuthCredential{
		AccessToken: accessToken, RefreshToken: refreshToken, ExpiresAt: expiresAt.UTC(),
	}}
	return store.update(ctx, func(credentials map[string]Credential) (bool, error) {
		credentials[provider] = entry
		return true, nil
	})
}

// Remove deletes one provider record and reports whether it existed.
func (store *Store) Remove(ctx context.Context, provider string) (bool, error) {
	if ctx == nil {
		panic("dacredential: nil context")
	}
	provider, err := validateProvider(provider)
	if err != nil {
		return false, err
	}
	removed := false
	err = store.update(ctx, func(credentials map[string]Credential) (bool, error) {
		_, removed = credentials[provider]
		delete(credentials, provider)
		return removed, nil
	})
	return removed, err
}

func (store *Store) update(ctx context.Context, change func(map[string]Credential) (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.acquire(ctx); err != nil {
		return err
	}
	defer store.release()
	snapshot, err := store.loadLocked(ctx)
	if err != nil {
		return err
	}
	credentials := cloneCredentials(snapshot.credentials)
	changed, err := change(credentials)
	if err != nil || !changed {
		return err
	}
	if len(credentials) > store.options.MaxCredentials {
		return fmt.Errorf("%w: too many credentials", ErrCredentialLimit)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.writeLocked(ctx, credentials)
}

type envelope struct {
	Version     int                        `json:"version"`
	Credentials map[string]json.RawMessage `json:"credentials"`
}

type apiKeyRecord struct {
	Type    Type   `json:"type"`
	Key     string `json:"key"`
	AddedAt string `json:"added_at"`
	BaseURL string `json:"base_url,omitempty"`
	Project string `json:"project,omitempty"`
}

type oauthRecord struct {
	Type         Type   `json:"type"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
}

func (store *Store) loadLocked(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	payload, err := store.readLocked()
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{credentials: map[string]Credential{}}, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	var document envelope
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&document); err != nil {
		return Snapshot{}, fmt.Errorf("%w: decode auth.json", ErrInvalidStore)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return Snapshot{}, fmt.Errorf("%w: trailing auth.json data", ErrInvalidStore)
	}
	if document.Version != storageVersion {
		return Snapshot{}, fmt.Errorf("%w: unsupported auth.json version", ErrInvalidStore)
	}
	if len(document.Credentials) > store.options.MaxCredentials {
		return Snapshot{}, fmt.Errorf("%w: too many credentials", ErrCredentialLimit)
	}
	result := Snapshot{credentials: make(map[string]Credential, len(document.Credentials))}
	for provider, raw := range document.Credentials {
		normalized, providerErr := validateProvider(provider)
		if providerErr != nil || normalized != provider {
			result.malformed++
			continue
		}
		var discriminator struct {
			Type Type `json:"type"`
		}
		if err := json.Unmarshal(raw, &discriminator); err != nil {
			result.malformed++
			continue
		}
		switch discriminator.Type {
		case APIKeyType:
			var record apiKeyRecord
			if err := strictDecode(raw, &record); err != nil {
				result.malformed++
				continue
			}
			key := strings.TrimSpace(record.Key)
			addedAt, timeErr := time.Parse(time.RFC3339, record.AddedAt)
			baseURL, urlErr := validateBaseURL(record.BaseURL, store.options.MaxMetadataBytes)
			if validateSecret(key, store.options.MaxSecretBytes) != nil || timeErr != nil || urlErr != nil ||
				len(record.Project) > store.options.MaxMetadataBytes || strings.ContainsAny(record.Project, "\x00\r\n") ||
				(record.Project != "" && provider != "langsmith") {
				result.malformed++
				continue
			}
			result.credentials[provider] = Credential{Type: APIKeyType, APIKey: &APIKeyCredential{
				Key: key, AddedAt: addedAt, BaseURL: baseURL, Project: record.Project,
			}}
		case OAuthType:
			var record oauthRecord
			if err := strictDecode(raw, &record); err != nil {
				result.malformed++
				continue
			}
			expiresAt, timeErr := time.Parse(time.RFC3339, record.ExpiresAt)
			if validateSecret(record.AccessToken, store.options.MaxSecretBytes) != nil ||
				validateSecret(record.RefreshToken, store.options.MaxSecretBytes) != nil || timeErr != nil {
				result.malformed++
				continue
			}
			result.credentials[provider] = Credential{Type: OAuthType, OAuth: &OAuthCredential{
				AccessToken: record.AccessToken, RefreshToken: record.RefreshToken, ExpiresAt: expiresAt,
			}}
		default:
			result.malformed++
		}
	}
	return result, nil
}

func (store *Store) readLocked() ([]byte, error) {
	info, err := os.Lstat(store.path)
	if err != nil {
		return nil, fmt.Errorf("read credential store: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > store.options.MaxFileBytes {
		return nil, fmt.Errorf("%w: auth.json must be a bounded regular file", ErrInvalidStore)
	}
	file, err := os.Open(store.path)
	if err != nil {
		return nil, fmt.Errorf("read credential store: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() > store.options.MaxFileBytes {
		return nil, fmt.Errorf("%w: auth.json changed before reading", ErrInvalidStore)
	}
	payload, err := io.ReadAll(io.LimitReader(file, store.options.MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read credential store: %w", err)
	}
	if int64(len(payload)) > store.options.MaxFileBytes {
		return nil, fmt.Errorf("%w: auth.json is too large", ErrCredentialLimit)
	}
	final, err := file.Stat()
	if err != nil || !final.Mode().IsRegular() || !os.SameFile(opened, final) || final.Size() > store.options.MaxFileBytes {
		return nil, fmt.Errorf("%w: auth.json changed while reading", ErrInvalidStore)
	}
	return payload, nil
}

func (store *Store) writeLocked(ctx context.Context, credentials map[string]Credential) error {
	raw := make(map[string]any, len(credentials))
	for provider, credential := range credentials {
		switch credential.Type {
		case APIKeyType:
			if credential.APIKey == nil {
				return fmt.Errorf("%w: missing API-key record", ErrInvalidCredential)
			}
			raw[provider] = apiKeyRecord{
				Type: APIKeyType, Key: credential.APIKey.Key,
				AddedAt: credential.APIKey.AddedAt.UTC().Format(time.RFC3339),
				BaseURL: credential.APIKey.BaseURL, Project: credential.APIKey.Project,
			}
		case OAuthType:
			if credential.OAuth == nil {
				return fmt.Errorf("%w: missing OAuth record", ErrInvalidCredential)
			}
			raw[provider] = oauthRecord{
				Type: OAuthType, AccessToken: credential.OAuth.AccessToken,
				RefreshToken: credential.OAuth.RefreshToken,
				ExpiresAt:    credential.OAuth.ExpiresAt.UTC().Format(time.RFC3339),
			}
		default:
			return fmt.Errorf("%w: unsupported credential type", ErrInvalidCredential)
		}
	}
	payload, err := json.Marshal(envelopeForWrite{Version: storageVersion, Credentials: raw})
	if err != nil {
		return fmt.Errorf("%w: encode auth.json", ErrInvalidStore)
	}
	if int64(len(payload)) > store.options.MaxFileBytes {
		return fmt.Errorf("%w: encoded auth.json is too large", ErrCredentialLimit)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parent := filepath.Dir(store.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return fmt.Errorf("secure credential directory: %w", err)
	}
	if info, err := os.Lstat(store.path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("%w: auth.json target is not a regular file", ErrInvalidStore)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect auth.json target: %w", err)
	}
	temporary, err := os.CreateTemp(parent, ".auth-*")
	if err != nil {
		return fmt.Errorf("create auth.json temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure auth.json temporary file: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write auth.json: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync auth.json: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close auth.json: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := replaceCredentialFile(temporaryPath, store.path); err != nil {
		return fmt.Errorf("replace auth.json: %w", err)
	}
	if err := os.Chmod(store.path, 0o600); err != nil {
		return fmt.Errorf("secure auth.json: %w", err)
	}
	if directory, err := os.Open(parent); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

type envelopeForWrite struct {
	Version     int            `json:"version"`
	Credentials map[string]any `json:"credentials"`
}

func validateProvider(provider string) (string, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" || len(provider) > 128 {
		return "", fmt.Errorf("%w: provider name is required and bounded", ErrInvalidCredential)
	}
	for index, char := range provider {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || (index > 0 && (char == '_' || char == '-' || char == '.')) {
			continue
		}
		return "", fmt.Errorf("%w: provider name contains unsupported characters", ErrInvalidCredential)
	}
	return provider, nil
}

func validateSecret(secret string, maximum int) error {
	if secret == "" || len(secret) > maximum {
		return fmt.Errorf("%w: secret is empty or exceeds its bound", ErrInvalidCredential)
	}
	for _, character := range secret {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%w: secret contains control characters", ErrInvalidCredential)
		}
	}
	return nil
}

func validateBaseURL(value string, maximum int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > maximum || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%w: base URL is invalid", ErrInvalidCredential)
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("%w: base URL must be an http(s) URL without credentials", ErrInvalidCredential)
	}
	return value, nil
}

func strictDecode(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureJSONEnd(decoder)
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("extra JSON value")
	}
	return nil
}

func cloneCredential(credential Credential) Credential {
	copy := Credential{Type: credential.Type}
	if credential.APIKey != nil {
		value := *credential.APIKey
		copy.APIKey = &value
	}
	if credential.OAuth != nil {
		value := *credential.OAuth
		copy.OAuth = &value
	}
	return copy
}

func cloneCredentials(credentials map[string]Credential) map[string]Credential {
	copy := make(map[string]Credential, len(credentials))
	for provider, credential := range credentials {
		copy[provider] = cloneCredential(credential)
	}
	return copy
}

func (store *Store) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-store.lock:
		return nil
	}
}

func (store *Store) release() { store.lock <- struct{}{} }

package modelconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/semistrict/dago/daconfig"
)

const (
	defaultModelKey = "models.default"
	recentModelKey  = "models.recent"
)

var ErrNoCredentials = errors.New("no model credentials configured")

// PreferenceManifest returns the two persistable model-selection keys. Callers
// may include equivalent declarations in a larger application manifest.
func PreferenceManifest() *daconfig.Manifest {
	return daconfig.NewManifest(
		daconfig.Option{Key: defaultModelKey, Group: "Models", Summary: "Default model used for new runs", Kind: daconfig.KindString, Default: "", Persist: true},
		daconfig.Option{Key: recentModelKey, Group: "Models", Summary: "Most recently selected model", Kind: daconfig.KindString, Default: "", Persist: true},
	)
}

// PreferenceStore persists default and recent provider:model selections using
// the caller-owned owner-private atomic configuration store.
type PreferenceStore struct{ store *daconfig.Store }

// NewPreferenceStore constructs a preference store without I/O.
func NewPreferenceStore(store *daconfig.Store) *PreferenceStore {
	if store == nil {
		panic("modelconfig: preference config store is required")
	}
	return &PreferenceStore{store: store}
}

// Default returns the persisted default model, or an empty string when unset.
func (preferences *PreferenceStore) Default(ctx context.Context) (string, error) {
	return preferences.get(ctx, defaultModelKey)
}

// Recent returns the persisted recent model, or an empty string when unset.
func (preferences *PreferenceStore) Recent(ctx context.Context) (string, error) {
	return preferences.get(ctx, recentModelKey)
}

// SetDefault atomically persists one explicit provider:model selection.
func (preferences *PreferenceStore) SetDefault(ctx context.Context, modelSpec string) error {
	normalized, err := normalizePersistedSpec(modelSpec)
	if err != nil {
		return err
	}
	return preferences.store.Set(ctx, defaultModelKey, normalized)
}

// ClearDefault atomically removes the persisted default.
func (preferences *PreferenceStore) ClearDefault(ctx context.Context) (bool, error) {
	return preferences.store.Unset(ctx, defaultModelKey)
}

// SetRecent atomically persists one explicit provider:model selection.
func (preferences *PreferenceStore) SetRecent(ctx context.Context, modelSpec string) error {
	normalized, err := normalizePersistedSpec(modelSpec)
	if err != nil {
		return err
	}
	return preferences.store.Set(ctx, recentModelKey, normalized)
}

func (preferences *PreferenceStore) get(ctx context.Context, key string) (string, error) {
	if ctx == nil {
		panic("modelconfig: nil context")
	}
	layer, err := preferences.store.Load(ctx)
	if err != nil {
		return "", err
	}
	value, _ := layer.Values()[key].(string)
	return value, nil
}

func normalizePersistedSpec(modelSpec string) (string, error) {
	if strings.TrimSpace(modelSpec) != modelSpec || len(modelSpec) > 1024 || strings.ContainsAny(modelSpec, "\x00\r\n") {
		return "", fmt.Errorf("%w: persisted model is malformed", ErrInvalidSpec)
	}
	provider, model, found := strings.Cut(modelSpec, ":")
	provider = normalizeProvider(provider)
	if !found || !providerIdentifier(provider) || strings.TrimSpace(model) == "" || strings.TrimSpace(model) != model {
		return "", fmt.Errorf("%w: persisted model must use provider:model", ErrInvalidSpec)
	}
	return provider + ":" + model, nil
}

// ResolvePreferred selects a persisted default, then persisted recent model,
// then the pinned credential-based defaults. It does not probe any provider.
func (resolver *Resolver) ResolvePreferred(ctx context.Context, preferences *PreferenceStore, options ResolveOptions) (Resolution, error) {
	if preferences == nil {
		panic("modelconfig: preferences are required")
	}
	configured, err := preferences.Default(ctx)
	if err != nil {
		return Resolution{}, err
	}
	if configured == "" {
		configured, err = preferences.Recent(ctx)
		if err != nil {
			return Resolution{}, err
		}
	}
	if configured != "" {
		return resolver.Resolve(ctx, configured, options)
	}
	candidates := []struct {
		provider string
		model    string
	}{
		{provider: "openai", model: "gpt-5.6-terra"},
		{provider: "anthropic", model: "claude-opus-5"},
		{provider: "google_genai", model: "gemini-3.1-pro-preview"},
	}
	for _, candidate := range candidates {
		provider := resolver.providers[candidate.provider]
		credential, resolveErr := resolver.resolveCredential(ctx, provider)
		if resolveErr != nil {
			return Resolution{}, resolveErr
		}
		if credential.Configured {
			return resolver.Resolve(ctx, candidate.provider+":"+candidate.model, options)
		}
	}
	return Resolution{}, ErrNoCredentials
}

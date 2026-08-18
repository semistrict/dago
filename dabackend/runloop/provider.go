package runloop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	// EnvBlueprintID names the upstream-compatible blueprint-ID override.
	EnvBlueprintID = "RUNLOOP_SANDBOX_BLUEPRINT_ID"
	// EnvBlueprintName names the upstream-compatible blueprint-name default.
	EnvBlueprintName = "RUNLOOP_SANDBOX_BLUEPRINT_NAME"

	upstreamEnvPrefix          = "DEEPAGENTS_CODE_"
	defaultBlueprintDockerfile = "FROM python:3\n"
	defaultBlueprintPageSize   = 100
)

var (
	// ErrSandboxNotFound is the stable classification a Client should wrap when
	// Attach targets a missing devbox. Provider preserves it for errors.Is.
	ErrSandboxNotFound = errors.New("runloop sandbox not found")
	// ErrBlueprintNotReady classifies an existing blueprint whose build is not
	// complete. Provider never starts a duplicate build in that situation.
	ErrBlueprintNotReady = errors.New("runloop blueprint not ready")
)

// Blueprint is the lifecycle subset needed to reuse or build a named image.
type Blueprint struct {
	ID     string
	Name   string
	Status string
}

// BlueprintPage is one cursor page. A non-empty final item ID is required when
// HasMore is true so pagination cannot loop forever.
type BlueprintPage struct {
	Blueprints []Blueprint
	HasMore    bool
}

// Client combines sandbox I/O with the narrow devbox and blueprint lifecycle
// operations used by Provider. It deliberately contains no credential API;
// applications construct an authenticated implementation explicitly.
type Client interface {
	SandboxTransport
	Attach(context.Context, string) error
	Create(context.Context) (string, error)
	CreateFromBlueprintID(context.Context, string) (string, error)
	CreateFromBlueprintName(context.Context, string) (string, error)
	ListBlueprints(context.Context, string, string, int) (BlueprintPage, error)
	BuildBlueprint(context.Context, string, string) error
	Shutdown(context.Context, string) error
}

// EnvResolver returns an environment value and whether it is present. Empty
// values are treated as unset.
type EnvResolver func(string) (string, bool)

// ProviderOptions configures lifecycle resolution and the resulting backends.
// Zero values read the process environment and use the partner defaults.
type ProviderOptions struct {
	Backend                    Options
	ResolveEnv                 EnvResolver
	DefaultBlueprintDockerfile string
	BlueprintPageSize          int
}

// SandboxOptions selects attach, blueprint, or fresh-devbox creation. An
// explicit SandboxID bypasses all blueprint resolution. An environment
// blueprint ID wins over Snapshot, which wins over an environment name.
type SandboxOptions struct {
	SandboxID           string
	Snapshot            string
	BlueprintDockerfile string
}

// Provider owns no resources itself. GetOrCreate and Delete are the explicit
// billable lifecycle boundaries.
type Provider struct {
	client                     Client
	backend                    Options
	resolveEnv                 EnvResolver
	defaultBlueprintDockerfile string
	blueprintPageSize          int
}

// NewProvider constructs lifecycle support around a caller-authenticated
// client. A nil client or invalid static option panics.
func NewProvider(client Client, options ProviderOptions) *Provider {
	if isNil(client) {
		panic("runloop provider: client is required")
	}
	if options.BlueprintPageSize < 0 {
		panic("runloop provider: blueprint page size cannot be negative")
	}
	applyOptions(&options.Backend)
	if options.ResolveEnv == nil {
		options.ResolveEnv = os.LookupEnv
	}
	if options.DefaultBlueprintDockerfile == "" {
		options.DefaultBlueprintDockerfile = defaultBlueprintDockerfile
	}
	if options.BlueprintPageSize == 0 {
		options.BlueprintPageSize = defaultBlueprintPageSize
	}
	return &Provider{
		client:                     client,
		backend:                    options.Backend,
		resolveEnv:                 options.ResolveEnv,
		defaultBlueprintDockerfile: options.DefaultBlueprintDockerfile,
		blueprintPageSize:          options.BlueprintPageSize,
	}
}

// GetOrCreate attaches to the requested devbox or explicitly creates a remote
// devbox. A returned backend always retains the ID selected by this operation.
func (provider *Provider) GetOrCreate(ctx context.Context, options SandboxOptions) (*Backend, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sandboxID := strings.TrimSpace(options.SandboxID); sandboxID != "" {
		err := provider.client.Attach(ctx, sandboxID)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			if errors.Is(err, ErrSandboxNotFound) {
				return nil, fmt.Errorf("%w: %q", ErrSandboxNotFound, sandboxID)
			}
			return nil, wrapTransportError(fmt.Sprintf("runloop provider: attach %q", sandboxID), err)
		}
		return New(provider.client, sandboxID, provider.backend), nil
	}

	blueprintID := provider.env(EnvBlueprintID)
	blueprintName := strings.TrimSpace(options.Snapshot)
	if blueprintName == "" {
		blueprintName = provider.env(EnvBlueprintName)
	}

	var (
		sandboxID string
		err       error
	)
	switch {
	case blueprintID != "":
		sandboxID, err = provider.client.CreateFromBlueprintID(ctx, blueprintID)
	case blueprintName != "":
		dockerfile := options.BlueprintDockerfile
		if dockerfile == "" {
			dockerfile = provider.defaultBlueprintDockerfile
		}
		if err = provider.ensureBlueprint(ctx, blueprintName, dockerfile); err == nil {
			sandboxID, err = provider.client.CreateFromBlueprintName(ctx, blueprintName)
		}
	default:
		sandboxID, err = provider.client.Create(ctx)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, wrapTransportError("runloop provider: create devbox", err)
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, fmt.Errorf("runloop provider: create devbox returned an empty id")
	}
	return New(provider.client, sandboxID, provider.backend), nil
}

func (provider *Provider) ensureBlueprint(ctx context.Context, name, dockerfile string) error {
	var (
		cursor         string
		notReadyStatus string
	)
	seenCursors := map[string]struct{}{"": {}}
	for {
		page, err := provider.client.ListBlueprints(ctx, name, cursor, provider.blueprintPageSize)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return wrapTransportError("runloop provider: list blueprints", err)
		}
		for _, blueprint := range page.Blueprints {
			if blueprint.Name != name {
				continue
			}
			if blueprint.Status == "build_complete" {
				return nil
			}
			notReadyStatus = blueprint.Status
		}
		if !page.HasMore {
			break
		}
		if len(page.Blueprints) == 0 {
			return fmt.Errorf("runloop provider: invalid blueprint page: has_more without entries")
		}
		next := strings.TrimSpace(page.Blueprints[len(page.Blueprints)-1].ID)
		if next == "" {
			return fmt.Errorf("runloop provider: invalid blueprint page: missing cursor id")
		}
		if _, duplicate := seenCursors[next]; duplicate {
			return fmt.Errorf("runloop provider: invalid blueprint page: repeated cursor %q", next)
		}
		seenCursors[next] = struct{}{}
		cursor = next
	}
	if notReadyStatus != "" {
		if notReadyStatus == "failed" {
			return fmt.Errorf("%w: blueprint %q last build failed; delete it or fix the Dockerfile", ErrBlueprintNotReady, name)
		}
		return fmt.Errorf("%w: blueprint %q is still building (state %q)", ErrBlueprintNotReady, name, notReadyStatus)
	}
	err := provider.client.BuildBlueprint(ctx, name, dockerfile)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return wrapTransportError(fmt.Sprintf("runloop provider: build blueprint %q", name), err)
	}
	return nil
}

func (provider *Provider) env(name string) string {
	if value, ok := provider.resolveEnv(upstreamEnvPrefix + name); ok {
		return strings.TrimSpace(value)
	}
	value, ok := provider.resolveEnv(name)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

// Delete shuts down sandboxID. Deletion is explicit and preserves transport
// errors for errors.Is checks.
func (provider *Provider) Delete(ctx context.Context, sandboxID string) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return fmt.Errorf("runloop provider: sandbox id is required")
	}
	err := provider.client.Shutdown(ctx, sandboxID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return wrapTransportError(fmt.Sprintf("runloop provider: shutdown %q", sandboxID), err)
	}
	return nil
}

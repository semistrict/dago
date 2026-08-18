package dagent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode"

	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
)

const (
	// RuntimeModelConfigKey is the default per-run configurable selection key.
	RuntimeModelConfigKey = "model"
	// RuntimeModelStateKey is the default private checkpoint field.
	RuntimeModelStateKey = "_runtime_model_spec"
)

// ErrInvalidRuntimeModel identifies malformed runtime model selections.
var ErrInvalidRuntimeModel = errors.New("invalid runtime model selection")

// ModelResolver resolves an application-owned model spec for one invocation.
// Implementations own provider clients, credentials, caching, and network I/O
// and must honor ctx.
type ModelResolver interface {
	ResolveModel(context.Context, string) (damodel.Chat, error)
}

// ModelResolverFunc adapts a function to ModelResolver.
type ModelResolverFunc func(context.Context, string) (damodel.Chat, error)

// ResolveModel invokes the adapted function.
func (resolve ModelResolverFunc) ResolveModel(ctx context.Context, spec string) (damodel.Chat, error) {
	return resolve(ctx, spec)
}

// RuntimeModelOptions controls keys, bounds, and checkpoint persistence.
type RuntimeModelOptions struct {
	ConfigKey    string
	StateKey     string
	MaxSpecRunes int
	Ephemeral    bool
}

// DefaultRuntimeModelOptions returns finite, persistent production defaults.
func DefaultRuntimeModelOptions() RuntimeModelOptions {
	return RuntimeModelOptions{ConfigKey: RuntimeModelConfigKey, StateKey: RuntimeModelStateKey, MaxSpecRunes: 512}
}

// RuntimeModel returns middleware that selects request.Model from immutable
// per-run Configurable values. The resolver is a required positional
// dependency; static invalid inputs panic and resolution failures remain run
// errors. Explicit selections persist in private thread state by default.
func RuntimeModel(resolver ModelResolver, options RuntimeModelOptions) Middleware {
	if nilInterface(resolver) {
		panic("dagent: runtime model resolver is nil")
	}
	defaults := DefaultRuntimeModelOptions()
	if options.ConfigKey == "" {
		options.ConfigKey = defaults.ConfigKey
	}
	if options.StateKey == "" {
		options.StateKey = defaults.StateKey
	}
	if options.MaxSpecRunes == 0 {
		options.MaxSpecRunes = defaults.MaxSpecRunes
	}
	if !validRuntimeKey(options.ConfigKey) || !validRuntimeKey(options.StateKey) || options.MaxSpecRunes < 1 || options.MaxSpecRunes > 4096 {
		panic("dagent: invalid runtime model options")
	}
	middleware := Middleware{Name: "runtime_model"}
	if !options.Ephemeral {
		middleware.Fields = map[string]StateField{options.StateKey: Field(FieldSpec[string]{
			Kind: FieldLast, Contract: "dago.runtime-model-spec.v1", Private: true,
			Clone: func(value string) string { return value },
		})}
	}
	middleware.WrapModelCall = func(ctx context.Context, request ModelRequest, next ModelHandler) (response ModelResponse, err error) {
		spec, explicit, err := runtimeModelSpec(request, options)
		if err != nil {
			return ModelResponse{}, err
		}
		if spec != "" && !modelMatchesSpec(request.Model, spec) {
			selected, resolveErr := resolveRuntimeModel(ctx, resolver, spec)
			if resolveErr != nil {
				return ModelResponse{}, resolveErr
			}
			request.Model = selected
		}
		response, err = next(ctx, request)
		if err != nil {
			return response, err
		}
		if !options.Ephemeral && explicit {
			if response.Update == nil {
				response.Update = dastate.Values{}
			} else {
				response.Update = response.Update.Clone()
			}
			response.Update[options.StateKey] = spec
		}
		return response, nil
	}
	return middleware
}

func runtimeModelSpec(request ModelRequest, options RuntimeModelOptions) (string, bool, error) {
	raw, explicit := request.Runtime.Configurable.Get(options.ConfigKey)
	if !explicit && !options.Ephemeral {
		raw, explicit = request.State[options.StateKey]
		if explicit {
			// Checkpoint state is fallback, not a new persistence request.
			spec, _, err := validateRuntimeModelSpec(raw, options.MaxSpecRunes, true)
			return spec, false, err
		}
	}
	return validateRuntimeModelSpec(raw, options.MaxSpecRunes, explicit)
}

func validateRuntimeModelSpec(raw any, limit int, explicit bool) (string, bool, error) {
	if !explicit {
		return "", false, nil
	}
	spec, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("%w: model must be a string", ErrInvalidRuntimeModel)
	}
	if spec != strings.TrimSpace(spec) || len([]rune(spec)) > limit {
		return "", true, fmt.Errorf("%w: model is padded or too long", ErrInvalidRuntimeModel)
	}
	for _, value := range spec {
		if unicode.IsControl(value) {
			return "", true, fmt.Errorf("%w: model contains control characters", ErrInvalidRuntimeModel)
		}
	}
	return spec, true, nil
}

func resolveRuntimeModel(ctx context.Context, resolver ModelResolver, spec string) (model damodel.Chat, err error) {
	defer func() {
		if recover() != nil {
			model, err = nil, errors.New("runtime model resolver panicked")
		}
	}()
	model, err = resolver.ResolveModel(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime model: %w", err)
	}
	if nilInterface(model) {
		return nil, errors.New("runtime model resolver returned nil")
	}
	return model, nil
}

func modelMatchesSpec(model damodel.Chat, spec string) bool {
	if nilInterface(model) {
		return false
	}
	profile := model.Profile()
	if profile.Provider != "" && profile.Model != "" && spec == profile.Provider+":"+profile.Model {
		return true
	}
	return profile.Model != "" && spec == profile.Model
}

func validRuntimeKey(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character != '_' && character != '.' && character != '-' &&
			(character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

package modelconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/semistrict/dago/damodel"
)

var ErrInvalidOptions = errors.New("invalid model options")

type valueLimits struct {
	maxEntries int
	maxDepth   int
	maxBytes   int
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = cloneUnchecked(item)
	}
	return result
}

func cloneUnchecked(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneMap(value)
	case map[string]string:
		result := make(map[string]string, len(value))
		for key, item := range value {
			result[key] = item
		}
		return result
	case []any:
		result := make([]any, len(value))
		for index, item := range value {
			result[index] = cloneUnchecked(item)
		}
		return result
	case []string:
		return append([]string(nil), value...)
	default:
		return value
	}
}

func validateAndCloneMap(value map[string]any, limits valueLimits) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	entries, bytes := 0, 0
	cloned, err := validateAndCloneValue(value, 0, limits, &entries, &bytes)
	if err != nil {
		return nil, err
	}
	return cloned.(map[string]any), nil
}

func validateAndCloneValue(value any, depth int, limits valueLimits, entries, bytes *int) (any, error) {
	if depth > limits.maxDepth {
		return nil, fmt.Errorf("%w: nesting exceeds %d", ErrInvalidOptions, limits.maxDepth)
	}
	switch value := value.(type) {
	case nil, bool:
		return value, nil
	case string:
		*bytes += len(value)
		if *bytes > limits.maxBytes || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("%w: string data is not bounded", ErrInvalidOptions)
		}
		return value, nil
	case json.Number:
		if _, err := value.Float64(); err != nil {
			return nil, fmt.Errorf("%w: invalid number", ErrInvalidOptions)
		}
		return value, nil
	case float32:
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("%w: non-finite number", ErrInvalidOptions)
		}
		return value, nil
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("%w: non-finite number", ErrInvalidOptions)
		}
		return value, nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return value, nil
	case map[string]any:
		if len(value) > limits.maxEntries || *entries+len(value) > limits.maxEntries {
			return nil, fmt.Errorf("%w: option count exceeds %d", ErrInvalidOptions, limits.maxEntries)
		}
		*entries += len(value)
		result := make(map[string]any, len(value))
		for key, item := range value {
			if key == "" || len(key) > 256 || strings.TrimSpace(key) != key || strings.ContainsRune(key, 0) {
				return nil, fmt.Errorf("%w: invalid option name", ErrInvalidOptions)
			}
			*bytes += len(key)
			cloned, err := validateAndCloneValue(item, depth+1, limits, entries, bytes)
			if err != nil {
				return nil, err
			}
			result[key] = cloned
		}
		return result, nil
	case map[string]string:
		converted := make(map[string]any, len(value))
		for key, item := range value {
			converted[key] = item
		}
		return validateAndCloneValue(converted, depth, limits, entries, bytes)
	case []any:
		if len(value) > limits.maxEntries || *entries+len(value) > limits.maxEntries {
			return nil, fmt.Errorf("%w: option count exceeds %d", ErrInvalidOptions, limits.maxEntries)
		}
		*entries += len(value)
		result := make([]any, len(value))
		for index, item := range value {
			cloned, err := validateAndCloneValue(item, depth+1, limits, entries, bytes)
			if err != nil {
				return nil, err
			}
			result[index] = cloned
		}
		return result, nil
	case []string:
		converted := make([]any, len(value))
		for index, item := range value {
			converted[index] = item
		}
		return validateAndCloneValue(converted, depth, limits, entries, bytes)
	default:
		kind := reflect.TypeOf(value)
		return nil, fmt.Errorf("%w: unsupported value type %v", ErrInvalidOptions, kind)
	}
}

func mergeMaps(values ...map[string]any) map[string]any {
	result := map[string]any{}
	for _, value := range values {
		for key, item := range value {
			result[key] = cloneUnchecked(item)
		}
	}
	return result
}

func applyRuntimeProfile(chat damodel.Chat, overrides map[string]any) (damodel.Chat, error) {
	if len(overrides) == 0 {
		return chat, nil
	}
	profile, err := decodeRuntimeProfile(chat.Profile(), overrides)
	if err != nil {
		return nil, err
	}
	return damodel.WithProfile(chat, func(target *damodel.Profile) { *target = profile }), nil
}

func validateRuntimeProfileOverrides(overrides map[string]any) error {
	_, err := decodeRuntimeProfile(damodel.Profile{}, overrides)
	return err
}

func decodeRuntimeProfile(base damodel.Profile, overrides map[string]any) (damodel.Profile, error) {
	normalized := cloneMap(overrides)
	for alias, canonical := range map[string]string{"max_input_tokens": "context_window", "max_tokens": "max_output_tokens"} {
		if value, exists := normalized[alias]; exists {
			if _, duplicate := normalized[canonical]; duplicate {
				return damodel.Profile{}, fmt.Errorf("%w: profile contains both %s and %s", ErrInvalidOptions, alias, canonical)
			}
			normalized[canonical] = value
			delete(normalized, alias)
		}
	}
	if _, exists := normalized["provider"]; exists {
		return damodel.Profile{}, fmt.Errorf("%w: profile provider identity cannot be overridden", ErrInvalidOptions)
	}
	if _, exists := normalized["model"]; exists {
		return damodel.Profile{}, fmt.Errorf("%w: profile model identity cannot be overridden", ErrInvalidOptions)
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return damodel.Profile{}, fmt.Errorf("%w: encode profile override", ErrInvalidOptions)
	}
	profile := base
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return damodel.Profile{}, fmt.Errorf("%w: profile override: %v", ErrInvalidOptions, err)
	}
	if profile.ContextWindow < 0 || profile.MaxOutputTokens < 0 || profile.MaxImageBytes < 0 || profile.MaxImageDimension < 0 {
		return damodel.Profile{}, fmt.Errorf("%w: profile limits must not be negative", ErrInvalidOptions)
	}
	return profile, nil
}

// ApplyProfile validates and applies runtime-profile overrides to an already
// constructed model. It is useful for caller-owned authentication flows that
// cannot pass through Resolver's credential step.
func ApplyProfile(chat damodel.Chat, overrides map[string]any) (damodel.Chat, error) {
	if chat == nil {
		panic("modelconfig: model is required")
	}
	validated, err := validateAndCloneMap(overrides, valueLimits{maxEntries: 256, maxDepth: 8, maxBytes: 64 << 10})
	if err != nil {
		return nil, err
	}
	return applyRuntimeProfile(chat, validated)
}

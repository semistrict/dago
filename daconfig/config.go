// Package daconfig provides provider-neutral layered configuration contracts.
package daconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// CLIPrefix identifies the highest-precedence generic CLI environment overlay.
	CLIPrefix = "DEEPAGENTS_CLI_"
	// CodePrefix identifies the code-application environment overlay.
	CodePrefix = "DEEPAGENTS_CODE_"
)

// ErrInvalidConfig identifies invalid external configuration data.
var ErrInvalidConfig = errors.New("invalid configuration")

// Kind is the canonical scalar type of an option.
type Kind string

const (
	// KindString declares a string option.
	KindString Kind = "string"
	// KindBool declares a boolean option.
	KindBool Kind = "bool"
	// KindInt declares a bounded integer option.
	KindInt Kind = "int"
)

// Option declares one canonical configuration key.
type Option struct {
	Key         string `json:"key"`
	Group       string `json:"group"`
	Summary     string `json:"summary"`
	Kind        Kind   `json:"kind"`
	Environment string `json:"environment,omitempty"`
	Default     any    `json:"default,omitempty"`
	Redacted    bool   `json:"redacted,omitempty"`
	Persist     bool   `json:"persist,omitempty"`
	Minimum     int64  `json:"minimum,omitempty"`
	Maximum     int64  `json:"maximum,omitempty"`
}

// Manifest is an immutable, validated option catalog.
type Manifest struct {
	options []Option
	byKey   map[string]Option
}

var (
	keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	envPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

// NewManifest compiles a canonical option catalog. Invalid static declarations
// panic; callers receive an immutable manifest.
func NewManifest(options ...Option) *Manifest {
	manifest := &Manifest{options: make([]Option, len(options)), byKey: make(map[string]Option, len(options))}
	environments := make(map[string]string, len(options))
	for index, option := range options {
		if !keyPattern.MatchString(option.Key) || strings.TrimSpace(option.Group) == "" || strings.TrimSpace(option.Summary) == "" {
			panic(fmt.Sprintf("daconfig: invalid option declaration %q", option.Key))
		}
		if _, exists := manifest.byKey[option.Key]; exists {
			panic(fmt.Sprintf("daconfig: duplicate option %q", option.Key))
		}
		if option.Environment != "" {
			if !envPattern.MatchString(option.Environment) {
				panic(fmt.Sprintf("daconfig: invalid canonical environment name for %q", option.Key))
			}
			if previous, exists := environments[option.Environment]; exists {
				panic(fmt.Sprintf("daconfig: environment %q belongs to both %q and %q", option.Environment, previous, option.Key))
			}
			environments[option.Environment] = option.Key
		}
		if option.Maximum != 0 && option.Minimum > option.Maximum {
			panic(fmt.Sprintf("daconfig: invalid integer bounds for %q", option.Key))
		}
		defaultValue, err := coerce(option, option.Default)
		if err != nil {
			panic(fmt.Sprintf("daconfig: invalid default for %q: %v", option.Key, err))
		}
		option.Default = defaultValue
		manifest.options[index] = cloneOption(option)
		manifest.byKey[option.Key] = cloneOption(option)
	}
	return manifest
}

// Options returns defensive copies in declaration order.
func (manifest *Manifest) Options() []Option {
	if manifest == nil {
		return nil
	}
	result := make([]Option, len(manifest.options))
	for index, option := range manifest.options {
		result[index] = cloneOption(option)
	}
	return result
}

// Option looks up a canonical key case-insensitively.
func (manifest *Manifest) Option(key string) (Option, bool) {
	if manifest == nil {
		return Option{}, false
	}
	option, ok := manifest.byKey[strings.ToLower(strings.TrimSpace(key))]
	return cloneOption(option), ok
}

// LookupEnv is a caller-supplied environment lookup.
type LookupEnv func(string) (string, bool)

// ResolverOptions bounds and orders environment and layer resolution.
type ResolverOptions struct {
	Prefixes       []string
	MaxValueBytes  int
	MaxLayerValues int
}

// DefaultResolverOptions returns finite production defaults.
func DefaultResolverOptions() ResolverOptions {
	return ResolverOptions{Prefixes: []string{CodePrefix, CLIPrefix}, MaxValueBytes: 64 << 10, MaxLayerValues: 256}
}

// Resolver combines file, environment, and explicit layers deterministically.
type Resolver struct {
	manifest *Manifest
	lookup   LookupEnv
	options  ResolverOptions
}

// NewResolver constructs a layered resolver. The manifest and environment
// lookup are required positional dependencies; construction performs no I/O.
func NewResolver(manifest *Manifest, lookup LookupEnv, options ResolverOptions) *Resolver {
	if manifest == nil {
		panic("daconfig: manifest is nil")
	}
	if lookup == nil {
		panic("daconfig: environment lookup is nil")
	}
	defaults := DefaultResolverOptions()
	if options.Prefixes == nil {
		options.Prefixes = defaults.Prefixes
	}
	if options.MaxValueBytes == 0 {
		options.MaxValueBytes = defaults.MaxValueBytes
	}
	if options.MaxLayerValues == 0 {
		options.MaxLayerValues = defaults.MaxLayerValues
	}
	if options.MaxValueBytes < 1 || options.MaxValueBytes > 1<<20 || options.MaxLayerValues < 1 || options.MaxLayerValues > 4096 {
		panic("daconfig: resolver bounds are outside their finite range")
	}
	seen := map[string]struct{}{}
	for _, prefix := range options.Prefixes {
		if prefix == "" || len(prefix) > 128 || !envPattern.MatchString(strings.TrimSuffix(prefix, "_")) || !strings.HasSuffix(prefix, "_") {
			panic("daconfig: invalid environment prefix")
		}
		if _, duplicate := seen[prefix]; duplicate {
			panic("daconfig: duplicate environment prefix")
		}
		seen[prefix] = struct{}{}
	}
	options.Prefixes = append([]string(nil), options.Prefixes...)
	return &Resolver{manifest: manifest, lookup: lookup, options: options}
}

// Layer is an immutable named set of canonical option values.
type Layer struct {
	Name   string
	values map[string]any
}

// NewLayer copies a required, bounded named layer.
func NewLayer(name string, values map[string]any) Layer {
	if strings.TrimSpace(name) == "" || len(name) > 256 {
		panic("daconfig: layer name is required and bounded")
	}
	return Layer{Name: name, values: cloneValues(values)}
}

// Values returns a defensive copy of this layer's values.
func (layer Layer) Values() map[string]any { return cloneValues(layer.values) }

// Entry is one effective, source-attributed introspection record.
type Entry struct {
	Key      string `json:"key"`
	Group    string `json:"group"`
	Summary  string `json:"summary,omitempty"`
	Kind     Kind   `json:"kind"`
	Source   string `json:"source"`
	Set      bool   `json:"set"`
	Redacted bool   `json:"redacted,omitempty"`
	Value    any    `json:"value,omitempty"`
}

// Snapshot is an immutable effective configuration view.
type Snapshot struct {
	entries map[string]Entry
	order   []string
}

// Resolve applies defaults, supplied layers in order, canonical environment
// variables, configured prefixes in order, then overrides. Later values win.
func (resolver *Resolver) Resolve(layers []Layer, overrides Layer) (Snapshot, error) {
	entries := make(map[string]Entry, len(resolver.manifest.options))
	for _, option := range resolver.manifest.options {
		entries[option.Key] = Entry{Key: option.Key, Group: option.Group, Summary: option.Summary, Kind: option.Kind, Source: "default", Value: cloneValue(option.Default), Redacted: option.Redacted}
	}
	for _, layer := range layers {
		if err := resolver.applyLayer(entries, layer); err != nil {
			return Snapshot{}, err
		}
	}
	for _, option := range resolver.manifest.options {
		if option.Environment == "" {
			continue
		}
		if raw, ok := resolver.lookup(option.Environment); ok {
			resolver.applyEnvironment(entries, option, raw, "env:"+option.Environment)
		}
		alreadyPrefixed := false
		for _, prefix := range resolver.options.Prefixes {
			alreadyPrefixed = alreadyPrefixed || strings.HasPrefix(option.Environment, prefix)
		}
		if alreadyPrefixed {
			continue
		}
		for _, prefix := range resolver.options.Prefixes {
			name := prefix + option.Environment
			if raw, ok := resolver.lookup(name); ok {
				resolver.applyEnvironment(entries, option, raw, "env:"+name)
			}
		}
	}
	if overrides.Name != "" || len(overrides.values) != 0 {
		if err := resolver.applyLayer(entries, overrides); err != nil {
			return Snapshot{}, err
		}
	}
	order := make([]string, len(resolver.manifest.options))
	for index, option := range resolver.manifest.options {
		order[index] = option.Key
	}
	return Snapshot{entries: entries, order: order}, nil
}

func (resolver *Resolver) applyEnvironment(entries map[string]Entry, option Option, raw string, source string) {
	if len(raw) > resolver.options.MaxValueBytes {
		return
	}
	value, err := coerce(option, raw)
	if err != nil {
		return
	}
	entry := entries[option.Key]
	entry.Source, entry.Set, entry.Value = source, true, cloneValue(value)
	entries[option.Key] = entry
}

func (resolver *Resolver) applyLayer(entries map[string]Entry, layer Layer) error {
	if len(layer.values) > resolver.options.MaxLayerValues {
		return fmt.Errorf("%w: layer %q exceeds %d values", ErrInvalidConfig, layer.Name, resolver.options.MaxLayerValues)
	}
	keys := make([]string, 0, len(layer.values))
	for key := range layer.values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		option, ok := resolver.manifest.byKey[key]
		if !ok {
			return fmt.Errorf("%w: layer %q contains unknown option %q", ErrInvalidConfig, layer.Name, key)
		}
		if err := resolver.apply(entries, option, layer.values[key], layer.Name); err != nil {
			return err
		}
	}
	return nil
}

func (resolver *Resolver) apply(entries map[string]Entry, option Option, raw any, source string) error {
	if encoded, err := json.Marshal(raw); err != nil || len(encoded) > resolver.options.MaxValueBytes {
		return fmt.Errorf("%w: %s value for %q exceeds its bound", ErrInvalidConfig, source, option.Key)
	}
	value, err := coerce(option, raw)
	if err != nil {
		return fmt.Errorf("%w: %s value for %q: %v", ErrInvalidConfig, source, option.Key, err)
	}
	entry := entries[option.Key]
	entry.Source, entry.Set, entry.Value = source, true, cloneValue(value)
	entries[option.Key] = entry
	return nil
}

// Entries returns redacted defensive copies in manifest order.
func (snapshot Snapshot) Entries() []Entry {
	result := make([]Entry, 0, len(snapshot.order))
	for _, key := range snapshot.order {
		entry := snapshot.entries[key]
		if entry.Redacted {
			entry.Value = nil
		} else {
			entry.Value = cloneValue(entry.Value)
		}
		result = append(result, entry)
	}
	return result
}

// Select returns redacted introspection entries for a canonical key or group.
func (snapshot Snapshot) Select(key string) []Entry {
	key = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(key, ".")))
	if entry, ok := snapshot.entries[key]; ok {
		if entry.Redacted {
			entry.Value = nil
		} else {
			entry.Value = cloneValue(entry.Value)
		}
		return []Entry{entry}
	}
	prefix := key + "."
	result := []Entry{}
	for _, optionKey := range snapshot.order {
		if strings.HasPrefix(optionKey, prefix) {
			entry := snapshot.entries[optionKey]
			if entry.Redacted {
				entry.Value = nil
			} else {
				entry.Value = cloneValue(entry.Value)
			}
			result = append(result, entry)
		}
	}
	return result
}

// String returns a string value or the useful empty-string zero value.
func (snapshot Snapshot) String(key string) string {
	entry, ok := snapshot.entries[key]
	if !ok {
		return ""
	}
	value, _ := entry.Value.(string)
	return value
}

// Bool returns a boolean value or the useful false zero value.
func (snapshot Snapshot) Bool(key string) bool {
	entry, ok := snapshot.entries[key]
	if !ok {
		return false
	}
	value, _ := entry.Value.(bool)
	return value
}

// Int returns an integer value or the useful zero value.
func (snapshot Snapshot) Int(key string) int {
	entry, ok := snapshot.entries[key]
	if !ok {
		return 0
	}
	value, _ := entry.Value.(int)
	return value
}

func coerce(option Option, raw any) (any, error) {
	if raw == nil {
		switch option.Kind {
		case KindString:
			return "", nil
		case KindBool:
			return false, nil
		case KindInt:
			return 0, validateInteger(option, 0)
		default:
			return nil, fmt.Errorf("unsupported kind %q", option.Kind)
		}
	}
	switch option.Kind {
	case KindString:
		value, ok := raw.(string)
		if !ok || strings.ContainsRune(value, 0) {
			return nil, errors.New("must be a string without NUL bytes")
		}
		return value, nil
	case KindBool:
		if value, ok := raw.(bool); ok {
			return value, nil
		}
		value, ok := raw.(string)
		if !ok {
			return nil, errors.New("must be a boolean")
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on":
			return true, nil
		case "", "0", "false", "no", "off":
			return false, nil
		default:
			return nil, errors.New("must be true/false, yes/no, on/off, or 1/0")
		}
	case KindInt:
		var value int64
		switch typed := raw.(type) {
		case int:
			value = int64(typed)
		case int64:
			value = typed
		case float64:
			if typed != float64(int64(typed)) {
				return nil, errors.New("must be an integer")
			}
			value = int64(typed)
		case json.Number:
			parsed, err := typed.Int64()
			if err != nil {
				return nil, errors.New("must be an integer")
			}
			value = parsed
		case string:
			parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
			if err != nil {
				return nil, errors.New("must be an integer")
			}
			value = parsed
		default:
			return nil, errors.New("must be an integer")
		}
		if err := validateInteger(option, value); err != nil {
			return nil, err
		}
		if int64(int(value)) != value {
			return nil, errors.New("is outside the platform integer range")
		}
		return int(value), nil
	default:
		return nil, fmt.Errorf("unsupported kind %q", option.Kind)
	}
}

func validateInteger(option Option, value int64) error {
	if value < option.Minimum || option.Maximum != 0 && value > option.Maximum {
		return fmt.Errorf("must be between %d and %d", option.Minimum, option.Maximum)
	}
	return nil
}

func cloneOption(option Option) Option { option.Default = cloneValue(option.Default); return option }

func cloneValues(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = cloneValue(value)
	}
	return result
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = cloneValue(typed[index])
		}
		return result
	case map[string]any:
		return cloneValues(typed)
	case json.RawMessage:
		return append(json.RawMessage(nil), typed...)
	default:
		return value
	}
}

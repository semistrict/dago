package dago

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// HarnessProfileConfig is the JSON/YAML-safe subset of a runtime Profile.
// Pointer fields preserve the distinction between unset and explicitly empty.
type HarnessProfileConfig struct {
	BaseSystemPrompt         *string
	SystemPromptSuffix       *string
	ToolDescriptionOverrides map[string]string
	ExcludedTools            []string
	ExcludedMiddleware       []string
	GeneralPurposeSubagent   *GeneralPurposeSubagentProfile
}

var harnessProfileConfigKeys = map[string]bool{
	"base_system_prompt": true, "system_prompt_suffix": true,
	"tool_description_overrides": true, "excluded_tools": true,
	"excluded_middleware": true, "general_purpose_subagent": true,
}

var generalPurposeProfileKeys = map[string]bool{
	"enabled": true, "description": true, "system_prompt": true,
}

func (config HarnessProfileConfig) Validate() error {
	for name, description := range config.ToolDescriptionOverrides {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(description) == "" {
			return fmt.Errorf("tool_description_overrides requires non-empty string names and descriptions")
		}
	}
	for _, name := range config.ExcludedTools {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
			return fmt.Errorf("excluded_tools contains an empty or whitespace-padded name")
		}
	}
	for _, name := range config.ExcludedMiddleware {
		if err := validateProfileMiddlewareExclusion(name); err != nil {
			return err
		}
		if isRequiredMiddlewareExclusion(name) {
			return fmt.Errorf("excluded_middleware cannot remove required scaffolding %q", name)
		}
	}
	return nil
}

func validateProfileMiddlewareExclusion(name string) error {
	if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
		return fmt.Errorf("excluded_middleware contains an empty or whitespace-padded name")
	}
	if strings.HasPrefix(name, "_") {
		return fmt.Errorf("excluded_middleware name %q cannot start with an underscore", name)
	}
	if strings.Contains(name, ":") {
		return fmt.Errorf("excluded_middleware name %q cannot be a class path", name)
	}
	return nil
}

// ToMap returns deterministic plain data suitable for safe JSON or YAML.
func (config HarnessProfileConfig) ToMap() (map[string]any, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	result := map[string]any{}
	if config.BaseSystemPrompt != nil {
		result["base_system_prompt"] = *config.BaseSystemPrompt
	}
	if config.SystemPromptSuffix != nil {
		result["system_prompt_suffix"] = *config.SystemPromptSuffix
	}
	if len(config.ToolDescriptionOverrides) > 0 {
		values := make(map[string]string, len(config.ToolDescriptionOverrides))
		for name, description := range config.ToolDescriptionOverrides {
			values[name] = description
		}
		result["tool_description_overrides"] = values
	}
	if len(config.ExcludedTools) > 0 {
		result["excluded_tools"] = sortedUnique(config.ExcludedTools)
	}
	if len(config.ExcludedMiddleware) > 0 {
		result["excluded_middleware"] = sortedUnique(config.ExcludedMiddleware)
	}
	if config.GeneralPurposeSubagent != nil {
		result["general_purpose_subagent"] = generalPurposeProfileMap(*config.GeneralPurposeSubagent)
	}
	return result, nil
}

func HarnessProfileConfigFromMap(data map[string]any) (HarnessProfileConfig, error) {
	for key := range data {
		if !harnessProfileConfigKeys[key] {
			return HarnessProfileConfig{}, fmt.Errorf("unknown harness profile config key %q", key)
		}
	}
	var result HarnessProfileConfig
	var err error
	if value, exists := data["base_system_prompt"]; exists {
		result.BaseSystemPrompt, err = optionalConfigString(value, "base_system_prompt")
		if err != nil {
			return HarnessProfileConfig{}, err
		}
	}
	if value, exists := data["system_prompt_suffix"]; exists {
		result.SystemPromptSuffix, err = optionalConfigString(value, "system_prompt_suffix")
		if err != nil {
			return HarnessProfileConfig{}, err
		}
	}
	if value, exists := data["tool_description_overrides"]; exists && value != nil {
		result.ToolDescriptionOverrides, err = configStringMap(value, "tool_description_overrides")
		if err != nil {
			return HarnessProfileConfig{}, err
		}
	}
	if value, exists := data["excluded_tools"]; exists && value != nil {
		result.ExcludedTools, err = configStringSlice(value, "excluded_tools")
		if err != nil {
			return HarnessProfileConfig{}, err
		}
	}
	if value, exists := data["excluded_middleware"]; exists && value != nil {
		result.ExcludedMiddleware, err = configStringSlice(value, "excluded_middleware")
		if err != nil {
			return HarnessProfileConfig{}, err
		}
	}
	if value, exists := data["general_purpose_subagent"]; exists && value != nil {
		result.GeneralPurposeSubagent, err = generalPurposeProfileFromValue(value)
		if err != nil {
			return HarnessProfileConfig{}, err
		}
	}
	if err := result.Validate(); err != nil {
		return HarnessProfileConfig{}, err
	}
	return result, nil
}

func (config HarnessProfileConfig) ToProfile() (Profile, error) {
	if err := config.Validate(); err != nil {
		return Profile{}, err
	}
	return Profile{
		Kind: ProfileHarness, BaseSystemPrompt: configCloneStringPointer(config.BaseSystemPrompt),
		SystemPromptSuffix: configCloneStringPointer(config.SystemPromptSuffix),
		ToolDescriptions:   configCloneStringMap(config.ToolDescriptionOverrides),
		ExcludeTools:       sortedUnique(config.ExcludedTools),
		ExcludeMiddleware:  sortedUnique(config.ExcludedMiddleware),
		GeneralPurpose:     configCloneGeneralPurposeProfile(config.GeneralPurposeSubagent),
	}, nil
}

func HarnessProfileConfigFromProfile(profile Profile) (HarnessProfileConfig, error) {
	if profile.Kind != "" && profile.Kind != ProfileHarness {
		return HarnessProfileConfig{}, fmt.Errorf("profile %q is not a harness profile", profile.Name)
	}
	if len(profile.Middleware) > 0 || profile.SystemPrompt != "" {
		return HarnessProfileConfig{}, fmt.Errorf("runtime middleware and additive system prompts cannot be represented in a harness profile config")
	}
	result := HarnessProfileConfig{
		BaseSystemPrompt:         configCloneStringPointer(profile.BaseSystemPrompt),
		SystemPromptSuffix:       configCloneStringPointer(profile.SystemPromptSuffix),
		ToolDescriptionOverrides: configCloneStringMap(profile.ToolDescriptions),
		ExcludedTools:            sortedUnique(profile.ExcludeTools),
		ExcludedMiddleware:       sortedUnique(profile.ExcludeMiddleware),
		GeneralPurposeSubagent:   configCloneGeneralPurposeProfile(profile.GeneralPurpose),
	}
	if err := result.Validate(); err != nil {
		return HarnessProfileConfig{}, err
	}
	return result, nil
}

func RegisterHarnessProfileConfig(name string, config HarnessProfileConfig) error {
	profile, err := config.ToProfile()
	if err != nil {
		return err
	}
	profile.Name = name
	return RegisterProfile(profile)
}

func (config HarnessProfileConfig) MarshalJSON() ([]byte, error) {
	value, err := config.ToMap()
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func (config *HarnessProfileConfig) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	parsed, err := HarnessProfileConfigFromMap(value)
	if err != nil {
		return err
	}
	*config = parsed
	return nil
}

func (config HarnessProfileConfig) MarshalYAML() (any, error) {
	return config.ToMap()
}

func (config *HarnessProfileConfig) UnmarshalYAML(node *yaml.Node) error {
	var value map[string]any
	if err := node.Decode(&value); err != nil {
		return err
	}
	parsed, err := HarnessProfileConfigFromMap(value)
	if err != nil {
		return err
	}
	*config = parsed
	return nil
}

func generalPurposeProfileMap(profile GeneralPurposeSubagentProfile) map[string]any {
	result := map[string]any{}
	if profile.Enabled != nil {
		result["enabled"] = *profile.Enabled
	}
	if profile.Description != nil {
		result["description"] = *profile.Description
	}
	if profile.SystemPrompt != nil {
		result["system_prompt"] = *profile.SystemPrompt
	}
	return result
}

func generalPurposeProfileFromValue(value any) (*GeneralPurposeSubagentProfile, error) {
	data, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("general_purpose_subagent must be an object, got %T", value)
	}
	for key := range data {
		if !generalPurposeProfileKeys[key] {
			return nil, fmt.Errorf("unknown general_purpose_subagent key %q", key)
		}
	}
	result := &GeneralPurposeSubagentProfile{}
	var err error
	if value, exists := data["enabled"]; exists && value != nil {
		enabled, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("general_purpose_subagent.enabled must be a boolean, got %T", value)
		}
		result.Enabled = &enabled
	}
	if value, exists := data["description"]; exists {
		result.Description, err = optionalConfigString(value, "general_purpose_subagent.description")
		if err != nil {
			return nil, err
		}
	}
	if value, exists := data["system_prompt"]; exists {
		result.SystemPrompt, err = optionalConfigString(value, "general_purpose_subagent.system_prompt")
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func optionalConfigString(value any, field string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be a string or null, got %T", field, value)
	}
	return &text, nil
}

func configStringMap(value any, field string) (map[string]string, error) {
	result := map[string]string{}
	switch values := value.(type) {
	case map[string]string:
		for key, item := range values {
			result[key] = item
		}
	case map[string]any:
		for key, raw := range values {
			item, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("%s.%s must be a string, got %T", field, key, raw)
			}
			result[key] = item
		}
	default:
		return nil, fmt.Errorf("%s must be an object, got %T", field, value)
	}
	return result, nil
}

func configStringSlice(value any, field string) ([]string, error) {
	var result []string
	switch values := value.(type) {
	case []string:
		result = append(result, values...)
	case []any:
		for _, raw := range values {
			item, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("%s entries must be strings, got %T", field, raw)
			}
			result = append(result, item)
		}
	default:
		return nil, fmt.Errorf("%s must be a list of strings, got %T", field, value)
	}
	return sortedUnique(result), nil
}

func sortedUnique(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func configCloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func configCloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func configCloneGeneralPurposeProfile(value *GeneralPurposeSubagentProfile) *GeneralPurposeSubagentProfile {
	if value == nil {
		return nil
	}
	return &GeneralPurposeSubagentProfile{
		Enabled: configCloneBoolPointer(value.Enabled), Description: configCloneStringPointer(value.Description),
		SystemPrompt: configCloneStringPointer(value.SystemPrompt),
	}
}

func configCloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

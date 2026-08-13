package dago

import (
	"context"
	"fmt"
	"strings"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
)

// Profile is a composable construction overlay. Later profiles win for scalar
// and tool-description values; slices append in declaration order.
type Profile struct {
	Name               string
	BaseSystemPrompt   *string
	SystemPromptSuffix *string
	SystemPrompt       string
	ToolDescriptions   map[string]string
	ExcludeTools       []string
	Middleware         []dagent.Middleware
	ExcludeMiddleware  []string
	GeneralPurpose     *GeneralPurposeSubagentProfile
}

type GeneralPurposeSubagentMode string

const (
	GeneralPurposeSubagentEnabled  GeneralPurposeSubagentMode = "enabled"
	GeneralPurposeSubagentDisabled GeneralPurposeSubagentMode = "disabled"
)

// GeneralPurposeSubagentProfile controls the automatically added worker. An
// empty Mode inherits; otherwise it explicitly enables or disables the worker.
type GeneralPurposeSubagentProfile struct {
	Mode         GeneralPurposeSubagentMode
	Description  *string
	SystemPrompt *string
}

func validateProfileKey(name string) error {
	if name == "" {
		return fmt.Errorf("profile name is required")
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("profile name %q has leading or trailing whitespace", name)
	}
	if strings.Count(name, ":") > 1 {
		return fmt.Errorf("profile name %q must use provider or provider:model form", name)
	}
	if provider, model, found := strings.Cut(name, ":"); found && (provider == "" || model == "" || provider != strings.TrimSpace(provider) || model != strings.TrimSpace(model)) {
		return fmt.Errorf("profile name %q must use provider:model without empty or whitespace-padded parts", name)
	}
	return nil
}

func MergeProfiles(profiles ...Profile) Profile {
	result := Profile{ToolDescriptions: map[string]string{}}
	for _, profile := range profiles {
		if profile.Name != "" {
			result.Name = profile.Name
		}
		if profile.BaseSystemPrompt != nil {
			value := *profile.BaseSystemPrompt
			result.BaseSystemPrompt = &value
		}
		if profile.SystemPromptSuffix != nil {
			value := *profile.SystemPromptSuffix
			result.SystemPromptSuffix = &value
		}
		if profile.SystemPrompt != "" {
			if result.SystemPrompt != "" {
				result.SystemPrompt += "\n\n"
			}
			result.SystemPrompt += profile.SystemPrompt
		}
		for name, description := range profile.ToolDescriptions {
			result.ToolDescriptions[name] = description
		}
		result.ExcludeTools = appendUnique(result.ExcludeTools, profile.ExcludeTools...)
		result.Middleware = mergeMiddlewareByName(result.Middleware, profile.Middleware)
		result.ExcludeMiddleware = appendUnique(result.ExcludeMiddleware, profile.ExcludeMiddleware...)
		result.GeneralPurpose = mergeGeneralPurposeProfiles(result.GeneralPurpose, profile.GeneralPurpose)
	}
	return result
}

func resolveProfiles(chat damodel.Chat, explicit []Profile) (Profile, error) {
	values := make([]Profile, 0, len(explicit)+2)
	modelProfile := chat.Profile()
	if builtin, exists := builtinHarnessProfile(modelProfile.Provider, modelProfile.Model); exists {
		values = append(values, builtin)
	}
	for index, profile := range explicit {
		if err := validateProfile(profile); err != nil {
			return Profile{}, fmt.Errorf("profile %d: %w", index, err)
		}
	}
	values = append(values, explicit...)
	return MergeProfiles(values...), nil
}

func cloneProfile(profile Profile) Profile {
	copy := profile
	if profile.BaseSystemPrompt != nil {
		value := *profile.BaseSystemPrompt
		copy.BaseSystemPrompt = &value
	}
	if profile.SystemPromptSuffix != nil {
		value := *profile.SystemPromptSuffix
		copy.SystemPromptSuffix = &value
	}
	copy.ToolDescriptions = make(map[string]string, len(profile.ToolDescriptions))
	for name, description := range profile.ToolDescriptions {
		copy.ToolDescriptions[name] = description
	}
	copy.ExcludeTools = append([]string(nil), profile.ExcludeTools...)
	copy.Middleware = append([]dagent.Middleware(nil), profile.Middleware...)
	copy.ExcludeMiddleware = append([]string(nil), profile.ExcludeMiddleware...)
	if profile.GeneralPurpose != nil {
		value := *profile.GeneralPurpose
		if value.Description != nil {
			description := *value.Description
			value.Description = &description
		}
		if value.SystemPrompt != nil {
			prompt := *value.SystemPrompt
			value.SystemPrompt = &prompt
		}
		copy.GeneralPurpose = &value
	}
	return copy
}

func validateProfile(profile Profile) error {
	if profile.Name != "" {
		if err := validateProfileKey(profile.Name); err != nil {
			return err
		}
	}
	if profile.GeneralPurpose != nil && profile.GeneralPurpose.Mode != "" && profile.GeneralPurpose.Mode != GeneralPurposeSubagentEnabled && profile.GeneralPurpose.Mode != GeneralPurposeSubagentDisabled {
		return fmt.Errorf("profile %q has invalid general-purpose subagent mode %q", profile.Name, profile.GeneralPurpose.Mode)
	}
	for _, name := range profile.ExcludeMiddleware {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
			return fmt.Errorf("profile %q has invalid empty or whitespace-padded middleware exclusion", profile.Name)
		}
		if strings.HasPrefix(name, "_") {
			return fmt.Errorf("profile %q cannot exclude private middleware %q", profile.Name, name)
		}
		if strings.Contains(name, ":") {
			return fmt.Errorf("profile %q middleware exclusion %q must be a public name, not a class path", profile.Name, name)
		}
		if isRequiredMiddlewareExclusion(name) {
			return fmt.Errorf("profile %q cannot exclude required middleware %q", profile.Name, name)
		}
	}
	return nil
}

func isRequiredMiddlewareExclusion(name string) bool {
	switch name {
	case "filesystem", "subagents":
		return true
	default:
		return false
	}
}

func mergeMiddlewareByName(base, override []dagent.Middleware) []dagent.Middleware {
	result := append([]dagent.Middleware(nil), base...)
	positions := make(map[string]int, len(result))
	for index, item := range result {
		positions[item.Name] = index
	}
	for _, item := range override {
		if index, exists := positions[item.Name]; exists {
			if item.SerializedName == "" {
				item.SerializedName = result[index].SerializedName
			}
			result[index] = item
			continue
		}
		positions[item.Name] = len(result)
		result = append(result, item)
	}
	return result
}

func mergeGeneralPurposeProfiles(base, override *GeneralPurposeSubagentProfile) *GeneralPurposeSubagentProfile {
	if base == nil {
		if override == nil {
			return nil
		}
		copy := Profile{GeneralPurpose: override}
		return cloneProfile(copy).GeneralPurpose
	}
	if override == nil {
		copy := Profile{GeneralPurpose: base}
		return cloneProfile(copy).GeneralPurpose
	}
	result := *base
	if override.Mode != "" {
		result.Mode = override.Mode
	}
	if override.Description != nil {
		result.Description = override.Description
	}
	if override.SystemPrompt != nil {
		result.SystemPrompt = override.SystemPrompt
	}
	copy := Profile{GeneralPurpose: &result}
	return cloneProfile(copy).GeneralPurpose
}

func applyProfilePrompt(profile Profile, user, authoredBase string) string {
	base := authoredBase
	if profile.BaseSystemPrompt != nil {
		base = *profile.BaseSystemPrompt
	}
	if profile.SystemPrompt != "" {
		if base != "" {
			base += "\n\n"
		}
		base += profile.SystemPrompt
	}
	if profile.SystemPromptSuffix != nil {
		if base != "" {
			base += "\n\n" + *profile.SystemPromptSuffix
		} else {
			base = *profile.SystemPromptSuffix
		}
	}
	if user == "" {
		return base
	}
	if base == "" {
		return user
	}
	return user + "\n\n" + base
}

func appendUnique(values []string, additions ...string) []string {
	seen := make(map[string]bool, len(values)+len(additions))
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range additions {
		if value != "" && !seen[value] {
			values = append(values, value)
			seen[value] = true
		}
	}
	return values
}

type profileTool struct {
	datool.Tool
	description string
}

func (wrapped profileTool) Definition() datool.Definition {
	definition := wrapped.Tool.Definition()
	definition.Description = wrapped.description
	return definition
}

func applyToolProfile(values []datool.Tool, descriptions map[string]string, excluded map[string]bool) []datool.Tool {
	result := make([]datool.Tool, 0, len(values))
	for _, executable := range values {
		definition := executable.Definition()
		if excluded[definition.Name] {
			continue
		}
		if description := descriptions[definition.Name]; description != "" {
			executable = profileTool{Tool: executable, description: description}
		}
		result = append(result, executable)
	}
	return result
}

// ToolExclusion removes profile-excluded tools at the final model
// boundary, after custom middleware has had an opportunity to alter the request.
// It intentionally leaves the executor registry intact so historical or resumed
// tool calls retain the same behavior as the canonical middleware.
func ToolExclusion(names []string) dagent.Middleware {
	excluded := make(map[string]bool, len(names))
	for _, name := range names {
		excluded[name] = true
	}
	return dagent.Middleware{Name: "tool_exclusion", WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
		filtered := make([]datool.Tool, 0, len(request.Tools))
		for _, item := range request.Tools {
			if !excluded[item.Definition().Name] {
				filtered = append(filtered, item)
			}
		}
		request.Tools = filtered
		return next(ctx, request)
	}}
}

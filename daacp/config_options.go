package daacp

import (
	acp "github.com/coder/acp-go-sdk"
)

const modelConfigID acp.SessionConfigId = "model"

func cloneConfigOptions(options []acp.SessionConfigOption) []acp.SessionConfigOption {
	cloned := make([]acp.SessionConfigOption, len(options))
	for index, option := range options {
		cloned[index] = option
		if option.Select != nil {
			selectCopy := *option.Select
			selectCopy.Meta = cloneAnyMap(option.Select.Meta)
			if option.Select.Description != nil {
				description := *option.Select.Description
				selectCopy.Description = &description
			}
			selectCopy.Options = cloneSelectOptions(option.Select.Options)
			cloned[index].Select = &selectCopy
		}
		if option.Boolean != nil {
			booleanCopy := *option.Boolean
			booleanCopy.Meta = cloneAnyMap(option.Boolean.Meta)
			if option.Boolean.Description != nil {
				description := *option.Boolean.Description
				booleanCopy.Description = &description
			}
			cloned[index].Boolean = &booleanCopy
		}
	}
	return cloned
}

func cloneSelectOptions(options acp.SessionConfigSelectOptions) acp.SessionConfigSelectOptions {
	cloned := acp.SessionConfigSelectOptions{}
	if options.Ungrouped != nil {
		values := make(acp.SessionConfigSelectOptionsUngrouped, len(*options.Ungrouped))
		for index, value := range *options.Ungrouped {
			values[index] = cloneSelectOption(value)
		}
		cloned.Ungrouped = &values
	}
	if options.Grouped != nil {
		groups := make(acp.SessionConfigSelectOptionsGrouped, len(*options.Grouped))
		for index, group := range *options.Grouped {
			groups[index] = group
			groups[index].Meta = cloneAnyMap(group.Meta)
			groups[index].Options = make([]acp.SessionConfigSelectOption, len(group.Options))
			for valueIndex, value := range group.Options {
				groups[index].Options[valueIndex] = cloneSelectOption(value)
			}
		}
		cloned.Grouped = &groups
	}
	return cloned
}

func cloneSelectOption(option acp.SessionConfigSelectOption) acp.SessionConfigSelectOption {
	cloned := option
	cloned.Meta = cloneAnyMap(option.Meta)
	if option.Description != nil {
		description := *option.Description
		cloned.Description = &description
	}
	return cloned
}

func cloneAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func configOptionsWithModel(options []acp.SessionConfigOption, model string) []acp.SessionConfigOption {
	result := cloneConfigOptions(options)
	for index := range result {
		if result[index].Select != nil && result[index].Select.Id == modelConfigID && model != "" {
			result[index].Select.CurrentValue = acp.SessionConfigValueId(model)
		}
	}
	return result
}

func defaultModelSelection(options []acp.SessionConfigOption) string {
	for _, option := range options {
		if option.Select != nil && option.Select.Id == modelConfigID {
			return string(option.Select.CurrentValue)
		}
	}
	return ""
}

func selectOptionSupports(option acp.SessionConfigOptionSelect, value acp.SessionConfigValueId) bool {
	if option.Options.Ungrouped != nil {
		for _, candidate := range *option.Options.Ungrouped {
			if candidate.Value == value {
				return true
			}
		}
	}
	if option.Options.Grouped != nil {
		for _, group := range *option.Options.Grouped {
			for _, candidate := range group.Options {
				if candidate.Value == value {
					return true
				}
			}
		}
	}
	return false
}

func modelSelectionSupported(options []acp.SessionConfigOption, model string) bool {
	if model == "" {
		return false
	}
	for _, option := range options {
		if option.Select != nil && option.Select.Id == modelConfigID {
			return selectOptionSupports(*option.Select, acp.SessionConfigValueId(model)) || string(option.Select.CurrentValue) == model
		}
	}
	return false
}

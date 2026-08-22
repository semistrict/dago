package daacp

import (
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestModelListResponseUsesAdvertisedModelConfiguration(t *testing.T) {
	category := acp.SessionConfigOptionCategoryModel
	values := acp.SessionConfigSelectOptionsUngrouped{
		{Name: "Model One", Value: "provider:model-one"},
		{Name: "Model Two", Value: "provider:model-two"},
	}
	response := modelListResponse([]acp.SessionConfigOption{{Select: &acp.SessionConfigOptionSelect{
		Id: "model", Name: "Model", Category: &category, CurrentValue: "provider:model-two",
		Options: acp.SessionConfigSelectOptions{Ungrouped: &values},
	}}})
	if response.Version != 1 || response.DefaultModel != "provider:model-two" {
		t.Fatalf("response = %#v", response)
	}
	if len(response.Models) != 2 || response.Models[0].ID != "provider:model-one" || response.Models[0].Name != "Model One" {
		t.Fatalf("models = %#v", response.Models)
	}
}

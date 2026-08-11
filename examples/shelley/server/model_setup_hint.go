package server

import (
	"context"
	"fmt"
)

const modelSetupHintLocal = "add_model"

func unsupportedModelMessage(modelID string, modelList []ModelInfo) string {
	if len(modelList) == 0 {
		return "No AI models are configured. Add one in Shelley's model picker."
	}
	return fmt.Sprintf("Unsupported model: %s", modelID)
}

func modelSetupHintForModels(_ context.Context, modelList []ModelInfo) string {
	if len(modelList) == 0 {
		return modelSetupHintLocal
	}
	return ""
}

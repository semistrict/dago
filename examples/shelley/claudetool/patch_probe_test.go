package claudetool

import (
	"context"
	"encoding/json"

	dtool "github.com/semistrict/dago/tool"

	"shelley.exe.dev/llm"
)

type patchProbeResult struct {
	LLMContent []llm.Content
	Display    any
	Error      error
}

func runPatchProbe(patch *PatchTool, ctx context.Context, input json.RawMessage) patchProbeResult {
	result, err := patch.NativeTool().Execute(ctx, input, dtool.Runtime{})
	if err != nil {
		return patchProbeResult{Error: err}
	}
	var display PatchDisplayData
	if len(result.Artifact) > 0 {
		if err := json.Unmarshal(result.Artifact, &display); err != nil {
			return patchProbeResult{Error: err}
		}
	}
	contents := make([]llm.Content, 0, len(result.Content))
	for _, block := range result.Content {
		contents = append(contents, llm.Content{Type: llm.ContentTypeText, Text: block.Text})
	}
	return patchProbeResult{LLMContent: contents, Display: display}
}

package browse

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"

	dmessage "github.com/semistrict/dago/message"
	dtool "github.com/semistrict/dago/tool"

	"shelley.exe.dev/llm"
)

// nativeToolProbe keeps the original browser assertions readable while every
// invocation crosses Dago's Tool.Execute boundary.
type nativeToolProbe struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	executable  dtool.Tool
}

type nativeProbeResult struct {
	LLMContent []llm.Content
	Display    any
	Error      error
}

func probeNativeTool(executable dtool.Tool) *nativeToolProbe {
	definition := executable.Definition()
	return &nativeToolProbe{
		Name: definition.Name, Description: definition.Description,
		InputSchema: definition.InputSchema, executable: executable,
	}
}

func probeBrowserTools(browser *BrowseTools) []*nativeToolProbe {
	return []*nativeToolProbe{
		probeNativeTool(browser.NativeCombinedTool()),
		probeNativeTool(browser.NativeReadImageTool()),
	}
}

func (probe *nativeToolProbe) Run(ctx context.Context, arguments json.RawMessage) nativeProbeResult {
	result, err := probe.executable.Execute(ctx, arguments, dtool.Runtime{})
	if err != nil {
		return nativeProbeResult{Error: err}
	}
	contents := make([]llm.Content, 0, len(result.Content))
	for _, block := range result.Content {
		switch block.Type {
		case dmessage.BlockText:
			contents = append(contents, llm.Content{Type: llm.ContentTypeText, Text: block.Text})
		case dmessage.BlockImage:
			var width, height int
			_ = json.Unmarshal(block.Extra[browserImageWidthKey], &width)
			_ = json.Unmarshal(block.Extra[browserImageHeightKey], &height)
			contents = append(contents, llm.Content{
				Type: llm.ContentTypeText, MediaType: block.MIMEType,
				Data:         base64.StdEncoding.EncodeToString(block.Data),
				DisplayWidth: width, DisplayHeight: height,
			})
		}
	}
	var display any
	if len(result.Artifact) > 0 {
		if err := json.Unmarshal(result.Artifact, &display); err != nil {
			return nativeProbeResult{Error: err}
		}
		display = normalizeProbeJSON(display)
	}
	return nativeProbeResult{LLMContent: contents, Display: display}
}

func normalizeProbeJSON(value any) any {
	switch typed := value.(type) {
	case float64:
		if typed == math.Trunc(typed) {
			return int(typed)
		}
		return typed
	case []any:
		for index := range typed {
			typed[index] = normalizeProbeJSON(typed[index])
		}
		return typed
	case map[string]any:
		for key := range typed {
			typed[key] = normalizeProbeJSON(typed[key])
		}
		return typed
	default:
		return value
	}
}

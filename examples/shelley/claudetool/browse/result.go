package browse

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	dmessage "github.com/semistrict/dago/message"
	dtool "github.com/semistrict/dago/tool"

	"shelley.exe.dev/llm"
)

const (
	browserImageWidthKey  = "shelley.display_width"
	browserImageHeightKey = "shelley.display_height"
)

type browserExecution struct {
	Content []dmessage.ContentBlock
	Display any
}

func browserText(text string) browserExecution {
	return browserExecution{Content: []dmessage.ContentBlock{{Type: dmessage.BlockText, Text: text}}}
}

func browserImage(description string, data []byte, mimeType string, width, height int, display any) browserExecution {
	widthJSON, _ := json.Marshal(width)
	heightJSON, _ := json.Marshal(height)
	return browserExecution{
		Content: []dmessage.ContentBlock{
			{Type: dmessage.BlockText, Text: description},
			{
				Type: dmessage.BlockImage, Data: data, MIMEType: mimeType,
				Extra: map[string]json.RawMessage{browserImageWidthKey: widthJSON, browserImageHeightKey: heightJSON},
			},
		},
		Display: display,
	}
}

func (execution browserExecution) dagoResult() (dtool.Result, error) {
	var artifact json.RawMessage
	var err error
	if execution.Display != nil {
		artifact, err = json.Marshal(execution.Display)
		if err != nil {
			return dtool.Result{}, fmt.Errorf("encode browser display: %w", err)
		}
	}
	return dtool.Result{Content: execution.Content, Artifact: artifact}, nil
}

func (execution browserExecution) legacyResult() llm.ToolOut {
	content := make([]llm.Content, 0, len(execution.Content))
	for _, block := range execution.Content {
		switch block.Type {
		case dmessage.BlockText:
			content = append(content, llm.Content{Type: llm.ContentTypeText, Text: block.Text})
		case dmessage.BlockImage:
			var width, height int
			_ = json.Unmarshal(block.Extra[browserImageWidthKey], &width)
			_ = json.Unmarshal(block.Extra[browserImageHeightKey], &height)
			content = append(content, llm.Content{
				Type: llm.ContentTypeText, MediaType: block.MIMEType,
				Data:         base64.StdEncoding.EncodeToString(block.Data),
				DisplayWidth: width, DisplayHeight: height,
			})
		}
	}
	return llm.ToolOut{LLMContent: content, Display: execution.Display}
}

func legacyBrowserResult(execution browserExecution, err error) llm.ToolOut {
	if err != nil {
		return llm.ErrorToolOut(err)
	}
	return execution.legacyResult()
}

package browse

import (
	"encoding/json"
	"fmt"

	dmessage "github.com/semistrict/dago/message"
	dtool "github.com/semistrict/dago/tool"
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

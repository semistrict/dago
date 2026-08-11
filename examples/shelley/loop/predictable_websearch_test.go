package loop

import (
	"context"
	"strings"
	"testing"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
)

// TestPredictableWebSearchCitations verifies the "web search" predictable
// pattern reproduces the Anthropic server-side web-search shape: a
// server_tool_use block, a web_search_tool_result with sources, and a run of
// text blocks where cited quotes carry a Citations array. This is what the UI
// coalesces into flowing paragraphs with inline citation markers.
func TestPredictableWebSearchCitations(t *testing.T) {
	for _, trigger := range []string{"web search", "citations"} {
		t.Run(trigger, func(t *testing.T) {
			svc := NewPredictableService()
			resp, err := svc.Invoke(context.Background(), damodel.Request{Messages: []dmessage.Message{dmessage.Human(trigger)}})
			if err != nil {
				t.Fatalf("Do: %v", err)
			}

			var (
				serverToolUse int
				searchResults int
				textBlocks    int
				citedBlocks   int
				prose         strings.Builder
			)
			for _, c := range resp.Message.Content {
				switch c.Type {
				case dmessage.BlockServerTool:
					serverToolUse++
				case dmessage.BlockSearchResult:
					searchResults++
				case dmessage.BlockText:
					textBlocks++
					prose.WriteString(c.Text)
					if len(c.Citations) > 0 {
						citedBlocks++
						if len(c.Citations) == 0 {
							t.Fatalf("empty citation array on a cited block")
						}
						if c.Citations[0].URL == "" {
							t.Fatalf("unexpected citation shape: %v", c.Citations[0])
						}
					}
				}
			}

			if serverToolUse != 1 {
				t.Errorf("server_tool_use blocks = %d, want 1", serverToolUse)
			}
			if searchResults == 0 {
				t.Errorf("web search results = 0, want > 0")
			}
			if textBlocks < 5 {
				t.Errorf("text blocks = %d, want several (to exercise coalescing)", textBlocks)
			}
			if citedBlocks == 0 {
				t.Errorf("cited text blocks = 0, want > 0")
			}
			if !strings.Contains(prose.String(), "never lose work, so model switching pairs well") {
				t.Errorf("adjacent cited prose does not preserve the upstream sentence: %q", prose.String())
			}
		})
	}
}

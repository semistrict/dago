//go:build tinygo

package browserapp

import "testing"

func TestTinyGoAvailableToolsOmitInterpreter(t *testing.T) {
	for _, tool := range availableTools(true) {
		if tool["name"] == "js_eval" {
			t.Fatal("TinyGo browser tools include js_eval")
		}
	}
}

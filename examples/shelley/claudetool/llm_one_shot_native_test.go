package claudetool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/datool"
)

func TestLLMOneShotNativeToolUsesNativeModelContract(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prompt.txt"), []byte("summarize this"), 0o600); err != nil {
		t.Fatal(err)
	}
	chat := modeltest.NewPredictable(modeltest.PredictableOptions{DefaultResponse: "native one-shot result"})
	provider := &oneShotMockProvider{services: map[string]damodel.Chat{"native-model": chat}}
	executable := (&llmOneShotTool{
		LLMProvider: provider, ModelID: "native-model", WorkingDir: newMutableWorkingDir(root),
		AvailableModels: []AvailableModel{{ID: "native-model"}},
	}).nativeTool()
	result, err := executable.Execute(context.Background(), json.RawMessage(`{"prompt_files":"prompt.txt"}`), datool.Runtime{CallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || !strings.HasPrefix(result.Content[0].Text, "native one-shot result") || !strings.Contains(result.Content[0].Text, "model: native-model") {
		t.Fatalf("content = %#v", result.Content)
	}
	if len(result.OtherUsage) != 1 || result.OtherUsage[0].Purpose != "llm_one_shot" || result.OtherUsage[0].Model != "predictable-v1" {
		t.Fatalf("other usage = %#v", result.OtherUsage)
	}
}

package claudetool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dmodel "github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
	dtool "github.com/semistrict/dago/tool"
)

func TestLLMOneShotNativeToolUsesDagoModelContract(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prompt.txt"), []byte("summarize this"), 0o600); err != nil {
		t.Fatal(err)
	}
	chat := modeltest.NewPredictable(modeltest.PredictableOptions{DefaultResponse: "native one-shot result"})
	provider := &oneShotMockProvider{services: map[string]dmodel.Chat{"native-model": chat}}
	executable := (&LLMOneShotTool{
		LLMProvider: provider, ModelID: "native-model", WorkingDir: NewMutableWorkingDir(root),
		AvailableModels: []AvailableModel{{ID: "native-model"}},
	}).NativeTool()
	result, err := executable.Execute(context.Background(), json.RawMessage(`{"prompt_files":"prompt.txt"}`), dtool.Runtime{CallID: "call-1"})
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

package claudetool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	dmodel "github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
	dtool "github.com/semistrict/dago/tool"
)

type nativeKeywordProvider struct{ chat dmodel.Chat }

func (provider nativeKeywordProvider) GetChat(string) (dmodel.Chat, error) {
	return provider.chat, nil
}

func (nativeKeywordProvider) GetAvailableModels() []string { return []string{"native-keyword"} }

func TestKeywordNativeToolUsesDagoModelContract(t *testing.T) {
	root := t.TempDir()
	command := exec.Command("git", "init", "--quiet")
	command.Dir = root
	if err := command.Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "needle.txt"), []byte("native keyword needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chat := modeltest.NewPredictable(modeltest.PredictableOptions{DefaultResponse: "native relevance result"})
	provider := nativeKeywordProvider{chat: chat}
	executable := NewKeywordToolWithWorkingDir(provider, NewMutableWorkingDir(root)).NativeTool()
	result, err := executable.Execute(context.Background(), json.RawMessage(`{
		"query":"find the native keyword fixture",
		"search_terms":["native keyword needle"]
	}`), dtool.Runtime{CallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "native relevance result" {
		t.Fatalf("content = %#v", result.Content)
	}
	if len(result.OtherUsage) != 1 || result.OtherUsage[0].Purpose != "keyword_search" || result.OtherUsage[0].Model != "predictable-v1" {
		t.Fatalf("other usage = %#v", result.OtherUsage)
	}
}

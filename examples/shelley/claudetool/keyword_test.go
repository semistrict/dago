package claudetool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	dtool "github.com/semistrict/dago/tool"
)

func TestKeywordInputSearchTermsFlexible(t *testing.T) {
	tests := []struct {
		name string
		json string
		want []string
	}{
		{"array", `{"query":"q","search_terms":["a","b"]}`, []string{"a", "b"}},
		{"string", `{"query":"q","search_terms":"a"}`, []string{"a"}},
		{"empty array", `{"query":"q","search_terms":[]}`, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var in keywordInput
			if err := json.Unmarshal([]byte(tt.json), &in); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual([]string(in.SearchTerms), tt.want) {
				t.Errorf("got %#v, want %#v", in.SearchTerms, tt.want)
			}
		})
	}
}

// Mock LLM provider for testing
type mockLLMProvider struct{}

type mockService struct{}

func (m *mockService) Invoke(context.Context, dmodel.Request) (dmodel.Response, error) {
	return dmodel.Response{Message: dmessage.Assistant("test response")}, nil
}
func (m *mockService) Stream(context.Context, dmodel.Request) (dmodel.Stream, error) {
	return dmodel.EmptyStream{}, nil
}
func (m *mockService) Profile() dmodel.Profile {
	return dmodel.Profile{ContextWindow: 4096, SupportsImages: true}
}
func (m *mockLLMProvider) GetChat(string) (dmodel.Chat, error) {
	return &mockService{}, nil
}

func (m *mockLLMProvider) GetAvailableModels() []string {
	return []string{"test-model"}
}

func TestNewKeywordTool(t *testing.T) {
	provider := &mockLLMProvider{}
	tool := NewKeywordTool(provider)

	if tool == nil {
		t.Fatal("NewKeywordTool returned nil")
	}
}

func TestNewKeywordToolWithWorkingDir(t *testing.T) {
	provider := &mockLLMProvider{}
	wd := NewMutableWorkingDir("/test")
	tool := NewKeywordToolWithWorkingDir(provider, wd)

	if tool == nil {
		t.Fatal("NewKeywordToolWithWorkingDir returned nil")
	}

	if tool.workingDir != wd {
		t.Error("workingDir not set correctly")
	}
}

func TestKeywordTool_Tool(t *testing.T) {
	provider := &mockLLMProvider{}
	keywordTool := NewKeywordTool(provider)
	tool := keywordTool.NativeTool()

	if tool == nil {
		t.Fatal("Tool() returned nil")
	}

	if tool.Definition().Name != keywordName {
		t.Errorf("expected name %q, got %q", keywordName, tool.Definition().Name)
	}

	if tool.Definition().Description != keywordDescription {
		t.Errorf("expected description %q, got %q", keywordDescription, tool.Definition().Description)
	}

}

func TestFindRepoRoot(t *testing.T) {
	// Create a temp directory structure
	tmpDir := t.TempDir()

	// Test when not in a git repo (should fail)
	_, err := FindRepoRoot(tmpDir)
	if err == nil {
		t.Error("expected error when not in git repo")
	}

	// Initialize a git repo properly
	cmd := exec.Command("git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Skip("git not available, skipping test")
	}

	// Test when in a git repo (should succeed)
	root, err := FindRepoRoot(tmpDir)
	if err != nil {
		t.Errorf("unexpected error when in git repo: %v", err)
	}

	if root != tmpDir {
		t.Errorf("expected root %q, got %q", tmpDir, root)
	}
}

func TestKeywordRun(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed, skipping test")
	}

	// Create a temp directory with some files
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	content := "This is a test file with some content for keyword search testing."
	if err := os.WriteFile(testFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	provider := &mockLLMProvider{}
	wd := NewMutableWorkingDir(tmpDir)
	keywordTool := NewKeywordToolWithWorkingDir(provider, wd)

	// Test with valid input
	input := keywordInput{
		Query:       "what files exist in this project",
		SearchTerms: stringSlice{"test", "file"},
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := keywordTool.NativeTool().Execute(context.Background(), raw, dtool.Runtime{})

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if len(result.Content) == 0 {
		t.Error("expected LLM content")
	}
}

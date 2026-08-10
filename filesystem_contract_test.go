package dago

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	"github.com/semistrict/dago/message"
	memorystore "github.com/semistrict/dago/store"
	"github.com/semistrict/dago/tool"
)

func TestFilesystemToolSchemasDescribeEveryArgument(t *testing.T) {
	memory, err := backend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := FilesystemMiddleware(FilesystemOptions{Backend: memory})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"ls": {"path"}, "read_file": {"file_path", "offset", "limit"},
		"write_file": {"file_path", "content"},
		"edit_file":  {"file_path", "old_string", "new_string", "replace_all"},
		"delete":     {"file_path"}, "glob": {"pattern", "path"},
		"grep": {"pattern", "path", "glob", "output_mode", "max_count"},
	}
	for _, executable := range middleware.Tools {
		definition := executable.Definition()
		arguments, expected := want[definition.Name]
		if !expected {
			continue
		}
		var document struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(definition.InputSchema, &document); err != nil {
			t.Fatalf("%s schema: %v", definition.Name, err)
		}
		for _, name := range arguments {
			property, exists := document.Properties[name]
			if !exists {
				t.Errorf("%s is missing %s", definition.Name, name)
				continue
			}
			if property["type"] == nil && property["anyOf"] == nil && property["$ref"] == nil {
				t.Errorf("%s.%s has no type", definition.Name, name)
			}
			if strings.TrimSpace(schemaString(property["description"])) == "" {
				t.Errorf("%s.%s has no description", definition.Name, name)
			}
		}
	}
}

func TestFilesystemDescriptionsRespectVisibilityAndOverrides(t *testing.T) {
	memory, err := backend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := FilesystemMiddleware(FilesystemOptions{
		Backend: memory, Tools: []string{"read_file", "grep"},
		ToolDescriptions: map[string]string{"read_file": "custom read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions := map[string]tool.Definition{}
	for _, executable := range middleware.Tools {
		definition := executable.Definition()
		definitions[definition.Name] = definition
	}
	if definitions["read_file"].Description != "custom read" {
		t.Fatalf("read description = %q", definitions["read_file"].Description)
	}
	if strings.Contains(definitions["grep"].Description, "use execute") {
		t.Fatalf("grep advertises a hidden tool: %q", definitions["grep"].Description)
	}
}

func TestReadFileDistinguishesZeroWindowFromEmptyFile(t *testing.T) {
	memory, err := backend.NewMemory(map[string]backend.FileData{
		"/full.txt":  {Content: "contents", Encoding: backend.EncodingUTF8},
		"/empty.txt": {Content: " \n\t", Encoding: backend.EncodingUTF8},
	})
	if err != nil {
		t.Fatal(err)
	}
	read := filesystemTool(t, FilesystemOptions{Backend: memory}, "read_file")
	zero, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/full.txt","limit":0}`), tool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if text := zero.Content[0].Text; !strings.Contains(text, "file was not inspected") || !strings.Contains(text, "limit` was 0") {
		t.Fatalf("zero-window result = %q", text)
	}
	empty, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/empty.txt"}`), tool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if text := empty.Content[0].Text; text != "System reminder: File exists but has empty contents" {
		t.Fatalf("empty result = %q", text)
	}
}

func TestReadFilePaginationAndNegativeOffset(t *testing.T) {
	memory, err := backend.NewMemory(map[string]backend.FileData{
		"/lines.txt": {Content: "first\nsecond\nthird", Encoding: backend.EncodingUTF8},
	})
	if err != nil {
		t.Fatal(err)
	}
	read := filesystemTool(t, FilesystemOptions{Backend: memory}, "read_file")
	page, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/lines.txt","limit":2}`), tool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if text := page.Content[0].Text; !strings.Contains(text, "[Read 2 lines (lines 1-2 of 3 total). 1 line remaining from offset 2.]") {
		t.Fatalf("page result = %q", text)
	}
	clamped, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/lines.txt","offset":-2}`), tool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if text := clamped.Content[0].Text; !strings.HasPrefix(text, "1  first") || !strings.Contains(text, "Requested offset -2") {
		t.Fatalf("clamped result = %q", text)
	}
}

func TestNumberLinesUsesStableContinuationMarkers(t *testing.T) {
	text := numberLines(strings.Repeat("x", 5001)+"\nshort\n", 9)
	lines := strings.Split(text, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "  9  ") || !strings.HasPrefix(lines[1], "9.1  ") || !strings.HasPrefix(lines[2], " 10  short") {
		t.Fatalf("numbered lines = %q", text)
	}
}

func TestExecuteReportsExitStatusAndCaptureTruncation(t *testing.T) {
	shell, err := backend.NewLocalShell(backend.LocalShellOptions{
		Filesystem: backend.FilesystemOptions{Root: t.TempDir()},
		MaxOutput:  4,
	})
	if err != nil {
		t.Fatal(err)
	}
	execute := filesystemTool(t, FilesystemOptions{Backend: shell}, "execute")
	result, err := execute.Execute(context.Background(), json.RawMessage(`{"command":"printf 123456; exit 3","timeout":1}`), tool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "1234") || !strings.Contains(text, "failed with exit code 3") || !strings.Contains(text, "capture size limit") {
		t.Fatalf("execute result = %q", text)
	}
	if !strings.Contains(string(result.Artifact), `"exit_code":3`) || !strings.Contains(string(result.Artifact), `"truncated":true`) {
		t.Fatalf("execute artifact = %s", result.Artifact)
	}
	capped := filesystemTool(t, FilesystemOptions{Backend: shell, MaxExecuteTimeout: 1}, "execute")
	if _, err := capped.Execute(context.Background(), json.RawMessage(`{"command":"true","timeout":2}`), tool.Runtime{}); err == nil || !strings.Contains(err.Error(), "exceeds maximum 1") {
		t.Fatalf("execute timeout error = %v", err)
	}
}

func TestCompositeExecuteDescribesVirtualShellPaths(t *testing.T) {
	shell, err := backend.NewLocalShell(backend.LocalShellOptions{Filesystem: backend.FilesystemOptions{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := backend.NewFilesystem(backend.FilesystemOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	persistent, err := backend.NewStore(memorystore.NewMemory(), memorystore.Namespace{"files"})
	if err != nil {
		t.Fatal(err)
	}
	composite, err := backend.NewComposite(shell, map[string]backend.Backend{"/common/": mounted, "/memories/": persistent})
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := FilesystemMiddleware(FilesystemOptions{Backend: composite})
	if err != nil {
		t.Fatal(err)
	}
	system := message.System("base prompt")
	_, err = middleware.WrapModelCall(context.Background(), agent.ModelRequest{SystemMessage: &system, Tools: middleware.Tools}, func(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
		text := request.SystemMessage.TextContent()
		if !strings.Contains(text, "## Shell paths vs. virtual paths") || !strings.Contains(text, "`/common/` -> `") || !strings.Contains(text, "`/memories/`") || !strings.Contains(text, "not accessible from the shell") {
			t.Fatalf("system prompt = %q", text)
		}
		foundExecute := false
		for _, executable := range request.Tools {
			foundExecute = foundExecute || executable.Definition().Name == "execute"
		}
		if !foundExecute {
			t.Fatal("composite default shell did not expose execute")
		}
		return agent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCompositeArtifactsRootControlsToolOffload(t *testing.T) {
	memory, err := backend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	composite, err := backend.NewCompositeWithOptions(backend.CompositeOptions{Default: memory, ArtifactsRoot: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := FilesystemMiddleware(FilesystemOptions{Backend: composite, LargeResultBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	response, err := middleware.WrapToolCall(context.Background(), agent.ToolCallRequest{
		Call: message.ToolCall{ID: "evict", Name: "custom"},
	}, func(context.Context, agent.ToolCallRequest) (agent.ToolCallResponse, error) {
		return agent.ToolCallResponse{Result: tool.TextResult(strings.Repeat("x", 50))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text := response.Result.Content[0].Text; !strings.Contains(text, "/workspace/large_tool_results/evict.txt") {
		t.Fatalf("offload result = %q", text)
	}
	read, err := composite.Read(context.Background(), "/workspace/large_tool_results/evict.txt", 0, 100)
	if err != nil || read.Data == nil || read.Data.Content != strings.Repeat("x", 50) {
		t.Fatalf("offloaded file = %#v, %v", read, err)
	}
}

func schemaString(value any) string {
	text, _ := value.(string)
	return text
}

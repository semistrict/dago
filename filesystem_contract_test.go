package dago

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	memorystore "github.com/semistrict/dago/dastore"
	"github.com/semistrict/dago/datool"
)

type blockingGlobBackend struct {
	dabackend.Backend
	release <-chan struct{}
	err     error
}

type recordingGrepBackend struct {
	dabackend.Backend
	options []dabackend.GrepOptions
	result  dabackend.GrepResult
}

func (backend *recordingGrepBackend) Grep(_ context.Context, _ string, options dabackend.GrepOptions) (dabackend.GrepResult, error) {
	backend.options = append(backend.options, options)
	return backend.result, nil
}

type recordingConfigurableSandbox struct {
	dabackend.Backend
	options []dabackend.ExecuteOptions
	unknown bool
}

func (sandbox *recordingConfigurableSandbox) ID() string { return "recording" }

func (sandbox *recordingConfigurableSandbox) Execute(context.Context, string, time.Duration) (dabackend.ExecuteResult, error) {
	panic("legacy Execute called for configurable sandbox")
}

func (sandbox *recordingConfigurableSandbox) ExecuteWithOptions(_ context.Context, _ string, options dabackend.ExecuteOptions) (dabackend.ExecuteResult, error) {
	sandbox.options = append(sandbox.options, options)
	if sandbox.unknown {
		return dabackend.ExecuteResult{Output: "unknown", Truncated: true}, nil
	}
	code := 0
	return dabackend.ExecuteResult{ExitCode: &code}, nil
}

func (value blockingGlobBackend) Glob(context.Context, string, string) (dabackend.GlobResult, error) {
	if value.err != nil {
		return dabackend.GlobResult{}, value.err
	}
	<-value.release
	return dabackend.GlobResult{}, nil
}

func TestFilesystemToolSchemasDescribeEveryArgument(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
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

func TestFilesystemToolsNormalizePathsAndRejectAmbiguousHostSyntax(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	read := filesystemTool(t, FilesystemOptions{Backend: memory}, "read_file")
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"C:\\Users\\test.txt"}`), datool.Runtime{}); err == nil || !strings.Contains(err.Error(), "Windows absolute") {
		t.Fatalf("Windows path error = %v", err)
	}
	edit := filesystemTool(t, FilesystemOptions{Backend: memory}, "edit_file")
	if _, err := edit.Execute(context.Background(), json.RawMessage(`{"file_path":"./question/..","old_string":"a","new_string":"b"}`), datool.Runtime{}); err == nil || !strings.Contains(err.Error(), "traversal") {
		t.Fatalf("traversal path error = %v", err)
	}
	glob := filesystemTool(t, FilesystemOptions{Backend: memory}, "glob")
	if _, err := glob.Execute(context.Background(), json.RawMessage(`{"pattern":"../*.txt"}`), datool.Runtime{}); err == nil || !strings.Contains(err.Error(), "traversal") {
		t.Fatalf("traversal glob error = %v", err)
	}
	write := filesystemTool(t, FilesystemOptions{Backend: memory}, "write_file")
	if _, err := write.Execute(context.Background(), json.RawMessage(`{"file_path":"notes/today.txt","content":"hello"}`), datool.Runtime{}); err != nil {
		t.Fatal(err)
	}
	stored, err := memory.Read(context.Background(), "/notes/today.txt", 0, 10)
	if err != nil || stored.Data == nil || stored.Data.Content != "hello" {
		t.Fatalf("normalized write = %#v, %v", stored, err)
	}
}

func TestFilesystemDescriptionsRespectVisibilityAndOverrides(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
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
	definitions := map[string]datool.Definition{}
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
	memory, err := dabackend.NewMemory(map[string]dabackend.FileData{
		"/full.txt":  {Content: "contents", Encoding: dabackend.EncodingUTF8},
		"/empty.txt": {Content: " \n\t", Encoding: dabackend.EncodingUTF8},
	})
	if err != nil {
		t.Fatal(err)
	}
	read := filesystemTool(t, FilesystemOptions{Backend: memory}, "read_file")
	zero, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/full.txt","limit":0}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if text := zero.Content[0].Text; !strings.Contains(text, "file was not inspected") || !strings.Contains(text, "limit` was 0") {
		t.Fatalf("zero-window result = %q", text)
	}
	empty, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/empty.txt"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if text := empty.Content[0].Text; text != "System reminder: File exists but has empty contents" {
		t.Fatalf("empty result = %q", text)
	}
}

func TestReadFilePaginationAndNegativeOffset(t *testing.T) {
	memory, err := dabackend.NewMemory(map[string]dabackend.FileData{
		"/lines.txt": {Content: "first\nsecond\nthird", Encoding: dabackend.EncodingUTF8},
	})
	if err != nil {
		t.Fatal(err)
	}
	read := filesystemTool(t, FilesystemOptions{Backend: memory}, "read_file")
	page, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/lines.txt","limit":2}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if text := page.Content[0].Text; !strings.Contains(text, "[Read 2 lines (lines 1-2 of 3 total). 1 line remaining from offset 2.]") {
		t.Fatalf("page result = %q", text)
	}
	clamped, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/lines.txt","offset":-2}`), datool.Runtime{})
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
	shell, err := dabackend.NewLocalShell(dabackend.LocalShellOptions{
		Filesystem: dabackend.FilesystemOptions{Root: t.TempDir()},
		MaxOutput:  4,
	})
	if err != nil {
		t.Fatal(err)
	}
	execute := filesystemTool(t, FilesystemOptions{Backend: shell}, "execute")
	result, err := execute.Execute(context.Background(), json.RawMessage(`{"command":"printf 123456; exit 3","timeout":1}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "1234") || !strings.Contains(text, "failed with exit code 3") || !strings.Contains(text, "capture size limit") {
		t.Fatalf("execute result = %q", text)
	}
	if string(result.Artifact) != `{"exit_code":3}` {
		t.Fatalf("execute artifact = %s", result.Artifact)
	}
	capped := filesystemTool(t, FilesystemOptions{Backend: shell, MaxExecuteTimeout: 1}, "execute")
	if _, err := capped.Execute(context.Background(), json.RawMessage(`{"command":"true","timeout":2}`), datool.Runtime{}); err == nil || !strings.Contains(err.Error(), "exceeds maximum 1") {
		t.Fatalf("execute timeout error = %v", err)
	}
}

func TestExecuteArtifactOmitsUnknownExitCodeAndTruncation(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &recordingConfigurableSandbox{Backend: memory, unknown: true}
	execute := filesystemTool(t, FilesystemOptions{Backend: sandbox}, "execute")
	result, err := execute.Execute(context.Background(), json.RawMessage(`{"command":"true"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Artifact) != `{}` {
		t.Fatalf("unknown exit code artifact = %s", result.Artifact)
	}
}

func TestFilesystemNonEmptyToolAllowlistRequiresReadFile(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FilesystemMiddleware(FilesystemOptions{Backend: memory, Tools: []string{"write_file"}}); err == nil || !strings.Contains(err.Error(), "read_file must be included") {
		t.Fatalf("allowlist error = %v", err)
	}
	if _, err := FilesystemMiddleware(FilesystemOptions{Backend: memory, Tools: []string{}}); err != nil {
		t.Fatalf("empty disabled tool set: %v", err)
	}
}

func TestGrepSupportsConfiguredAndPerCallUncappedSearch(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	recording := &recordingGrepBackend{Backend: memory}
	grep := filesystemTool(t, FilesystemOptions{Backend: recording, GrepUncapped: true}, "grep")
	for _, arguments := range []string{
		`{"pattern":"hit"}`,
		`{"pattern":"hit","max_count":5}`,
		`{"pattern":"hit","max_count":null}`,
	} {
		if _, err := grep.Execute(context.Background(), json.RawMessage(arguments), datool.Runtime{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(recording.options) != 3 || !recording.options[0].Uncapped || recording.options[0].MaxCount != 0 {
		t.Fatalf("configured uncapped options = %#v", recording.options)
	}
	if recording.options[1].Uncapped || recording.options[1].MaxCount != 5 {
		t.Fatalf("per-call cap options = %#v", recording.options[1])
	}
	if !recording.options[2].Uncapped || recording.options[2].MaxCount != 0 {
		t.Fatalf("per-call null options = %#v", recording.options[2])
	}
}

func TestGrepPreservesPartialMatchesWithBoundedError(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	recording := &recordingGrepBackend{Backend: memory, result: dabackend.GrepResult{
		Error:   strings.Repeat("failure", 2_000),
		Matches: []dabackend.GrepMatch{{Path: "/partial.txt", Line: 7, Text: "hit"}},
	}}
	grep := filesystemTool(t, FilesystemOptions{Backend: recording}, "grep")
	result, err := grep.Execute(context.Background(), json.RawMessage(`{"pattern":"hit","output_mode":"content"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].Text
	if result.Status != damessage.ToolStatusError || len(text) >= len(recording.result.Error) || !strings.Contains(text, "Partial matches:") || !strings.Contains(text, "/partial.txt:") || !strings.Contains(text, "7: hit") {
		t.Fatalf("partial grep output = %q", text)
	}
}

func TestExecuteDistinguishesOmittedAndZeroTimeouts(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &recordingConfigurableSandbox{Backend: memory}
	execute := filesystemTool(t, FilesystemOptions{Backend: sandbox}, "execute")
	for _, arguments := range []string{`{"command":"true"}`, `{"command":"true","timeout":0}`, `{"command":"true","timeout":3}`} {
		if _, err := execute.Execute(context.Background(), json.RawMessage(arguments), datool.Runtime{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(sandbox.options) != 3 || sandbox.options[0].Timeout != nil {
		t.Fatalf("omitted timeout = %#v", sandbox.options)
	}
	if sandbox.options[1].Timeout == nil || *sandbox.options[1].Timeout != 0 {
		t.Fatalf("zero timeout = %#v", sandbox.options[1])
	}
	if sandbox.options[2].Timeout == nil || *sandbox.options[2].Timeout != 3*time.Second {
		t.Fatalf("custom timeout = %#v", sandbox.options[2])
	}
}

func TestCompositeExecuteDescribesVirtualShellPaths(t *testing.T) {
	shell, err := dabackend.NewLocalShell(dabackend.LocalShellOptions{Filesystem: dabackend.FilesystemOptions{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	persistent, err := dabackend.NewStore(memorystore.NewMemory(), memorystore.Namespace{"files"})
	if err != nil {
		t.Fatal(err)
	}
	composite, err := dabackend.NewComposite(shell, map[string]dabackend.Backend{"/common/": mounted, "/memories/": persistent})
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := FilesystemMiddleware(FilesystemOptions{Backend: composite})
	if err != nil {
		t.Fatal(err)
	}
	system := damessage.System("base prompt")
	_, err = middleware.WrapModelCall(context.Background(), dagent.ModelRequest{SystemMessage: &system, Tools: middleware.Tools}, func(_ context.Context, request dagent.ModelRequest) (dagent.ModelResponse, error) {
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
		return dagent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCompositeArtifactsRootControlsToolOffload(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	composite, err := dabackend.NewCompositeWithOptions(dabackend.CompositeOptions{Default: memory, ArtifactsRoot: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := FilesystemMiddleware(FilesystemOptions{Backend: composite, LargeResultBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	response, err := middleware.WrapToolCall(context.Background(), dagent.ToolCallRequest{
		Call: damessage.ToolCall{ID: "evict", Name: "custom"},
	}, func(context.Context, dagent.ToolCallRequest) (dagent.ToolCallResponse, error) {
		return dagent.ToolCallResponse{Result: datool.TextResult(strings.Repeat("x", 50))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text := response.Result.Content[0].Text; !strings.Contains(text, "/workspace/large_tool_results/evict") {
		t.Fatalf("offload result = %q", text)
	}
	read, err := composite.Read(context.Background(), "/workspace/large_tool_results/evict", 0, 100)
	if err != nil || read.Data == nil || read.Data.Content != strings.Repeat("x", 50) {
		t.Fatalf("offloaded file = %#v, %v", read, err)
	}
}

func TestToolResultOffloadConfinesArtifactAndPreservesMedia(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := FilesystemMiddleware(FilesystemOptions{
		Backend: memory, ArtifactsRoot: "/artifacts/large_tool_results", LargeResultBytes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	image := damessage.ContentBlock{Type: damessage.BlockImage, MIMEType: "image/png", Data: []byte("image")}
	response, err := middleware.WrapToolCall(context.Background(), dagent.ToolCallRequest{
		Call: damessage.ToolCall{ID: `../outside\result`, Name: "custom"},
	}, func(context.Context, dagent.ToolCallRequest) (dagent.ToolCallResponse, error) {
		return dagent.ToolCallResponse{Result: datool.Result{Content: []damessage.ContentBlock{
			{Type: damessage.BlockText, Text: strings.Repeat("x", 50)}, image,
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "/artifacts/large_tool_results/___outside_result"
	if len(response.Result.Content) != 2 || !strings.Contains(response.Result.Content[0].Text, wantPath) || response.Result.Content[1].Type != damessage.BlockImage || string(response.Result.Content[1].Data) != "image" {
		t.Fatalf("offloaded result = %#v", response.Result.Content)
	}
	read, err := memory.Read(context.Background(), wantPath, 0, 100)
	if err != nil || read.Data == nil || read.Data.Content != strings.Repeat("x", 50) {
		t.Fatalf("offloaded file = %#v, %v", read, err)
	}
	if _, err := memory.Read(context.Background(), "/artifacts/outside/result.txt", 0, 100); err == nil {
		t.Fatal("separator-bearing call ID escaped the artifact subtree")
	}
}

func TestFilesystemScrubsOnlyUnsupportedModelFacingMedia(t *testing.T) {
	pathMetadata, _ := json.Marshal("/report.pdf")
	pdf := damessage.ContentBlock{
		Type: damessage.BlockFile, MIMEType: "application/pdf", Data: []byte("pdf"),
		Extra: map[string]json.RawMessage{readFilePathMetadata: pathMetadata},
	}
	original := damessage.Message{Role: damessage.RoleTool, ToolCallID: "read", Content: []damessage.ContentBlock{pdf}}
	unknown := scrubUnsupportedFilesystemMedia([]damessage.Message{original}, modeltest.New(damodel.Profile{}))
	if unknown[0].Content[0].Type != damessage.BlockFile {
		t.Fatalf("unknown profile should preserve PDF: %#v", unknown)
	}

	falseValue := false
	rejecting := modeltest.New(damodel.Profile{
		Provider: "anthropic", SupportsPDF: true, SupportsPDFToolMessages: &falseValue,
	})
	scrubbed := scrubUnsupportedFilesystemMedia([]damessage.Message{original}, rejecting)
	if block := scrubbed[0].Content[0]; block.Type != damessage.BlockText || !strings.Contains(block.Text, "/report.pdf") || !strings.Contains(block.Text, "does not support file content") {
		t.Fatalf("scrubbed PDF = %#v", block)
	}
	if original.Content[0].Type != damessage.BlockFile || string(original.Content[0].Data) != "pdf" {
		t.Fatalf("scrub mutated persisted input: %#v", original)
	}

	docx := pdf
	docx.MIMEType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	if block := scrubUnsupportedFilesystemMedia([]damessage.Message{{Role: damessage.RoleTool, Content: []damessage.ContentBlock{docx}}}, modeltest.New(damodel.Profile{Provider: "anthropic"}))[0].Content[0]; block.Type != damessage.BlockText {
		t.Fatalf("unsupported document = %#v", block)
	}
	if block := scrubUnsupportedFilesystemMedia([]damessage.Message{{Role: damessage.RoleTool, Content: []damessage.ContentBlock{docx}}}, modeltest.New(damodel.Profile{Provider: "openai", SupportsFiles: true}))[0].Content[0]; block.Type != damessage.BlockFile {
		t.Fatalf("supported document = %#v", block)
	}
	reference := docx
	reference.Data = nil
	reference.URL = "https://example.test/file"
	if block := scrubUnsupportedFilesystemMedia([]damessage.Message{{Role: damessage.RoleHuman, Content: []damessage.ContentBlock{reference}}}, modeltest.New(damodel.Profile{Provider: "anthropic"}))[0].Content[0]; block.Type != damessage.BlockFile {
		t.Fatalf("provider-managed reference = %#v", block)
	}
}

func TestFilesystemSearchResultsUseStableModelFacingShapes(t *testing.T) {
	result := dabackend.GrepResult{Matches: []dabackend.GrepMatch{
		{Path: "/b.go", Line: 3, Text: "needle b"},
		{Path: "/a.go", Line: 2, Text: "needle a"},
		{Path: "/a.go", Line: 8, Text: "needle again"},
	}}
	content := formatGrep(result, "content", "needle", true)
	want := "/a.go:\n  2: needle a\n  8: needle again\n/b.go:\n  3: needle b"
	if content != want {
		t.Fatalf("content grep = %q", content)
	}
	truncated := result
	truncated.Truncated = true
	if text := formatGrep(truncated, "count", "needle", true); !strings.Contains(text, "/a.go: 2") || !strings.Contains(text, "valid but incomplete") {
		t.Fatalf("count grep = %q", text)
	}
	if text := formatGrep(dabackend.GrepResult{}, "files_with_matches", `foo|bar`, false); !strings.Contains(text, "No matches found") || !strings.Contains(text, "literal text, not regex") {
		t.Fatalf("empty regex-like grep = %q", text)
	}
	if text := formatGrep(dabackend.GrepResult{}, "files_with_matches", `foo|bar`, true); strings.Contains(text, "literal text, not regex") {
		t.Fatalf("redacted grep leaked a regex hint = %q", text)
	}

	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	ls := filesystemTool(t, FilesystemOptions{Backend: memory}, "ls")
	listing, err := ls.Execute(context.Background(), json.RawMessage(`{"path":"/"}`), datool.Runtime{})
	if err != nil || listing.Content[0].Text != "No files found" {
		t.Fatalf("empty listing = %#v, %v", listing, err)
	}
	glob := filesystemTool(t, FilesystemOptions{Backend: memory}, "glob")
	matches, err := glob.Execute(context.Background(), json.RawMessage(`{"pattern":"**/*.go"}`), datool.Runtime{})
	if err != nil || matches.Content[0].Text != "No files found" {
		t.Fatalf("empty glob = %#v, %v", matches, err)
	}
}

func TestFilesystemGlobBoundsUnresponsiveBackends(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	glob := filesystemTool(t, FilesystemOptions{
		Backend: blockingGlobBackend{Backend: memory, release: release}, GlobTimeout: 10 * time.Millisecond,
	}, "glob")
	for index := 0; index < 4; index++ {
		_, err := glob.Execute(context.Background(), json.RawMessage(`{"pattern":"**/*"}`), datool.Runtime{})
		if err == nil || !strings.Contains(err.Error(), "glob timed out after 10ms") {
			t.Fatalf("timeout %d error = %v", index, err)
		}
	}
	_, err = glob.Execute(context.Background(), json.RawMessage(`{"pattern":"**/*"}`), datool.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "too many glob calls") {
		t.Fatalf("overload error = %v", err)
	}
}

func TestFilesystemGlobPreservesBackendTimeoutErrors(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("backend RPC timeout")
	glob := filesystemTool(t, FilesystemOptions{
		Backend: blockingGlobBackend{Backend: memory, err: want}, GlobTimeout: time.Second,
	}, "glob")
	_, err = glob.Execute(context.Background(), json.RawMessage(`{"pattern":"**/*"}`), datool.Runtime{})
	if !errors.Is(err, want) || strings.Contains(err.Error(), "glob timed out after") {
		t.Fatalf("backend timeout error = %v", err)
	}
}

func schemaString(value any) string {
	text, _ := value.(string)
	return text
}

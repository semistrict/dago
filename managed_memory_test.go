package dago

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/datool"
)

func TestManagedMemoryGuardRestoresBlockAndKeepsUnmanagedEdits(t *testing.T) {
	backend := managedMemoryBackend(t, managedMemoryDocument("Ada", "\n", "Old note.\n"))
	guard := ManagedMemoryGuard(backend, "/AGENTS.md")
	response, err := callManagedMemoryTool(t.Context(), guard, "edit_file", "/AGENTS.md", func(ctx context.Context) error {
		current := downloadManagedMemory(t, ctx, backend, "/AGENTS.md")
		current = strings.ReplaceAll(current, "Ada", "Mallory")
		current = strings.ReplaceAll(current, "Old note.", "New note.")
		_, writeErr := backend.Write(ctx, "/AGENTS.md", current)
		return writeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Result.Status != damessage.ToolStatusError {
		t.Fatalf("status = %q", response.Result.Status)
	}
	after := downloadManagedMemory(t, t.Context(), backend, "/AGENTS.md")
	if strings.Contains(after, "Mallory") || !strings.Contains(after, `preferred name is "Ada"`) || !strings.Contains(after, "New note.") {
		t.Fatalf("restored memory = %q", after)
	}
	if strings.Count(after, ManagedMemoryBlockStart) != 1 || strings.Count(after, ManagedMemoryBlockEnd) != 1 {
		t.Fatalf("managed marker counts in %q", after)
	}
}

func TestManagedMemoryGuardRejectsInvalidStaticDependencies(t *testing.T) {
	requirePanicContaining(t, "backend is nil", func() {
		ManagedMemoryGuard(nil, "/AGENTS.md")
	})
	backend := managedMemoryBackend(t, "")
	requirePanicContaining(t, "absolute virtual path", func() {
		ManagedMemoryGuard(backend, "AGENTS.md")
	})
}

func TestManagedMemoryGuardLeavesUnchangedBlockAndIdempotentEditsAlone(t *testing.T) {
	backend := managedMemoryBackend(t, managedMemoryDocument("Ada", "\n", "Old note.\n"))
	guard := ManagedMemoryGuard(backend, "/AGENTS.md")
	edits := []struct{ old, replacement string }{
		{old: "Old note.", replacement: "New note."},
		{old: "New note.", replacement: "Final note."},
	}
	for _, edit := range edits {
		response, err := callManagedMemoryTool(t.Context(), guard, "edit_file", "/AGENTS.md", func(ctx context.Context) error {
			_, editErr := backend.Edit(ctx, "/AGENTS.md", edit.old, edit.replacement, false)
			return editErr
		})
		if err != nil || response.Result.Status == damessage.ToolStatusError {
			t.Fatalf("edit %q -> %q: response=%#v error=%v", edit.old, edit.replacement, response, err)
		}
	}
	after := downloadManagedMemory(t, t.Context(), backend, "/AGENTS.md")
	if !strings.Contains(after, "Final note.") || strings.Count(after, ManagedMemoryBlockStart) != 1 {
		t.Fatalf("memory = %q", after)
	}
}

func TestManagedMemoryGuardRepairsPartialAndDuplicateMarkers(t *testing.T) {
	tests := map[string]func(string) string{
		"missing end": func(value string) string {
			return strings.Replace(strings.Replace(value, ManagedMemoryBlockEnd, "", 1), "Ada", "Mallory", 1)
		},
		"duplicate start": func(value string) string {
			return strings.Replace(value, ManagedMemoryBlockStart, ManagedMemoryBlockStart+"\n"+ManagedMemoryBlockStart, 1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			backend := managedMemoryBackend(t, managedMemoryDocument("Ada", "\n", "Old note.\n"))
			guard := ManagedMemoryGuard(backend, "/AGENTS.md")
			response, err := callManagedMemoryTool(t.Context(), guard, "write_file", "/AGENTS.md", func(ctx context.Context) error {
				value := mutate(downloadManagedMemory(t, ctx, backend, "/AGENTS.md"))
				value = strings.Replace(value, "Old note.", "New note.", 1)
				_, writeErr := backend.Write(ctx, "/AGENTS.md", value)
				return writeErr
			})
			if err != nil || response.Result.Status != damessage.ToolStatusError {
				t.Fatalf("response=%#v error=%v", response, err)
			}
			after := downloadManagedMemory(t, t.Context(), backend, "/AGENTS.md")
			if inspect := inspectManagedBlock(after); inspect.state != managedBlockValid || !strings.Contains(inspect.content, "Ada") {
				t.Fatalf("managed block = %#v in %q", inspect, after)
			}
			if strings.Count(after, ManagedMemoryBlockStart) != 1 || strings.Count(after, ManagedMemoryBlockEnd) != 1 || !strings.Contains(after, "New note.") {
				t.Fatalf("restored memory = %q", after)
			}
		})
	}
}

func TestManagedMemoryGuardFailsClosedOnPreexistingMalformedMarkers(t *testing.T) {
	content := "notes\n" + ManagedMemoryBlockStart + "\ndangling\n"
	backend := managedMemoryBackend(t, content)
	guard := ManagedMemoryGuard(backend, "/AGENTS.md")
	called := false
	response, err := callManagedMemoryTool(t.Context(), guard, "edit_file", "/AGENTS.md", func(context.Context) error {
		called = true
		return nil
	})
	if err != nil || called || response.Result.Status != damessage.ToolStatusError {
		t.Fatalf("called=%v response=%#v error=%v", called, response, err)
	}
	if got := downloadManagedMemory(t, t.Context(), backend, "/AGENTS.md"); got != content {
		t.Fatalf("malformed file changed: %q", got)
	}
}

func TestManagedMemoryGuardPreservesCRLF(t *testing.T) {
	before := managedMemoryDocument("Ada", "\r\n", "Old note.\r\n")
	backend := managedMemoryBackend(t, before)
	guard := ManagedMemoryGuard(backend, "/AGENTS.md")
	_, err := callManagedMemoryTool(t.Context(), guard, "edit_file", "/AGENTS.md", func(ctx context.Context) error {
		current := downloadManagedMemory(t, ctx, backend, "/AGENTS.md")
		current = strings.Replace(current, "Ada", "Mallory", 1)
		current = strings.Replace(current, "Old note.", "New note.", 1)
		_, writeErr := backend.Write(ctx, "/AGENTS.md", current)
		return writeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	after := downloadManagedMemory(t, t.Context(), backend, "/AGENTS.md")
	if strings.Contains(strings.ReplaceAll(after, "\r\n", ""), "\n") || !strings.Contains(after, "New note.\r\n") {
		t.Fatalf("newline style was not preserved: %q", after)
	}
}

func TestManagedMemoryGuardRollsBackOversizedUnmergeableEdit(t *testing.T) {
	var extra strings.Builder
	for index := 0; index < maxManagedMemoryDiffLines+1; index++ {
		extra.WriteString("note\n")
	}
	before := managedMemoryDocument("Ada", "\n", extra.String())
	backend := managedMemoryBackend(t, before)
	guard := ManagedMemoryGuard(backend, "/AGENTS.md")
	response, err := callManagedMemoryTool(t.Context(), guard, "write_file", "/AGENTS.md", func(ctx context.Context) error {
		_, writeErr := backend.Write(ctx, "/AGENTS.md", "replacement without markers\n")
		return writeErr
	})
	if err != nil || response.Result.Status != damessage.ToolStatusError {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	if got := downloadManagedMemory(t, t.Context(), backend, "/AGENTS.md"); got != before {
		t.Fatal("oversized unsafe merge did not roll back the complete file")
	}
}

func TestManagedMemoryGuardRestoresPartialMutationWhenToolReturnsError(t *testing.T) {
	backend := managedMemoryBackend(t, managedMemoryDocument("Ada", "\n", "Keep note.\n"))
	guard := ManagedMemoryGuard(backend, "/AGENTS.md")
	response, err := callManagedMemoryTool(t.Context(), guard, "edit_file", "/AGENTS.md", func(ctx context.Context) error {
		current := downloadManagedMemory(t, ctx, backend, "/AGENTS.md")
		if _, writeErr := backend.Write(ctx, "/AGENTS.md", strings.Replace(current, "Ada", "Mallory", 1)); writeErr != nil {
			return writeErr
		}
		return &memoryTestError{text: "simulated partial tool failure"}
	})
	if err != nil || response.Result.Status != damessage.ToolStatusError {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	if got := downloadManagedMemory(t, t.Context(), backend, "/AGENTS.md"); strings.Contains(got, "Mallory") || !strings.Contains(got, "Ada") {
		t.Fatalf("partial mutation was not restored: %q", got)
	}
}

func TestManagedMemoryGuardFailsClosedOnInvalidUTF8AtConfiguredPath(t *testing.T) {
	backend := dabackend.NewMemory(map[string]dabackend.FileData{
		"/AGENTS.md": {Content: string([]byte{0xff, 0xfe}), Encoding: dabackend.EncodingUTF8},
	})
	guard := ManagedMemoryGuard(backend, "/AGENTS.md")
	called := false
	response, err := callManagedMemoryTool(t.Context(), guard, "write_file", "/AGENTS.md", func(context.Context) error {
		called = true
		return nil
	})
	if err != nil || called || response.Result.Status != damessage.ToolStatusError {
		t.Fatalf("called=%v response=%#v error=%v", called, response, err)
	}
}

func TestManagedMemoryGuardAllowsCreatingAConfiguredMissingFile(t *testing.T) {
	root := t.TempDir()
	backend, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	guard := ManagedMemoryGuard(backend, "/AGENTS.md")
	response, err := callManagedMemoryTool(t.Context(), guard, "write_file", "/AGENTS.md", func(ctx context.Context) error {
		_, writeErr := backend.Write(ctx, "/AGENTS.md", managedMemoryDocument("Ada", "\n", ""))
		return writeErr
	})
	if err != nil || response.Result.Status == damessage.ToolStatusError {
		t.Fatalf("response=%#v error=%v", response, err)
	}
}

func TestManagedMemoryGuardBlocksDeleteOfFileAndParent(t *testing.T) {
	for _, target := range []string{"/memory/AGENTS.md", "/memory"} {
		t.Run(target, func(t *testing.T) {
			backend := dabackend.NewMemory(map[string]dabackend.FileData{
				"/memory/AGENTS.md": {Content: managedMemoryDocument("Ada", "\n", ""), Encoding: dabackend.EncodingUTF8},
			})
			guard := ManagedMemoryGuard(backend, "/memory/AGENTS.md")
			called := false
			response, callErr := callManagedMemoryTool(t.Context(), guard, "delete", target, func(context.Context) error {
				called = true
				return nil
			})
			if callErr != nil || called || response.Result.Status != damessage.ToolStatusError {
				t.Fatalf("called=%v response=%#v error=%v", called, response, callErr)
			}
		})
	}
}

func TestManagedMemoryGuardSerializesConcurrentOutsideEdits(t *testing.T) {
	backend := managedMemoryBackend(t, managedMemoryDocument("Ada", "\n", "alpha\nbeta\n"))
	guard := ManagedMemoryGuard(backend, "/AGENTS.md")
	start := make(chan struct{})
	var wait sync.WaitGroup
	errors := make(chan error, 2)
	for _, replacement := range []struct{ old, new string }{{"alpha", "one"}, {"beta", "two"}} {
		wait.Add(1)
		go func(old, replacement string) {
			defer wait.Done()
			<-start
			response, err := callManagedMemoryTool(context.Background(), guard, "edit_file", "/AGENTS.md", func(ctx context.Context) error {
				_, editErr := backend.Edit(ctx, "/AGENTS.md", old, replacement, false)
				return editErr
			})
			if err == nil && response.Result.Status == damessage.ToolStatusError {
				err = &memoryTestError{text: "outside edit was rejected"}
			}
			errors <- err
		}(replacement.old, replacement.new)
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	after := downloadManagedMemory(t, t.Context(), backend, "/AGENTS.md")
	if !strings.Contains(after, "one\ntwo") || !strings.Contains(after, `preferred name is "Ada"`) {
		t.Fatalf("concurrent memory = %q", after)
	}
}

func TestManagedMemoryGuardBlocksSymlinkAliasAndBackendConfinesEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(managedMemoryDocument("Ada", "\n", "")), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("AGENTS.md", filepath.Join(root, "alias.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.md")); err != nil {
		t.Fatal(err)
	}
	backend, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	guard := ManagedMemoryGuard(backend, "/AGENTS.md")
	called := false
	response, err := callManagedMemoryTool(t.Context(), guard, "edit_file", "/alias.md", func(context.Context) error {
		called = true
		return nil
	})
	if err != nil || called || response.Result.Status != damessage.ToolStatusError {
		t.Fatalf("alias called=%v response=%#v error=%v", called, response, err)
	}
	configuredAlias := ManagedMemoryGuard(backend, "/alias.md")
	response, err = callManagedMemoryTool(t.Context(), configuredAlias, "edit_file", "/alias.md", func(context.Context) error {
		t.Fatal("configured symlink edit should not run")
		return nil
	})
	if err != nil || response.Result.Status != damessage.ToolStatusError {
		t.Fatalf("configured alias response=%#v error=%v", response, err)
	}
	if _, err := backend.WriteDurable(t.Context(), "/alias.md", "replacement"); err == nil {
		t.Fatal("durable writer replaced a final symlink")
	}
	if _, err := backend.Write(t.Context(), "/escape.md", "changed"); err == nil {
		t.Fatal("confined backend allowed an escaping symlink write")
	}
	outsideContent, err := os.ReadFile(outside)
	if err != nil || string(outsideContent) != "outside" {
		t.Fatalf("outside content=%q error=%v", outsideContent, err)
	}
}

func TestManagedMemoryGuardUsesDurableRestoreWithoutChangingMode(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(filePath, []byte(managedMemoryDocument("Ada", "\n", "Old note.\n")), 0o640); err != nil {
		t.Fatal(err)
	}
	backend, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	guard := ManagedMemoryGuard(backend, "/AGENTS.md")
	_, err = callManagedMemoryTool(t.Context(), guard, "edit_file", "/AGENTS.md", func(ctx context.Context) error {
		_, editErr := backend.Edit(ctx, "/AGENTS.md", "Ada", "Mallory", false)
		return editErr
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filePath)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%v error=%v", info.Mode().Perm(), err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "AGENTS.md" {
		t.Fatalf("durable-write leftovers=%v error=%v", entries, err)
	}
	if content, err := os.ReadFile(filePath); err != nil || strings.Contains(string(content), "Mallory") {
		t.Fatalf("restored content=%q error=%v", content, err)
	}
}

func TestManagedMemoryGuardIsAutomaticallyAppliedByFilesystemConfig(t *testing.T) {
	backend := managedMemoryBackend(t, managedMemoryDocument("Ada", "\n", "Old note.\n"))
	middleware := mustFilesystem(backend, Filesystem{managedMemoryPaths: []string{"/AGENTS.md"}})
	tool := filesystemTool(t, backend, Filesystem{}, "edit_file")
	arguments := json.RawMessage(`{"file_path":"/AGENTS.md","old_string":"Ada","new_string":"Mallory"}`)
	request := dagent.ToolCallRequest{Call: damessage.ToolCall{ID: "call-1", Name: "edit_file", Arguments: arguments}, Tool: tool}
	response, err := middleware.WrapToolCall(t.Context(), request, func(ctx context.Context, call dagent.ToolCallRequest) (dagent.ToolCallResponse, error) {
		result, executeErr := call.Tool.Execute(ctx, call.Call.Arguments, datool.Runtime{CallID: call.Call.ID})
		return dagent.ToolCallResponse{Result: result}, executeErr
	})
	if err != nil || response.Result.Status != damessage.ToolStatusError {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	if after := downloadManagedMemory(t, t.Context(), backend, "/AGENTS.md"); strings.Contains(after, "Mallory") {
		t.Fatalf("automatic guard did not restore memory: %q", after)
	}
}

func TestNewAgentAutomaticallyGuardsConfiguredMemorySources(t *testing.T) {
	before := managedMemoryDocument("Ada", "\n", "Keep note.\n")
	backend := managedMemoryBackend(t, before)
	changed := strings.Replace(before, "Ada", "Mallory", 1)
	model := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
			ID: "change-memory", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/AGENTS.md","content":` + mustJSONText(changed) + `}`),
		}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			latest := request.Messages[len(request.Messages)-1]
			if latest.Role != damessage.RoleTool || latest.ToolStatus != damessage.ToolStatusError || !strings.Contains(latest.TextContent(), "machine-managed") {
				return &memoryTestError{text: "managed memory edit did not return a tool error"}
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	agent := New(model, WithBackend(backend), WithFilesystem(Filesystem{}), WithMemory(Memory{Sources: []string{"/AGENTS.md"}}))
	if _, err := agent.Invoke(t.Context(), dagent.Prompt("change memory")); err != nil {
		t.Fatal(err)
	}
	if got := downloadManagedMemory(t, t.Context(), backend, "/AGENTS.md"); got != before {
		t.Fatalf("managed memory = %q", got)
	}
}

func TestInheritedSubagentFilesystemKeepsParentMemoryGuard(t *testing.T) {
	configured := agentConfig{managedMemoryPaths: []string{"/AGENTS.md"}}
	inherited := inheritedSubagentConfig(configured, nil, false)
	WithFilesystem(Filesystem{ReadLimit: 12}).apply(&inherited)
	if len(inherited.managedMemoryPaths) != 1 || inherited.managedMemoryPaths[0] != "/AGENTS.md" {
		t.Fatalf("inherited managed memory paths = %#v", inherited.managedMemoryPaths)
	}
}

func managedMemoryBackend(t *testing.T, content string) *dabackend.Memory {
	t.Helper()
	backend := dabackend.NewMemory(map[string]dabackend.FileData{
		"/AGENTS.md": {Content: content, Encoding: dabackend.EncodingUTF8},
	})
	return backend
}

func managedMemoryDocument(name, newline, extra string) string {
	return "## User Preferences" + newline + newline +
		ManagedMemoryBlockStart + newline +
		`- The user's preferred name is "` + name + `".` + newline +
		ManagedMemoryBlockEnd + newline + newline + extra
}

func callManagedMemoryTool(ctx context.Context, middleware dagent.Middleware, name, filePath string, mutate func(context.Context) error) (dagent.ToolCallResponse, error) {
	arguments, _ := json.Marshal(map[string]string{"file_path": filePath})
	request := dagent.ToolCallRequest{Call: damessage.ToolCall{ID: "call-1", Name: name, Arguments: arguments}}
	return middleware.WrapToolCall(ctx, request, func(ctx context.Context, _ dagent.ToolCallRequest) (dagent.ToolCallResponse, error) {
		if err := mutate(ctx); err != nil {
			return dagent.ToolCallResponse{}, err
		}
		return dagent.ToolCallResponse{Result: datool.TextResult("ok")}, nil
	})
}

func downloadManagedMemory(t *testing.T, ctx context.Context, backend dabackend.Backend, filePath string) string {
	t.Helper()
	results := backend.Download(ctx, []string{filePath})
	if len(results) != 1 || results[0].Error != "" {
		t.Fatalf("download %s = %#v", filePath, results)
	}
	return string(results[0].Content)
}

func mustJSONText(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

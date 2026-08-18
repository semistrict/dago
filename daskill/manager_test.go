package daskill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
)

func writeTestSkill(t *testing.T, root, name, description, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestManagerPrecedenceAndUsefulDefaults(t *testing.T) {
	root := t.TempDir()
	low := filepath.Join(root, "low")
	high := filepath.Join(root, "high")
	writeTestSkill(t, low, "review", "low source", "low body")
	writeTestSkill(t, high, "review", "high source", "high body")
	writeTestSkill(t, low, "only-low", "only low", "body")
	manager := NewManager([]Source{{Name: "low", Dir: low}, {Name: "high", Dir: high}}, NewTrustStore(filepath.Join(root, "trust.json")), ManagerOptions{})

	entries, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Skill.Name != "review" || entries[1].Source != "high" || entries[1].Skill.Description != "high source" {
		t.Fatalf("unexpected effective entries: %#v", entries)
	}
	loaded, err := manager.Load(context.Background(), "review")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Skill.Body != "high body" {
		t.Fatalf("loaded body = %q", loaded.Skill.Body)
	}
}

func TestManagerExternalSymlinkRequiresExactTrust(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated Windows privileges")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	external := filepath.Join(root, "external")
	writeTestSkill(t, external, "linked", "external skill", "trusted body")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, "linked"), filepath.Join(source, "linked")); err != nil {
		t.Fatal(err)
	}
	store := NewTrustStore(filepath.Join(root, "state", "trust.json"))
	manager := NewManager([]Source{{Name: "project", Dir: source}}, store, ManagerOptions{})

	listed, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].TrustRequired || listed[0].Skill.Description != "" {
		t.Fatalf("untrusted content was read or omitted: %#v", listed)
	}
	_, err = manager.Load(context.Background(), "linked")
	var trustErr *TrustRequiredError
	wantTarget, evalErr := filepath.EvalSymlinks(filepath.Join(external, "linked"))
	if evalErr != nil {
		t.Fatal(evalErr)
	}
	if !errors.As(err, &trustErr) || trustErr.TargetDir != wantTarget {
		t.Fatalf("load error = %#v", err)
	}
	if err := store.Trust(context.Background(), trustErr.TargetDir); err != nil {
		t.Fatal(err)
	}
	loaded, err := manager.Load(context.Background(), "linked")
	if err != nil || loaded.Skill.Body != "trusted body" {
		t.Fatalf("trusted load = %#v, %v", loaded, err)
	}
}

func TestTrustStoreRejectsCorruptionBroadPermissionsAndRepointing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission and symlink semantics are Unix-specific")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "trust.json")
	store := NewTrustStore(path)
	if err := store.Trust(context.Background(), link); err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(context.Background(), link); err != nil {
		t.Fatal(err)
	}
	records, err := store.List(context.Background())
	if err != nil || len(records) != 0 {
		t.Fatalf("revoke through symlink left records %#v: %v", records, err)
	}
	if err := store.Trust(context.Background(), link); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	trusted, err := store.Trusted(context.Background(), link)
	if err != nil || trusted {
		t.Fatalf("repointed link trusted=%v err=%v", trusted, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background()); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("broad permission error = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(context.Background()); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt-store clear error = %v", err)
	}
}

func TestManagerCancellationAndBounds(t *testing.T) {
	root := t.TempDir()
	writeTestSkill(t, root, "large", "large", strings.Repeat("x", 32))
	manager := NewManager([]Source{{Name: "test", Dir: root}}, NewTrustStore(filepath.Join(root, "trust.json")), ManagerOptions{MaximumFile: 16})
	if _, err := manager.Load(context.Background(), "large"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("size error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestBuiltinsAreFreshAndComplete(t *testing.T) {
	first := Builtins()
	second := Builtins()
	if len(first) != 3 {
		t.Fatalf("builtins = %#v", first)
	}
	first[0].Body = "changed"
	if second[0].Body == "changed" {
		t.Fatal("Builtins returned shared mutable data")
	}
	names := map[string]bool{}
	for _, skill := range second {
		names[skill.Name] = skill.Description != "" && skill.Body != ""
	}
	for _, name := range []string{"remember", "skill-creator", "deepagents-thread-inspector"} {
		if !names[name] {
			t.Fatalf("missing complete builtin %q", name)
		}
	}
}

func TestThreadInspectorReconstructsReadOnlyBoundedTranscript(t *testing.T) {
	saver := dacheckpoint.NewMemorySaver()
	checkpoint, err := dacheckpoint.Empty(0)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.ChannelValues[dagent.MessagesKey] = dacheckpoint.DeltaSnapshot{Value: []damessage.Message{
		{ID: "remove-me", Role: damessage.RoleHuman, Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: "old"}}},
		{ID: "replace-me", Role: damessage.RoleHuman, Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: "before"}}},
		{ID: "replace-me", Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: "after"}}},
		{ID: "remove-me", Role: damessage.RoleRemove},
	}}
	checkpoint.ChannelVersions[dagent.MessagesKey] = "v1"
	config, err := saver.Put(context.Background(), dacheckpoint.Config{ThreadID: "thread-1"}, checkpoint, dacheckpoint.Metadata{}, map[string]string{dagent.MessagesKey: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	inspector := NewThreadInspector(saver, ThreadInspectorOptions{})
	threads, err := inspector.List(context.Background())
	if err != nil || len(threads) != 1 || threads[0].ThreadID != "thread-1" || threads[0].CheckpointID != config.CheckpointID {
		t.Fatalf("threads = %#v, %v", threads, err)
	}
	messages, err := inspector.Inspect(context.Background(), "thread-1")
	if err != nil || len(messages) != 1 || messages[0].Role != damessage.RoleAssistant || messages[0].TextContent() != "after" {
		t.Fatalf("messages = %#v, %v", messages, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := inspector.Inspect(ctx, "thread-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

package dacode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveInitialAgentPrecedenceAndUsefulProfileDefaults(t *testing.T) {
	stateDir := t.TempDir()
	for _, name := range []string{"default-profile", "recent-profile"} {
		if err := ensureAgentMemoryFile(stateDir, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeAgentPreference(stateDir, "recent-profile", recentAgentPreference); err != nil {
		t.Fatal(err)
	}
	if _, err := toggleDefaultAgent(t.Context(), stateDir, "default-profile"); err != nil {
		t.Fatal(err)
	}

	selected, err := resolveInitialAgent(t.Context(), stateDir, "")
	if err != nil || selected != "default-profile" {
		t.Fatalf("default selection = %q, %v", selected, err)
	}
	selected, err = resolveInitialAgent(t.Context(), stateDir, "explicit-profile")
	if err != nil || selected != "explicit-profile" {
		t.Fatalf("explicit selection = %q, %v", selected, err)
	}
	for _, relative := range []string{
		filepath.Join("explicit-profile", agentInstructionsFilename),
		filepath.Join("explicit-profile", agentSkillsDirectory),
		filepath.Join("explicit-profile", agentSessionsDirectory),
	} {
		if _, err := os.Stat(filepath.Join(stateDir, relative)); err != nil {
			t.Fatalf("profile default %s: %v", relative, err)
		}
	}
	if recent, err := configuredRecentAgent(stateDir); err != nil || recent != "explicit-profile" {
		t.Fatalf("recent selection = %q, %v", recent, err)
	}
}

func TestResolveInitialAgentIgnoresStalePreferences(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, defaultAgentFilename), []byte("missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, recentAgentFilename), []byte("also-missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := resolveInitialAgent(t.Context(), stateDir, "")
	if err != nil || selected != defaultAgentName {
		t.Fatalf("stale fallback = %q, %v", selected, err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, defaultAgentName, agentInstructionsFilename)); err != nil {
		t.Fatalf("built-in profile was not initialized: %v", err)
	}
}

func TestResolveInitialAgentUsesCanonicalConfiguredDefaultThenRecent(t *testing.T) {
	stateDir := t.TempDir()
	for _, name := range []string{"configured-default", "configured-recent", "file-default"} {
		if err := ensureAgentMemoryFile(stateDir, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := toggleDefaultAgent(t.Context(), stateDir, "file-default"); err != nil {
		t.Fatal(err)
	}
	selected, err := resolveInitialAgentConfigured(t.Context(), stateDir, "", "configured-default", "configured-recent")
	if err != nil || selected != "configured-default" {
		t.Fatalf("configured default selection = %q, %v", selected, err)
	}
	selected, err = resolveInitialAgentConfigured(t.Context(), stateDir, "", "missing", "configured-recent")
	if err != nil || selected != "file-default" {
		t.Fatalf("local default selection = %q, %v", selected, err)
	}
	if _, err := toggleDefaultAgent(t.Context(), stateDir, "file-default"); err != nil {
		t.Fatal(err)
	}
	selected, err = resolveInitialAgentConfigured(t.Context(), stateDir, "", "missing", "configured-recent")
	if err != nil || selected != "configured-recent" {
		t.Fatalf("configured recent selection = %q, %v", selected, err)
	}
}

func TestResolveInitialAgentRejectsReservedAndUnsafeExplicitNames(t *testing.T) {
	for _, name := range []string{"plugins", "bin", "conversation_history", ".state", "../escape", "line\nbreak"} {
		stateDir := t.TempDir()
		if _, err := resolveInitialAgent(t.Context(), stateDir, name); err == nil {
			t.Errorf("explicit agent %q was accepted", name)
		}
	}
}

func TestResolveInitialAgentHonorsCancellationBeforeCreatingProfile(t *testing.T) {
	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolveInitialAgent(ctx, stateDir, "new-profile"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled selection error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "new-profile")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled selection created a profile: %v", err)
	}
}

func TestResetAgentProfileCopiesPromptAndReplacesPrivateState(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureAgentMemoryFile(stateDir, "source"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "source", agentInstructionsFilename), []byte("Review carefully.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureAgentMemoryFile(stateDir, "destination"); err != nil {
		t.Fatal(err)
	}
	oldSession := filepath.Join(stateDir, "destination", agentSessionsDirectory, "old")
	if err := os.WriteFile(oldSession, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	preview, err := resetAgentProfile(t.Context(), stateDir, "destination", "source", true)
	if err != nil || !preview.DryRun {
		t.Fatalf("dry run = %#v, %v", preview, err)
	}
	if _, err := os.Stat(oldSession); err != nil {
		t.Fatalf("dry run changed state: %v", err)
	}
	result, err := resetAgentProfile(t.Context(), stateDir, "destination", "source", false)
	if err != nil || result.Source != "source" {
		t.Fatalf("reset = %#v, %v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "destination", agentInstructionsFilename))
	if err != nil || string(data) != "Review carefully.\n" {
		t.Fatalf("copied prompt = %q, %v", data, err)
	}
	if _, err := os.Stat(oldSession); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old profile state survived reset: %v", err)
	}
	for _, child := range []string{agentSkillsDirectory, agentSessionsDirectory} {
		info, err := os.Stat(filepath.Join(stateDir, "destination", child))
		if err != nil || !info.IsDir() {
			t.Fatalf("reset %s directory = %#v, %v", child, info, err)
		}
	}
}

func TestResetAgentProfileCancellationAndSymlinkFailClosed(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureAgentMemoryFile(stateDir, "kept"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "kept", agentInstructionsFilename), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resetAgentProfile(canceled, stateDir, "kept", "", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reset error = %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(stateDir, "kept", agentInstructionsFilename))
	if string(data) != "keep" {
		t.Fatalf("canceled reset changed prompt to %q", data)
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(stateDir, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := resetAgentProfile(t.Context(), stateDir, "linked", "", false); err == nil || !strings.Contains(err.Error(), "confined directory") {
		t.Fatalf("symlink reset error = %v", err)
	}
}

func TestAgentsCLIListResetJSONAndNoAuthentication(t *testing.T) {
	stateDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if _, err := resetAgentProfile(t.Context(), stateDir, "writer", "", false); err != nil {
		t.Fatal(err)
	}
	if err := writeAgentPreference(stateDir, "writer", recentAgentPreference); err != nil {
		t.Fatal(err)
	}
	var output, stderr bytes.Buffer
	if err := Run(t.Context(), []string{"agents", "list", "--json", "--state-dir", stateDir, "--config", configPath}, strings.NewReader(""), &output, &stderr); err != nil {
		t.Fatalf("agents list: %v; stderr=%s", err, stderr.String())
	}
	var envelope struct {
		Version int `json:"version"`
		Result  []struct {
			Name     string `json:"name"`
			IsRecent bool   `json:"is_recent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != 1 || len(envelope.Result) != 2 || envelope.Result[1].Name != "writer" || !envelope.Result[1].IsRecent {
		t.Fatalf("list payload = %#v", envelope)
	}

	output.Reset()
	if err := Run(t.Context(), []string{"agents", "reset", "--agent", "writer", "--dry-run", "--state-dir", stateDir, "--config", configPath}, strings.NewReader(""), &output, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Would reset agent writer") {
		t.Fatalf("dry-run output = %q", output.String())
	}
}

func TestParseCLIAgentAliases(t *testing.T) {
	for _, arguments := range [][]string{{"-a", "reviewer"}, {"--agent", "reviewer"}} {
		options, err := parseCLI(arguments, &bytes.Buffer{})
		if err != nil || options.agent != "reviewer" {
			t.Fatalf("parse %v = %q, %v", arguments, options.agent, err)
		}
	}
}

func TestAgentProfileSkillsAreSelectedAndReadOnly(t *testing.T) {
	stateDir := t.TempDir()
	for _, name := range []string{"one", "two"} {
		if err := ensureAgentMemoryFile(stateDir, name); err != nil {
			t.Fatal(err)
		}
	}
	skillDir := filepath.Join(stateDir, "one", agentSkillsDirectory, "review")
	if err := os.Mkdir(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: review\ndescription: Review code\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := &agentIdentity{name: "one"}
	backend, err := openAgentMemory(stateDir, identity)
	if err != nil {
		t.Fatal(err)
	}
	listing, err := backend.List(t.Context(), "/skills")
	if err != nil || len(listing.Entries) != 1 || strings.TrimSuffix(listing.Entries[0].Path, "/") != "/skills/review" {
		t.Fatalf("profile one skills = %#v, %v", listing, err)
	}
	read, err := backend.Read(t.Context(), "/skills/review/SKILL.md", 0, 1024)
	if err != nil || read.Data == nil || !strings.Contains(read.Data.Content, "name: review") {
		t.Fatalf("read selected skill = %#v, %v", read, err)
	}
	if _, err := backend.Write(t.Context(), "/skills/review/SKILL.md", "changed"); err == nil {
		t.Fatal("agent skill write was accepted")
	}
	identity.set("two")
	listing, err = backend.List(t.Context(), "/skills")
	if err != nil || len(listing.Entries) != 0 {
		t.Fatalf("profile two skills = %#v, %v", listing, err)
	}
}

func TestAgentProfileSkillSymlinkCannotCrossProfileBoundary(t *testing.T) {
	stateDir := t.TempDir()
	for _, name := range []string{"one", "two"} {
		if err := ensureAgentMemoryFile(stateDir, name); err != nil {
			t.Fatal(err)
		}
	}
	secretDir := filepath.Join(stateDir, "two", agentSkillsDirectory, "secret")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "SKILL.md"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(stateDir, "one", agentSkillsDirectory, "escape")
	if err := os.Symlink(filepath.Join("..", "..", "two", agentSkillsDirectory, "secret"), link); err != nil {
		t.Fatal(err)
	}
	backend, err := openAgentMemory(stateDir, &agentIdentity{name: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Read(t.Context(), "/skills/escape/SKILL.md", 0, 10); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("cross-profile skill read error = %v", err)
	}
}

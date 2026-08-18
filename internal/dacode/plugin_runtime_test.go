package dacode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dahook"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/daplugin"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

func TestRunDispatchesPluginAliasesBeforeModelStartup(t *testing.T) {
	for _, command := range []string{"plugin", "plugins"} {
		var output bytes.Buffer
		err := Run(t.Context(), []string{command, "--store", t.TempDir(), "list", "--json"}, strings.NewReader(""), &output, io.Discard)
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		if !strings.Contains(output.String(), `"command":"plugin"`) {
			t.Fatalf("%s output = %q", command, output.String())
		}
	}
}

func TestLoadRuntimePluginsComposesNamespacedSkillsHooksAndMCP(t *testing.T) {
	storeRoot, projectRoot := t.TempDir(), t.TempDir()
	plugin := installRuntimePlugin(t, storeRoot, `{
  "name":"review",
  "skills":"./skills",
  "mcpServers":{"local":{"command":"helper","args":["${CLAUDE_PLUGIN_ROOT}"],"cwd":".","env":{"OWN":"value"}}},
  "hooks":{"SessionStart":[]}
}`)
	writePluginCLIFile(t, plugin.Root, "skills/check/SKILL.md", "---\nname: check\ndescription: Check changes.\n---\nUse the checker.\n")
	writePluginCLIFile(t, plugin.Root, "skills/nested/check/SKILL.md", "---\nname: check\ndescription: Check nested changes.\n---\nUse the nested checker.\n")
	writePluginCLIFile(t, plugin.Root, ".mcp.json", `{"mcp_servers":{"local":{"command":"file-helper"}}}`)
	canonicalRoot, err := filepath.EvalSymlinks(plugin.Root)
	if err != nil {
		t.Fatal(err)
	}

	components, err := loadRuntimePlugins(t.Context(), storeRoot, projectRoot, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if len(components.SkillCatalog) != 2 || components.SkillCatalog[0].Name != "review@market:check" || components.SkillCatalog[1].Name != "review@market:nested:check" {
		t.Fatalf("skill catalog = %#v routes=%#v warnings=%#v root=%q", components.SkillCatalog, components.SkillRoutes, components.Warnings, plugin.Root)
	}
	if len(components.SkillRoutes) != 1 || len(components.Hooks) != 1 || len(components.MCP) != 1 {
		t.Fatalf("components = skills:%d hooks:%d mcp:%d warnings:%v", len(components.SkillRoutes), len(components.Hooks), len(components.MCP), components.Warnings)
	}
	connection := components.MCP[0]
	if !strings.HasPrefix(connection.Name, "plugin__review_market_") || connection.Command != "helper" || connection.CWD != canonicalRoot || connection.Env["CLAUDE_PLUGIN_ROOT"] != canonicalRoot || connection.Env["PLUGIN_DATA"] == "" || connection.Env["OWN"] != "value" {
		t.Fatalf("connection = %#v", connection)
	}
	if info, err := os.Stat(connection.Env["PLUGIN_DATA"]); err != nil || !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("plugin data directory = %#v, %v", info, err)
	}
	manager := daplugin.NewManager(daplugin.NewStore(storeRoot, daplugin.StoreOptions{}), nil, daplugin.ManagerOptions{})
	if err := manager.Disable(t.Context(), plugin.ID); err != nil {
		t.Fatal(err)
	}
	disabled, err := loadRuntimePlugins(t.Context(), storeRoot, projectRoot, os.LookupEnv)
	if err != nil || len(disabled.SkillCatalog)+len(disabled.Hooks)+len(disabled.MCP) != 0 {
		t.Fatalf("disabled = %#v, %v", disabled, err)
	}
}

func TestLoadRuntimePluginsSupportsSingleRootSkillDocument(t *testing.T) {
	storeRoot, projectRoot := t.TempDir(), t.TempDir()
	plugin := installRuntimePlugin(t, storeRoot, `{"name":"review","skills":"./SKILL.md"}`)
	writePluginCLIFile(t, plugin.Root, "SKILL.md", "---\nname: review\ndescription: Review changes.\n---\nReview carefully.\n")
	components, err := loadRuntimePlugins(t.Context(), storeRoot, projectRoot, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(components.SkillCatalog) != 1 || components.SkillCatalog[0].Name != "review@market:review" || !strings.HasSuffix(components.SkillCatalog[0].Path, "/SKILL.md") {
		t.Fatalf("single-file catalog = %#v", components.SkillCatalog)
	}
}

func TestLoadRuntimePluginsCancellationAndLinkedDataFailClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := loadRuntimePlugins(ctx, t.TempDir(), t.TempDir(), os.LookupEnv)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	storeRoot, projectRoot := t.TempDir(), t.TempDir()
	plugin := installRuntimePlugin(t, storeRoot, `{"name":"review","mcpServers":{"local":{"command":"helper"}},"hooks":{"SessionStart":[]}}`)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(storeRoot, "data")); err != nil {
		t.Fatal(err)
	}
	components, err := loadRuntimePlugins(t.Context(), storeRoot, projectRoot, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(components.Hooks) != 0 || len(components.MCP) != 0 || len(components.Warnings) == 0 {
		t.Fatalf("linked data did not fail closed: %#v (%s)", components, plugin.ID)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside data directory was modified: %v %#v", err, entries)
	}
}

func TestPluginMCPServerLimitFailsClosed(t *testing.T) {
	storeRoot := t.TempDir()
	canonicalStore, err := filepath.EvalSymlinks(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	servers := make(map[string]json.RawMessage, maxPluginMCPServers+1)
	for index := 0; index <= maxPluginMCPServers; index++ {
		servers[fmt.Sprintf("server_%03d", index)] = json.RawMessage(`{"command":"helper"}`)
	}
	components := pluginRuntimeComponents{}
	components.composeMCP(t.Context(), storeRoot, []daplugin.MCPSource{{
		PluginID: "large@market", Inline: servers,
		Environment: map[string]string{
			"CLAUDE_PLUGIN_ROOT": t.TempDir(), "PLUGIN_DATA": filepath.Join(canonicalStore, "data", "large"), "CLAUDE_PROJECT_DIR": t.TempDir(),
		},
	}}, os.LookupEnv)
	if len(components.MCP) != 0 || len(components.Warnings) != 1 || !strings.Contains(components.Warnings[0], "server limit") {
		t.Fatalf("oversized MCP composition = %#v", components)
	}
}

func TestDacodeHookRuntimeExecutesFaithfulLifecycleBoundaries(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	project, user, data := t.TempDir(), t.TempDir(), t.TempDir()
	document := hookTestDocument(t, executable, []dahook.Event{
		dahook.SessionStart, dahook.UserPromptSubmit, dahook.PreToolUse, dahook.PostToolUse,
		dahook.PostToolUseFailure, dahook.Stop, dahook.SessionEnd,
	})
	hooks, err := newDacodeHookRuntime(t.Context(), project, []dahook.Plugin{{
		ID: "test@local", Inline: document, Enabled: true,
		Environment: map[string]string{"CLAUDE_PLUGIN_ROOT": project, "PLUGIN_DATA": data, "CLAUDE_PROJECT_DIR": project},
	}}, dacodeHookRuntimeOptions{Headless: true, UserConfigDir: user})
	if err != nil {
		t.Fatal(err)
	}
	middleware := hooks.Middleware()
	runtimeContext := dagent.Runtime{Config: dacheckpoint.Config{ThreadID: "thread"}}
	values := dastate.Values{dagent.MessagesKey: []damessage.Message{damessage.Human("hello")}}
	update, err := middleware.BeforeAgent(t.Context(), values, runtimeContext)
	if err != nil {
		t.Fatal(err)
	}
	contextValues, _ := update[hookContextStateKey].([]string)
	if got := strings.Join(contextValues, ","); got != "from-SessionStart,from-UserPromptSubmit" {
		t.Fatalf("hook context = %q", got)
	}
	request := dagent.ModelRequest{State: update}
	_, err = middleware.WrapModelCall(t.Context(), request, func(_ context.Context, got dagent.ModelRequest) (dagent.ModelResponse, error) {
		if got.SystemMessage == nil || !strings.Contains(got.SystemMessage.TextContent(), "from-SessionStart") {
			t.Fatalf("system context = %#v", got.SystemMessage)
		}
		return dagent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	toolRequest := dagent.ToolCallRequest{Call: damessage.ToolCall{ID: "call", Name: "read_file", Arguments: json.RawMessage(`{"path":"README.md"}`)}, Runtime: runtimeContext}
	response, err := middleware.WrapToolCall(t.Context(), toolRequest, func(context.Context, dagent.ToolCallRequest) (dagent.ToolCallResponse, error) {
		return dagent.ToolCallResponse{Result: datool.TextResult("ok")}, nil
	})
	if err != nil || response.Result.Update[hookContextStateKey] == nil {
		t.Fatalf("successful tool hooks = %#v, %v", response, err)
	}
	_, callErr := middleware.WrapToolCall(t.Context(), toolRequest, func(context.Context, dagent.ToolCallRequest) (dagent.ToolCallResponse, error) {
		return dagent.ToolCallResponse{}, errors.New("expected tool failure")
	})
	if callErr == nil || callErr.Error() != "expected tool failure" {
		t.Fatalf("tool failure = %v", callErr)
	}
	values[dagent.MessagesKey] = []damessage.Message{damessage.Assistant("done")}
	if _, err := middleware.AfterAgent(t.Context(), values, runtimeContext); err != nil {
		t.Fatal(err)
	}
	if err := hooks.Close(); err != nil {
		t.Fatal(err)
	}
	recorded, err := os.ReadFile(filepath.Join(data, "events"))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []dahook.Event{dahook.SessionStart, dahook.UserPromptSubmit, dahook.PreToolUse, dahook.PostToolUse, dahook.PostToolUseFailure, dahook.Stop, dahook.SessionEnd} {
		if !strings.Contains(string(recorded), string(event)+"\n") {
			t.Fatalf("event %s missing from %q", event, recorded)
		}
	}
}

func TestInstalledPluginHookPublishesConfiguredStatus(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	handler := map[string]any{
		"type": "command", "argv": []string{executable, "-test.run=TestDacodePluginHookProcess", "--", "plugin-hook-helper"},
		"statusMessage": "Checking installed plugin",
	}
	manifest, err := json.Marshal(map[string]any{
		"name": "review", "version": "1",
		"hooks": map[string]any{"UserPromptSubmit": []any{map[string]any{"hooks": []any{handler}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	storeRoot, project := t.TempDir(), t.TempDir()
	installRuntimePlugin(t, storeRoot, string(manifest))
	components, err := loadRuntimePlugins(t.Context(), storeRoot, project, os.LookupEnv)
	if err != nil || len(components.Hooks) != 1 {
		t.Fatalf("components = %#v, %v", components, err)
	}
	var progress []dahook.Progress
	hooks, err := newDacodeHookRuntime(t.Context(), project, components.Hooks, dacodeHookRuntimeOptions{
		Headless: true, UserConfigDir: t.TempDir(), OnProgress: func(update dahook.Progress) { progress = append(progress, update) },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = hooks.Middleware().BeforeAgent(t.Context(), dastate.Values{
		dagent.MessagesKey: []damessage.Message{damessage.Human("hello")},
	}, dagent.Runtime{Config: dacheckpoint.Config{ThreadID: "thread"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 2 || !progress[0].Active || progress[0].Message != "Checking installed plugin" || progress[1].Active {
		t.Fatalf("progress = %#v", progress)
	}
}

func TestDacodeHookRuntimeCancellationDoesNotExhaustAuthenticatedExchanges(t *testing.T) {
	project := t.TempDir()
	hooks, err := newDacodeHookRuntime(t.Context(), project, nil, dacodeHookRuntimeOptions{
		Headless: true, UserConfigDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	invocation := dahook.Invocation{
		Event: dahook.PreToolUse, SessionID: "thread", CWD: project,
		Data: map[string]any{"tool_name": "read_file", "tool_input": map[string]any{"path": "README.md"}},
	}
	for index := 0; index < 1100; index++ {
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := hooks.run(cancelled, invocation); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled exchange %d = %v", index, err)
		}
	}
	decision, err := hooks.run(t.Context(), invocation)
	if err != nil || !decision.Continue {
		t.Fatalf("valid exchange after cancellations = %#v, %v", decision, err)
	}
}

func TestDacodePluginHookProcess(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "plugin-hook-helper" {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		os.Exit(2)
	}
	var invocation map[string]any
	if json.Unmarshal(raw, &invocation) != nil {
		os.Exit(2)
	}
	event, _ := invocation["hook_event_name"].(string)
	file, err := os.OpenFile(filepath.Join(os.Getenv("PLUGIN_DATA"), "events"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	_, _ = fmt.Fprintln(file, event)
	_ = file.Close()
	_, _ = fmt.Fprintf(os.Stdout, `{"additionalContext":%q}`, "from-"+event)
	os.Exit(0)
}

func hookTestDocument(t *testing.T, executable string, events []dahook.Event) json.RawMessage {
	t.Helper()
	hooks := map[string]any{}
	for _, event := range events {
		command := map[string]any{
			"type": "command", "argv": []string{executable, "-test.run=TestDacodePluginHookProcess", "--", "plugin-hook-helper"},
		}
		hooks[string(event)] = []any{map[string]any{"hooks": []any{command}}}
	}
	raw, err := json.Marshal(map[string]any{"hooks": hooks})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func installRuntimePlugin(t *testing.T, storeRoot, manifest string) daplugin.Plugin {
	t.Helper()
	catalog := t.TempDir()
	root := filepath.Join(catalog, "plugins", "review")
	writePluginCLIFile(t, root, "plugin.json", manifest)
	writePluginCLIFile(t, catalog, ".agents/plugins/marketplace.json", `{"name":"market","plugins":[{"name":"review","source":"./plugins/review"}]}`)
	manager := daplugin.NewManager(daplugin.NewStore(storeRoot, daplugin.StoreOptions{}), nil, daplugin.ManagerOptions{})
	if _, err := manager.AddMarketplace(t.Context(), daplugin.MarketplaceSource{Type: daplugin.SourceDirectory, Value: catalog}); err != nil {
		t.Fatal(err)
	}
	plugin, err := manager.Install(t.Context(), "review@market")
	if err != nil {
		t.Fatal(err)
	}
	return plugin
}

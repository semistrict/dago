package dahook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEventRegistryCoversLifecycle(t *testing.T) {
	client := []Event{SessionStart, UserPromptSubmit, SessionEnd, PermissionRequest, Notification}
	server := []Event{PreToolUse, PostToolUse, PostToolUseFailure, PreCompact, Stop, SubagentStart, SubagentStop}
	for _, event := range client {
		if EventOwner(event) != ClientOwner {
			t.Fatalf("%s owner", event)
		}
	}
	for _, event := range server {
		if EventOwner(event) != ServerOwner {
			t.Fatalf("%s owner", event)
		}
	}
	if len(client)+len(server) != len(allEvents) {
		t.Fatal("event registry incomplete")
	}
}

func TestEveryLifecycleEventProducesCompatibleEnvelope(t *testing.T) {
	for _, event := range []Event{SessionStart, UserPromptSubmit, SessionEnd, PermissionRequest, Notification, PreToolUse, PostToolUse, PostToolUseFailure, PreCompact, Stop, SubagentStart, SubagentStop} {
		invocation := validLifecycleInvocation(t, event)
		raw, err := json.Marshal(invocation)
		if err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		var envelope map[string]any
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope["hook_event_name"] != string(event) || envelope["session_id"] != "thread" || envelope["cwd"] == "" {
			t.Fatalf("%s envelope = %#v", event, envelope)
		}
	}
}

func TestLoadPrecedenceTrustPluginsAndHeadless(t *testing.T) {
	root := t.TempDir()
	user := t.TempDir()
	plugin := filepath.Join(t.TempDir(), "hooks.json")
	writeConfig(t, filepath.Join(root, ".deepagents", "hooks.json"), "project")
	writeConfig(t, filepath.Join(user, "hooks.json"), "user")
	writeConfig(t, plugin, "${PLUGIN_ROOT}/plugin")
	store := filepath.Join(t.TempDir(), "state", "hooks_trust.json")
	if err := TrustProject(t.Context(), store, root); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(t.Context(), root, LoadOptions{UserConfigDir: user, TrustStore: store, Plugins: []Plugin{{ID: "p", Path: plugin, Enabled: true, Environment: map[string]string{"PLUGIN_ROOT": "/safe"}}}})
	if err != nil {
		t.Fatal(err)
	}
	commands := []string{}
	for _, handler := range loaded.Handlers[SessionStart] {
		commands = append(commands, handler.Command)
	}
	if got, want := strings.Join(commands, ","), "project,user,${PLUGIN_ROOT}/plugin"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	headless, err := Load(t.Context(), root, LoadOptions{UserConfigDir: user, TrustStore: store, Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(headless.Handlers[SessionStart]); got != 1 {
		t.Fatalf("headless project hooks loaded: %d", got)
	}
	explicit, err := Load(t.Context(), root, LoadOptions{UserConfigDir: user, Headless: true, TrustProject: true})
	if err != nil || len(explicit.Handlers[SessionStart]) != 2 {
		t.Fatalf("explicit headless trust: %v %#v", err, explicit.Handlers)
	}
	if loaded.ID == "" {
		t.Fatal("empty snapshot id")
	}
}

func TestLoadInlinePluginHooksAndIsolatesMalformedPlugin(t *testing.T) {
	root, user := t.TempDir(), t.TempDir()
	loaded, err := Load(t.Context(), root, LoadOptions{UserConfigDir: user, Plugins: []Plugin{
		{ID: "good", Enabled: true, Inline: json.RawMessage(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","argv":["helper"]}]}]}}`)},
		{ID: "bad", Enabled: true, Inline: json.RawMessage(`{"hooks":`)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Handlers[SessionStart]) != 1 || len(loaded.Diagnostics) != 1 || loaded.Diagnostics[0].Code != "plugin_hooks_failed" {
		t.Fatalf("loaded = %#v", loaded)
	}
	if _, err := Load(t.Context(), root, LoadOptions{UserConfigDir: user, Plugins: []Plugin{{ID: "ambiguous", Enabled: true}}}); err == nil {
		t.Fatal("ambiguous plugin hook source accepted")
	}
}

func TestEngineRunsAllHandlersAndReducesInPrecedence(t *testing.T) {
	executable, _ := os.Executable()
	barrier := t.TempDir()
	engine := NewEngine(Snapshot{ID: "snap", Handlers: map[Event][]Handler{UserPromptSubmit: {
		{ID: "project", Event: UserPromptSubmit, Argv: []string{executable, "-test.run=TestHookProcess", "--", "barrier", barrier, "project", "block"}, Scope: ProjectScope, order: 0},
		{ID: "user", Event: UserPromptSubmit, Argv: []string{executable, "-test.run=TestHookProcess", "--", "barrier", barrier, "user", "context"}, Scope: UserScope, order: 1},
	}}}, EngineOptions{Environment: map[string]string{"GO_HOOK_HELPER": "1"}})
	decision, err := engine.Run(t.Context(), Invocation{Event: UserPromptSubmit, SessionID: "s", CWD: t.TempDir(), Data: map[string]any{"prompt": "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Continue || decision.StopReason != "project blocked" {
		t.Fatalf("decision = %#v", decision)
	}
	if got := strings.Join(decision.AdditionalContext, ","); got != "user context" {
		t.Fatalf("context = %q", got)
	}
}

func TestEngineReportsBoundedProgressAndIsolatesPresenterPanics(t *testing.T) {
	executable, _ := os.Executable()
	updates := make(chan Progress, 2)
	handler := Handler{
		ID: "status", Event: SessionStart,
		Argv:          []string{executable, "-test.run=TestHookProcess", "--", "context"},
		StatusMessage: "  checking\nworkspace\x00  ",
	}
	engine := NewEngine(Snapshot{ID: "s", Handlers: map[Event][]Handler{SessionStart: {handler}}}, EngineOptions{
		Environment: map[string]string{"GO_HOOK_HELPER": "1"}, OnProgress: func(progress Progress) { updates <- progress },
	})
	if _, err := engine.Run(t.Context(), validLifecycleInvocation(t, SessionStart)); err != nil {
		t.Fatal(err)
	}
	started, stopped := <-updates, <-updates
	if !started.Active || stopped.Active || started.OperationID == "" || stopped.OperationID != started.OperationID || started.Message != "checking workspace" || stopped.Message != started.Message {
		t.Fatalf("progress = %#v then %#v", started, stopped)
	}
	panicEngine := NewEngine(Snapshot{ID: "s", Handlers: map[Event][]Handler{SessionStart: {handler}}}, EngineOptions{
		Environment: map[string]string{"GO_HOOK_HELPER": "1"}, OnProgress: func(Progress) { panic("presenter failure") },
	})
	if _, err := panicEngine.Run(t.Context(), validLifecycleInvocation(t, SessionStart)); err != nil {
		t.Fatalf("presenter panic escaped: %v", err)
	}
}

func TestEngineMatchesBoundsSecretsAndCancellation(t *testing.T) {
	executable, _ := os.Executable()
	base := Handler{ID: "hook", Event: PreToolUse, Matcher: "Bash|Read", Argv: []string{executable, "-test.run=TestHookProcess", "--", "inspect"}, order: 0}
	engine := NewEngine(Snapshot{ID: "s", Handlers: map[Event][]Handler{PreToolUse: {base}}}, EngineOptions{MaxOutputBytes: 128, Environment: map[string]string{"GO_HOOK_HELPER": "1", "PATH": os.Getenv("PATH"), "API_TOKEN": "must-not-leak"}})
	decision, err := engine.Run(t.Context(), Invocation{Event: PreToolUse, SessionID: "s", CWD: t.TempDir(), Data: map[string]any{"tool_name": "Bash", "tool_input": map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Permission != PermissionAllow {
		t.Fatalf("decision = %#v", decision)
	}
	if strings.Contains(fmt.Sprint(decision), "must-not-leak") {
		t.Fatal("secret leaked")
	}
	base.Argv = []string{executable, "-test.run=TestHookProcess", "--", "sleep"}
	base.Timeout = 25 * time.Millisecond
	engine = NewEngine(Snapshot{ID: "s", Handlers: map[Event][]Handler{PreToolUse: {base}}}, EngineOptions{Environment: map[string]string{"GO_HOOK_HELPER": "1"}})
	decision, err = engine.Run(t.Context(), Invocation{Event: PreToolUse, SessionID: "s", CWD: t.TempDir(), Data: map[string]any{"tool_name": "Bash", "tool_input": map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.Diagnostics) == 0 || decision.Diagnostics[0].Code != "timeout" {
		t.Fatalf("timeout diagnostic = %#v", decision.Diagnostics)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = engine.Run(ctx, Invocation{Event: PreToolUse, SessionID: "s", CWD: t.TempDir(), Data: map[string]any{"tool_name": "Bash", "tool_input": map[string]any{}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestEngineTruncatesOutputAndStopContinuationIsBounded(t *testing.T) {
	executable, _ := os.Executable()
	engine := NewEngine(Snapshot{ID: "s", Handlers: map[Event][]Handler{SessionStart: {{ID: "large", Event: SessionStart, Argv: []string{executable, "-test.run=TestHookProcess", "--", "large"}}}}}, EngineOptions{MaxOutputBytes: 64, Environment: map[string]string{"GO_HOOK_HELPER": "1"}})
	decision, err := engine.Run(t.Context(), validLifecycleInvocation(t, SessionStart))
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.AdditionalContext) != 1 || len(decision.AdditionalContext[0]) != 64 {
		t.Fatalf("bounded context = %#v", decision.AdditionalContext)
	}
	if len(decision.Diagnostics) != 1 || decision.Diagnostics[0].Code != "stdout_truncated" {
		t.Fatalf("diagnostics = %#v", decision.Diagnostics)
	}
	stopEngine := NewEngine(Snapshot{ID: "s", Handlers: map[Event][]Handler{Stop: {{ID: "stop", Event: Stop, Argv: []string{executable, "-test.run=TestHookProcess", "--", "stop-context"}}}}}, EngineOptions{Environment: map[string]string{"GO_HOOK_HELPER": "1"}})
	invocation := validLifecycleInvocation(t, Stop)
	invocation.Data["continuation_count"] = 8
	decision, err = stopEngine.Run(t.Context(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ContinueLoop || len(decision.Diagnostics) != 1 || decision.Diagnostics[0].Code != "continuation_cap" {
		t.Fatalf("stop decision = %#v", decision)
	}
}

func TestMigratedHandlerPreservesInvocationWorkingDirectory(t *testing.T) {
	executable, _ := os.Executable()
	workingDirectory := t.TempDir()
	engine := NewEngine(Snapshot{ID: "s", Handlers: map[Event][]Handler{UserPromptSubmit: {{
		ID: "legacy", Event: UserPromptSubmit, LegacyEvent: "session.start",
		Argv: []string{executable, "-test.run=TestHookProcess", "--", "cwd"},
	}}}}, EngineOptions{Environment: map[string]string{"GO_HOOK_HELPER": "1"}})
	decision, err := engine.Run(t.Context(), Invocation{Event: UserPromptSubmit, SessionID: "s", CWD: workingDirectory, Data: map[string]any{"prompt": "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.AdditionalContext) != 1 {
		t.Fatalf("legacy context = %#v", decision.AdditionalContext)
	}
	got, err := filepath.EvalSymlinks(decision.AdditionalContext[0])
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(workingDirectory)
	if !strings.EqualFold(filepath.Clean(got), filepath.Clean(want)) {
		t.Fatalf("legacy cwd = %q, want %q", got, want)
	}
}

func TestHookCancellationTerminatesDescendantsHoldingPipes(t *testing.T) {
	executable, _ := os.Executable()
	sentinel := filepath.Join(t.TempDir(), "descendant-survived")
	engine := NewEngine(Snapshot{ID: "s", Handlers: map[Event][]Handler{PreToolUse: {{
		ID: "tree", Event: PreToolUse, Timeout: 75 * time.Millisecond,
		Argv: []string{executable, "-test.run=TestHookProcess", "--", "spawn-tree", sentinel},
	}}}}, EngineOptions{Environment: map[string]string{"GO_HOOK_HELPER": "1"}})
	started := time.Now()
	decision, err := engine.Run(t.Context(), Invocation{Event: PreToolUse, SessionID: "s", CWD: t.TempDir(), Data: map[string]any{"tool_name": "Bash", "tool_input": map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("process-tree cancellation took %v", elapsed)
	}
	if len(decision.Diagnostics) == 0 || decision.Diagnostics[0].Code != "timeout" {
		t.Fatalf("timeout diagnostic = %#v", decision.Diagnostics)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("hook descendant survived cancellation: %v", err)
	}
}

func TestPermissionRequestSpecificOutput(t *testing.T) {
	executable, _ := os.Executable()
	engine := NewEngine(Snapshot{ID: "s", Handlers: map[Event][]Handler{PermissionRequest: {{ID: "p", Event: PermissionRequest, Argv: []string{executable, "-test.run=TestHookProcess", "--", "permission-specific"}}}}}, EngineOptions{Environment: map[string]string{"GO_HOOK_HELPER": "1"}})
	decision, err := engine.Run(t.Context(), validLifecycleInvocation(t, PermissionRequest))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Permission != PermissionDeny || decision.PermissionReason != "operator policy" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestLegacyMigrationAndInvalidConfig(t *testing.T) {
	legacy := []byte(`{"hooks":[{"command":["echo","ok"],"events":["session.start","tool.use"]}]}`)
	order := 0
	handlers, diagnostics, err := parseDocument(legacy, UserScope, nil, &order)
	if err != nil {
		t.Fatal(err)
	}
	if len(handlers) != 1 || handlers[0].Event != UserPromptSubmit || len(diagnostics) != 1 {
		t.Fatalf("migration = %#v %#v", handlers, diagnostics)
	}
	invalid := []byte(`{"hooks":{"Unknown":[{"hooks":[{"type":"command","command":"x"}]}]}}`)
	if _, _, err := parseDocument(invalid, UserScope, nil, &order); err == nil {
		t.Fatal("invalid event accepted")
	}
}

func TestTrustStoreIsPrivateCanonicalAndFailsClosed(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(t.TempDir(), "state", "hooks_trust.json")
	if err := TrustProject(t.Context(), store, filepath.Join(root, ".")); err != nil {
		t.Fatal(err)
	}
	trusted, err := IsTrusted(t.Context(), store, root)
	if err != nil || !trusted {
		t.Fatalf("trusted=%v err=%v", trusted, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(store)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode=%o", info.Mode().Perm())
		}
	}
	if err := os.WriteFile(store, []byte(`{"version":99,"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := IsTrusted(t.Context(), store, root); err == nil {
		t.Fatal("unsupported trust version accepted")
	}
}

func TestConcurrentTrustUpdatesPreserveProjects(t *testing.T) {
	store := filepath.Join(t.TempDir(), "state", "hooks_trust.json")
	roots := []string{t.TempDir(), t.TempDir()}
	errorsChannel := make(chan error, 2)
	for _, root := range roots {
		go func() { errorsChannel <- TrustProject(t.Context(), store, root) }()
	}
	for range roots {
		if err := <-errorsChannel; err != nil {
			t.Fatal(err)
		}
	}
	for _, root := range roots {
		trusted, err := IsTrusted(t.Context(), store, root)
		if err != nil || !trusted {
			t.Fatalf("root %q trusted=%v err=%v", root, trusted, err)
		}
	}
}

func TestTranscriptConfinesRedactsBoundsAndCancels(t *testing.T) {
	root := t.TempDir()
	store := NewTranscriptStore(root, TranscriptStoreOptions{MaxBytes: 1024})
	records := []TranscriptRecord{{Role: "user", Content: json.RawMessage(`{"token":"sk-secretsecretsecret","text":"API_KEY=visible"}`)}}
	handle, err := store.Materialize(t.Context(), "../../thread", "../agent", records)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, _ := filepath.EvalSymlinks(root)
	if !strings.HasPrefix(handle.Path, canonicalRoot+string(os.PathSeparator)) {
		t.Fatalf("escaped path %q", handle.Path)
	}
	raw, _ := os.ReadFile(handle.Path)
	if strings.Contains(string(raw), "secretsecret") || strings.Contains(string(raw), "visible") {
		t.Fatalf("secret persisted: %s", raw)
	}
	large := []TranscriptRecord{{Role: "user", Content: json.RawMessage(`"` + strings.Repeat("x", 2048) + `"`)}}
	if _, err := store.Materialize(t.Context(), "t", "", large); err == nil {
		t.Fatal("oversized transcript accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.Materialize(ctx, "t", "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestTranscriptRedactsHeadersCredentialURLsAndPrivateKeys(t *testing.T) {
	privateKey := "-----BEGIN PRIVATE " + "KEY-----\nprivate-material\n-----END PRIVATE " + "KEY-----"
	content, err := json.Marshal(map[string]any{
		"headers": map[string]any{
			"Authorization": "Basic authorization-secret",
			"Cookie":        "session=cookie-secret",
			"X-Safe":        "retained",
		},
		"url": "https://operator:" + "url-password@example.test/path?access_token=query-secret&safe=retained",
		"pem": privateKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewTranscriptStore(t.TempDir(), TranscriptStoreOptions{})
	handle, err := store.Materialize(t.Context(), "thread", "", []TranscriptRecord{{Role: "user", Content: content}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(handle.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"authorization-secret", "cookie-secret", "url-password", "query-secret", "private-material"} {
		if strings.Contains(text, secret) {
			t.Fatalf("transcript leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "retained") || !strings.Contains(text, "REDACTED") {
		t.Fatalf("redacted transcript lost safe structure: %s", text)
	}
}

func TestTranscriptRedactsMixedCaseURLsAndRawHeaderLines(t *testing.T) {
	content, err := json.Marshal(strings.Join([]string{
		"AUTHORIZATION: Basic raw-authorization-secret",
		"Cookie: session=raw-cookie-secret",
		"Set-Cookie: refresh=raw-set-cookie-secret; Secure",
		"X-API-Key: raw-api-key-secret",
		"X-Custom-Token: raw-token-secret",
		"X-Safe: retained-header",
		"Fetch HtTpS://operator:url-secret@example.test/path?API_KEY=query-secret&safe=retained-query",
	}, "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewTranscriptStore(t.TempDir(), TranscriptStoreOptions{})
	handle, err := store.Materialize(t.Context(), "thread", "", []TranscriptRecord{{Role: "user", Content: content}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(handle.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{
		"raw-authorization-secret", "raw-cookie-secret", "raw-set-cookie-secret",
		"raw-api-key-secret", "raw-token-secret", "url-secret", "query-secret",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("transcript leaked %q: %s", secret, text)
		}
	}
	for _, retained := range []string{"retained-header", "retained-query", "X-Safe"} {
		if !strings.Contains(text, retained) {
			t.Fatalf("transcript lost %q: %s", retained, text)
		}
	}
}

func TestTranscriptRedactsStructuredHeaderPairs(t *testing.T) {
	content, err := json.Marshal(map[string]any{
		"headers": []any{
			map[string]any{"name": "Cookie", "value": "structured-cookie-secret"},
			map[string]any{"Name": "X-API-Key", "Value": "structured-api-secret"},
			map[string]any{"header": "Authorization", "value": "structured-auth-secret"},
			map[string]any{"name": "X-Safe", "value": "retained-object"},
			[]any{"Set-Cookie", "structured-array-secret"},
			[]any{"X-Safe", "retained-array"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewTranscriptStore(t.TempDir(), TranscriptStoreOptions{})
	handle, err := store.Materialize(t.Context(), "thread", "", []TranscriptRecord{{Role: "user", Content: content}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(handle.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"structured-cookie-secret", "structured-api-secret", "structured-auth-secret", "structured-array-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("transcript leaked %q: %s", secret, text)
		}
	}
	for _, retained := range []string{"retained-object", "retained-array", "X-Safe"} {
		if !strings.Contains(text, retained) {
			t.Fatalf("transcript lost %q: %s", retained, text)
		}
	}
}

func TestInterruptFulfillmentLedgerRejectsReplayAndMismatch(t *testing.T) {
	capability := testCapability()
	engine := NewEngine(Snapshot{ID: "snapshot", Handlers: map[Event][]Handler{}}, EngineOptions{})
	ledger := NewLedger(2)
	server := NewServer("snapshot", ledger, capability)
	interrupt, err := server.Interrupt("run", Invocation{Event: PreCompact, SessionID: "s", CWD: t.TempDir(), Data: map[string]any{"trigger": "manual"}}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request := interrupt.Request
	fulfiller := NewFulfiller(engine, capability, 2)
	response, err := fulfiller.Fulfill(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Resume(response); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Resume(response); err == nil {
		t.Fatal("replay accepted")
	}
	interrupt, err = server.Interrupt("run", Invocation{Event: Stop, SessionID: "s", CWD: t.TempDir(), Data: map[string]any{"last_assistant_message": "done"}}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request = interrupt.Request
	response = InvocationResponse{ProtocolVersion: 1, InvocationID: request.InvocationID, SnapshotID: "other"}
	if _, err := server.Resume(response); err == nil {
		t.Fatal("snapshot mismatch accepted")
	}
}

func TestResponseCapabilityBindsCompleteDecisionBeforeConsumption(t *testing.T) {
	capability := testCapability()
	server := NewServer("snapshot", NewLedger(4), capability)
	interrupt, err := server.Interrupt("run", validLifecycleInvocation(t, PreCompact), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	decision := Decision{
		Continue: true, StopReason: "stop", Permission: PermissionAllow,
		PermissionReason: "approved", AdditionalContext: []string{"context"},
		SystemMessages: []string{"system"}, SuppressOriginalPrompt: true,
		ContinueLoop: true, Diagnostics: []Diagnostic{{HandlerID: "handler", Code: "code", Message: "message"}},
	}
	response := InvocationResponse{ProtocolVersion: 1, InvocationID: interrupt.Request.InvocationID, SnapshotID: interrupt.Request.SnapshotID, Decision: decision}
	response.CapabilityMAC = capability.signResponse(interrupt.Request, response)
	mutations := map[string]func(*InvocationResponse){
		"protocol":           func(value *InvocationResponse) { value.ProtocolVersion++ },
		"id":                 func(value *InvocationResponse) { value.InvocationID += "x" },
		"snapshot":           func(value *InvocationResponse) { value.SnapshotID += "x" },
		"continue":           func(value *InvocationResponse) { value.Decision.Continue = false },
		"stop reason":        func(value *InvocationResponse) { value.Decision.StopReason += "x" },
		"permission":         func(value *InvocationResponse) { value.Decision.Permission = PermissionDeny },
		"permission reason":  func(value *InvocationResponse) { value.Decision.PermissionReason += "x" },
		"additional context": func(value *InvocationResponse) { value.Decision.AdditionalContext[0] += "x" },
		"system messages":    func(value *InvocationResponse) { value.Decision.SystemMessages[0] += "x" },
		"suppress prompt":    func(value *InvocationResponse) { value.Decision.SuppressOriginalPrompt = false },
		"continue loop":      func(value *InvocationResponse) { value.Decision.ContinueLoop = false },
		"diagnostics":        func(value *InvocationResponse) { value.Decision.Diagnostics[0].Message += "x" },
		"mac":                func(value *InvocationResponse) { value.CapabilityMAC = strings.Repeat("0", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			tampered := cloneResponse(t, response)
			mutate(&tampered)
			if _, err := server.Resume(tampered); err == nil {
				t.Fatal("tampered response accepted")
			}
		})
	}
	got, err := server.Resume(response)
	if err != nil {
		t.Fatalf("valid response rejected after tampering attempts: %v", err)
	}
	if !reflect.DeepEqual(got, decision) {
		t.Fatalf("decision = %#v, want %#v", got, decision)
	}
	if _, err := server.Resume(response); err == nil {
		t.Fatal("authenticated response replay accepted")
	}
}

func TestInterruptCapabilityBindsEveryFieldBeforeSideEffects(t *testing.T) {
	capability := testCapability()
	executable, _ := os.Executable()
	marker := filepath.Join(t.TempDir(), "effects")
	engine := NewEngine(Snapshot{ID: "snapshot", Handlers: map[Event][]Handler{PreCompact: {{
		ID: "effect", Event: PreCompact,
		Argv: []string{executable, "-test.run=TestHookProcess", "--", "touch", marker},
	}}}}, EngineOptions{Environment: map[string]string{"GO_HOOK_HELPER": "1"}})
	server := NewServer("snapshot", NewLedger(4), capability)
	interrupt, err := server.Interrupt("run", validLifecycleInvocation(t, PreCompact), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	base := interrupt.Request
	mutations := map[string]func(*InvocationRequest){
		"protocol": func(request *InvocationRequest) { request.ProtocolVersion++ },
		"id":       func(request *InvocationRequest) { request.InvocationID += "x" },
		"snapshot": func(request *InvocationRequest) { request.SnapshotID += "x" },
		"run":      func(request *InvocationRequest) { request.RunID += "x" },
		"event": func(request *InvocationRequest) {
			request.Invocation.Event = Stop
			request.Invocation.Data = map[string]any{"last_assistant_message": "done"}
		},
		"session":    func(request *InvocationRequest) { request.Invocation.SessionID += "x" },
		"cwd":        func(request *InvocationRequest) { request.Invocation.CWD = t.TempDir() },
		"event data": func(request *InvocationRequest) { request.Invocation.Data["trigger"] = "auto" },
		"deadline":   func(request *InvocationRequest) { request.Deadline = request.Deadline.Add(time.Second) },
		"mac":        func(request *InvocationRequest) { request.CapabilityMAC = strings.Repeat("0", 64) },
	}
	fulfiller := NewFulfiller(engine, capability, 4)
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			request := cloneRequest(t, base)
			mutate(&request)
			if _, err := fulfiller.Fulfill(t.Context(), request); err == nil {
				t.Fatal("tampered request executed")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("tampered request caused a side effect: %v", err)
			}
		})
	}
	wrong := NewFulfiller(engine, NewCapability([]byte("another capability key material!!")), 4)
	if _, err := wrong.Fulfill(t.Context(), base); err == nil {
		t.Fatal("request authenticated under the wrong capability")
	}
	if _, err := fulfiller.Fulfill(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	if _, err := fulfiller.Fulfill(t.Context(), base); err == nil {
		t.Fatal("consumed invocation replayed")
	}
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "x" {
		t.Fatalf("effects = %q, err=%v", raw, err)
	}
}

func TestClientInvocationLedgerClaimsConcurrentRequestOnce(t *testing.T) {
	capability := testCapability()
	executable, _ := os.Executable()
	marker := filepath.Join(t.TempDir(), "effects")
	engine := NewEngine(Snapshot{ID: "snapshot", Handlers: map[Event][]Handler{PreCompact: {{
		ID: "effect", Event: PreCompact,
		Argv: []string{executable, "-test.run=TestHookProcess", "--", "delayed-touch", marker},
	}}}}, EngineOptions{Environment: map[string]string{"GO_HOOK_HELPER": "1"}})
	server := NewServer("snapshot", NewLedger(2), capability)
	interrupt, err := server.Interrupt("run", validLifecycleInvocation(t, PreCompact), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	fulfiller := NewFulfiller(engine, capability, 2)
	start := make(chan struct{})
	errorsChannel := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := fulfiller.Fulfill(t.Context(), interrupt.Request)
			errorsChannel <- err
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-errorsChannel; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful fulfillments = %d, want 1", successes)
	}
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "x" {
		t.Fatalf("effects = %q, err=%v", raw, err)
	}
}

func TestLedgerNeverEvictsPendingInvocation(t *testing.T) {
	capability := testCapability()
	ledger := NewLedger(1)
	server := NewServer("snapshot", ledger, capability)
	firstInterrupt, err := server.Interrupt("run", validLifecycleInvocation(t, PreCompact), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Interrupt("run", validLifecycleInvocation(t, Stop), time.Now().Add(time.Minute)); err == nil {
		t.Fatal("pending invocation was silently evicted")
	}
	first := firstInterrupt.Request
	response := InvocationResponse{ProtocolVersion: 1, InvocationID: first.InvocationID, SnapshotID: first.SnapshotID, Decision: Decision{Continue: true}}
	response.CapabilityMAC = capability.signResponse(first, response)
	if _, err := server.Resume(response); err != nil {
		t.Fatalf("first response after capacity rejection: %v", err)
	}
}

func TestClientLedgerRetainsRecentReplayWithoutLifetimeExhaustion(t *testing.T) {
	capability := testCapability()
	server := NewServer("snapshot", NewLedger(8), capability)
	engine := NewEngine(Snapshot{ID: "snapshot", Handlers: map[Event][]Handler{}}, EngineOptions{})
	fulfiller := NewFulfiller(engine, capability, 8)
	invocation := Invocation{Event: PreCompact, SessionID: "session", CWD: t.TempDir(), Data: map[string]any{"trigger": "manual"}}
	var recentRequest InvocationRequest
	for index := 0; index < 2048; index++ {
		interrupt, err := server.Interrupt(fmt.Sprintf("run-%d", index), invocation, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatalf("interrupt %d: %v", index, err)
		}
		response, err := fulfiller.Fulfill(t.Context(), interrupt.Request)
		if err != nil {
			t.Fatalf("fulfill %d: %v", index, err)
		}
		if _, err := server.Resume(response); err != nil {
			t.Fatalf("resume %d: %v", index, err)
		}
		recentRequest = interrupt.Request
	}
	if _, err := fulfiller.Fulfill(t.Context(), recentRequest); err == nil {
		t.Fatal("recent consumed invocation replayed")
	}
	fulfiller.ledger.mu.Lock()
	defer fulfiller.ledger.mu.Unlock()
	if len(fulfiller.ledger.states) > 8 || len(fulfiller.ledger.consumedOrder) > 8 {
		t.Fatalf("client replay ledger grew beyond capacity: states=%d order=%d", len(fulfiller.ledger.states), len(fulfiller.ledger.consumedOrder))
	}
}

func TestInterruptJSONRoundTripRetainsFlattenedEventData(t *testing.T) {
	server := NewServer("snapshot", NewLedger(2), testCapability())
	interrupt, err := server.Interrupt("run", Invocation{Event: PreToolUse, SessionID: "thread", CWD: "/workspace", Data: map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "pwd"}}}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request := interrupt.Request
	raw, err := json.Marshal(BuildInterrupt(request))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"hook_event_name":"PreToolUse"`) || strings.Contains(string(raw), `"Data"`) {
		t.Fatalf("wire payload = %s", raw)
	}
	var decoded Interrupt
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Request.Invocation.Event != PreToolUse || decoded.Request.Invocation.Data["tool_name"] != "Bash" {
		t.Fatalf("decoded = %#v", decoded)
	}
}

func TestTranscriptCanonicalizesLinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	realRoot := t.TempDir()
	linkedRoot := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	store := NewTranscriptStore(linkedRoot, TranscriptStoreOptions{})
	handle, err := store.Materialize(t.Context(), "thread", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, _ := filepath.EvalSymlinks(realRoot)
	if !strings.HasPrefix(handle.Path, canonicalRoot+string(os.PathSeparator)) {
		t.Fatalf("path = %q, want canonical root %q", handle.Path, canonicalRoot)
	}
}

func TestHookProcess(t *testing.T) {
	if os.Getenv("GO_HOOK_HELPER") != "1" {
		return
	}
	var arguments []string
	for index, arg := range os.Args {
		if arg == "--" && index+1 < len(os.Args) {
			arguments = os.Args[index+1:]
		}
	}
	mode := ""
	if len(arguments) > 0 {
		mode = arguments[0]
	}
	var input map[string]any
	_ = json.NewDecoder(os.Stdin).Decode(&input)
	switch mode {
	case "block":
		fmt.Fprintln(os.Stderr, "project blocked")
		os.Exit(2)
	case "context":
		fmt.Print("user context")
	case "inspect":
		if os.Getenv("API_TOKEN") != "" {
			fmt.Print(`{"permissionDecision":"deny"}`)
		} else {
			fmt.Print(`{"permissionDecision":"allow"}`)
		}
	case "sleep":
		time.Sleep(time.Second)
	case "large":
		fmt.Print(strings.Repeat("x", 1024))
	case "stop-context":
		fmt.Print(`{"hookSpecificOutput":{"hookEventName":"Stop","additionalContext":"continue"}}`)
	case "permission-specific":
		fmt.Print(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"operator policy"}}}`)
	case "cwd":
		workingDirectory, err := os.Getwd()
		if err != nil {
			os.Exit(3)
		}
		fmt.Print(workingDirectory)
	case "touch":
		if len(arguments) != 2 || os.WriteFile(arguments[1], []byte("x"), 0o600) != nil {
			os.Exit(3)
		}
	case "delayed-touch":
		time.Sleep(100 * time.Millisecond)
		if len(arguments) != 2 || os.WriteFile(arguments[1], []byte("x"), 0o600) != nil {
			os.Exit(3)
		}
	case "spawn-tree":
		if len(arguments) != 2 {
			os.Exit(3)
		}
		child := exec.Command(os.Args[0], "-test.run=TestHookProcess", "--", "delayed-write", arguments[1])
		child.Env = os.Environ()
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if child.Start() != nil {
			os.Exit(3)
		}
		time.Sleep(10 * time.Second)
	case "delayed-write":
		time.Sleep(400 * time.Millisecond)
		if len(arguments) != 2 || os.WriteFile(arguments[1], []byte("survived"), 0o600) != nil {
			os.Exit(3)
		}
	case "barrier":
		if len(arguments) != 4 {
			os.Exit(3)
		}
		directory, participant, output := arguments[1], arguments[2], arguments[3]
		if err := os.WriteFile(filepath.Join(directory, participant), []byte("ready"), 0o600); err != nil {
			os.Exit(3)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			_, projectErr := os.Stat(filepath.Join(directory, "project"))
			_, userErr := os.Stat(filepath.Join(directory, "user"))
			if projectErr == nil && userErr == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(3)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if output == "block" {
			fmt.Fprintln(os.Stderr, "project blocked")
			os.Exit(2)
		}
		fmt.Print("user context")
	}
	os.Exit(0)
}

func testCapability() Capability {
	return NewCapability([]byte("test capability key material 1234567890"))
}

func cloneRequest(t *testing.T, request InvocationRequest) InvocationRequest {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var cloned InvocationRequest
	if err := json.Unmarshal(raw, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func cloneResponse(t *testing.T, response InvocationResponse) InvocationResponse {
	t.Helper()
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var cloned InvocationResponse
	if err := json.Unmarshal(raw, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func validLifecycleInvocation(t *testing.T, event Event) Invocation {
	t.Helper()
	invocation := Invocation{Event: event, SessionID: "thread", CWD: t.TempDir(), Data: map[string]any{}}
	switch event {
	case SessionStart:
		invocation.Data["source"] = "startup"
	case UserPromptSubmit:
		invocation.Data["prompt"] = "hello"
	case SessionEnd:
		invocation.Data["reason"] = "other"
	case PermissionRequest, PreToolUse:
		invocation.Data["tool_name"] = "Bash"
		invocation.Data["tool_input"] = map[string]any{"command": "pwd"}
	case Notification:
		invocation.Data["notification_type"] = "agent_completed"
		invocation.Data["message"] = "done"
	case PostToolUse:
		invocation.Data["tool_name"] = "Bash"
		invocation.Data["tool_response"] = "ok"
	case PostToolUseFailure:
		invocation.Data["tool_name"] = "Bash"
		invocation.Data["error"] = "failed"
	case PreCompact:
		invocation.Data["trigger"] = "manual"
	case Stop:
		invocation.Data["last_assistant_message"] = "done"
	case SubagentStart:
		invocation.AgentID = "agent-id"
		invocation.AgentType = "worker"
		invocation.Data["agent_name"] = "worker"
	case SubagentStop:
		invocation.AgentID = "agent-id"
		invocation.AgentType = "worker"
		invocation.AgentTranscriptPath = filepath.Join(t.TempDir(), "agent.jsonl")
		invocation.Data["agent_name"] = "worker"
		invocation.Data["last_assistant_message"] = "done"
	}
	return invocation
}

func writeConfig(t *testing.T, path, command string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":%q}]}]}}`, command)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

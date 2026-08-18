package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

func TestParseShellAllowListDefaultsFailClosedAndExpandsRecommended(t *testing.T) {
	empty, err := parseShellAllowList("")
	if err != nil || empty.configured() || empty.allows("ls") {
		t.Fatalf("empty policy = %#v, error = %v", empty, err)
	}

	recommended, err := parseShellAllowList(" ReCommended , git, recommended, git ")
	if err != nil {
		t.Fatal(err)
	}
	if !recommended.restrictive() || !recommended.allows("ls -la && git status") {
		t.Fatalf("recommended policy did not allow expected commands: %#v", recommended)
	}
	for _, command := range []string{"sh", "bash", "python", "node", "find", "curl", "rm"} {
		if slices.Contains(recommended.commands, command) {
			t.Errorf("recommended policy unexpectedly contains %q", command)
		}
	}
	if count := countStrings(recommended.commands, "git"); count != 1 {
		t.Fatalf("git count = %d in %#v", count, recommended.commands)
	}

	unrestricted, err := parseShellAllowList(" ALL ")
	if err != nil || !unrestricted.configured() || unrestricted.restrictive() {
		t.Fatalf("unrestricted policy = %#v, error = %v", unrestricted, err)
	}
	if !unrestricted.allows("rm -rf /tmp/example") || unrestricted.allows("   ") {
		t.Fatalf("unrestricted policy has unexpected empty/non-empty behavior")
	}
}

func TestParseShellAllowListRejectsAmbiguousEntries(t *testing.T) {
	for _, value := range []string{
		"all,ls",
		"ls,ALL,cat",
		"go test",
		"ls;rm",
		"git|sh",
		"echo $HOME",
	} {
		if _, err := parseShellAllowList(value); err == nil {
			t.Errorf("parseShellAllowList(%q) succeeded", value)
		}
	}
}

func TestRestrictiveShellAllowListChecksEveryCommand(t *testing.T) {
	allowList, err := parseShellAllowList("ls,cat,grep,wc,git")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"ls",
		"ls -la",
		"cat 'file with spaces.txt' | grep error | wc -l",
		"git status && git diff",
		"'l''s' -la",
	} {
		if !allowList.allows(command) {
			t.Errorf("expected %q to be allowed", command)
		}
	}
	for _, command := range []string{
		"",
		"rm -rf /",
		"ls | rm -rf /tmp/example",
		"git status; sh -c whoami",
		"/bin/ls",
		"LS",
		"PATH=/tmp ls",
		"ls 'unterminated",
		"ls valid 'unterminated",
		"ls trailing\\",
	} {
		if allowList.allows(command) {
			t.Errorf("expected %q to be rejected", command)
		}
	}
}

func TestRestrictiveShellAllowListRejectsInjectionPatterns(t *testing.T) {
	allowList, err := parseShellAllowList("ls,cat,grep,echo")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"ls $(whoami)",
		"cat `whoami`",
		`echo $'\x24\x28whoami\x29'`,
		"ls\nrm -rf /",
		"ls\rrm -rf /",
		"ls\trm",
		"cat <(whoami)",
		"echo test >(cat)",
		"cat <<< word",
		"cat <<EOF",
		"echo hello > output",
		"echo hello >> output",
		"cat < input",
		"echo ${PATH}",
		"echo $HOME",
		"ls &",
		"cat file & echo done",
	} {
		if allowList.allows(command) {
			t.Errorf("injection pattern was allowed: %q", command)
		}
		if !containsDangerousShellPattern(command) {
			t.Errorf("injection pattern was not detected: %q", command)
		}
	}
	for _, command := range []string{
		"ls -la",
		"cat file && grep value file",
		"echo '$?'",
	} {
		if !allowList.allows(command) {
			t.Errorf("safe command was rejected: %q", command)
		}
	}
}

func TestCustomShellAllowListCannotEscapeThroughOperators(t *testing.T) {
	allowList, err := parseShellAllowList("mycmd")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"mycmd $(sh -c whoami)",
		"mycmd `sh -c whoami`",
		"mycmd > output",
		"mycmd $HOME",
		"mycmd; sh -c whoami",
		"mycmd | sh",
		"mycmd && find . -exec sh -c whoami ;",
		"sh -c mycmd",
		"find . -exec mycmd {} +",
	} {
		if allowList.allows(command) {
			t.Errorf("custom command escaped the allow-list: %q", command)
		}
	}
}

func TestShellAllowListMiddlewareRejectsInlineWithoutCallingExecute(t *testing.T) {
	allowList, err := parseShellAllowList("ls,git")
	if err != nil {
		t.Fatal(err)
	}
	middleware := shellAllowListMiddleware(allowList)
	nextCalls := 0
	next := func(context.Context, dagent.ToolCallRequest) (dagent.ToolCallResponse, error) {
		nextCalls++
		return dagent.ToolCallResponse{Result: datool.Result{Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: "ran"}}}}, nil
	}

	allowed, err := middleware.WrapToolCall(t.Context(), shellToolRequest("ls -la && git status"), next)
	if err != nil || nextCalls != 1 || toolResultText(allowed.Result) != "ran" {
		t.Fatalf("allowed response = %#v, calls = %d, error = %v", allowed, nextCalls, err)
	}
	for _, arguments := range []json.RawMessage{
		json.RawMessage(`{"command":"ls | rm /tmp/example"}`),
		json.RawMessage(`{"command":"ls $(whoami)"}`),
		json.RawMessage(`{"command":`),
		json.RawMessage(`{}`),
	} {
		response, wrapErr := middleware.WrapToolCall(t.Context(), dagent.ToolCallRequest{Call: damessage.ToolCall{
			ID: "blocked", Name: "execute", Arguments: arguments,
		}}, next)
		if wrapErr != nil || nextCalls != 1 || response.Result.Status != damessage.ToolStatusError || !strings.Contains(toolResultText(response.Result), "rejected") {
			t.Fatalf("blocked response = %#v, calls = %d, error = %v", response, nextCalls, wrapErr)
		}
	}

	other, err := middleware.WrapToolCall(t.Context(), dagent.ToolCallRequest{Call: damessage.ToolCall{ID: "read", Name: "read_file"}}, next)
	if err != nil || nextCalls != 2 || toolResultText(other.Result) != "ran" {
		t.Fatalf("other response = %#v, calls = %d, error = %v", other, nextCalls, err)
	}
}

func TestConfiguredShellAllowListReplacesOnlyShellApproval(t *testing.T) {
	rules := mutatingToolApprovalRules()
	unchanged := approvalRulesForShellAllowList(rules, shellAllowList{})
	if !slices.ContainsFunc(unchanged, func(rule dagent.ApprovalRule) bool { return rule.Pattern == "execute" }) {
		t.Fatal("zero policy removed the shell approval gate")
	}
	for _, value := range []string{"recommended", "all"} {
		allowList, err := parseShellAllowList(value)
		if err != nil {
			t.Fatal(err)
		}
		configured := approvalRulesForShellAllowList(rules, allowList)
		if slices.ContainsFunc(configured, func(rule dagent.ApprovalRule) bool { return rule.Pattern == "execute" }) {
			t.Errorf("%s policy retained the shell approval gate: %#v", value, configured)
		}
		if !slices.ContainsFunc(configured, func(rule dagent.ApprovalRule) bool { return rule.Pattern == "write_file" }) {
			t.Errorf("%s policy removed the file-write approval gate: %#v", value, configured)
		}
	}
}

func TestCLIParsesShellAllowListAndDocumentsFlag(t *testing.T) {
	options, err := parseCLI([]string{"-n", "inspect", "-S", "recommended,git"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !options.shellAllowList.restrictive() || !options.shellAllowList.allows("git status") {
		t.Fatalf("shell allow-list = %#v", options.shellAllowList)
	}
	if _, err := parseCLI([]string{"-n", "inspect", "--shell-allow-list", "all,git"}, io.Discard); err == nil {
		t.Fatal("ambiguous all list was accepted")
	}
	var usage strings.Builder
	printUsage(&usage)
	if !strings.Contains(usage.String(), "--shell-allow-list") {
		t.Fatalf("usage does not document shell allow-list:\n%s", usage.String())
	}
}

func TestConfiguredShellAllowListEnablesRunnerShell(t *testing.T) {
	allowList, err := parseShellAllowList("recommended")
	if err != nil {
		t.Fatal(err)
	}
	runner, closer, err := newRunner(runnerOptions{
		Authentication: modelAuthentication{apiKey: "test-key"},
		Model:          defaultModel,
		WorkingDir:     t.TempDir(),
		StateDir:       t.TempDir(),
		ShellAllowList: allowList,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	definitions := runner.Tools()
	if !slices.ContainsFunc(definitions, func(definition datool.Definition) bool { return definition.Name == "execute" }) {
		t.Fatalf("configured shell allow-list did not enable execute: %#v", definitions)
	}
}

func TestShellAllowListIsEnforcedAtBackendBoundary(t *testing.T) {
	allowList, err := parseShellAllowList("printf")
	if err != nil {
		t.Fatal(err)
	}
	shell, err := dabackend.NewLocalShell(dabackend.LocalShellOptions{
		Filesystem: dabackend.FilesystemOptions{Root: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := enforceShellAllowList(shell, allowList)
	sandbox, ok := dabackend.SandboxOf(backend)
	if !ok {
		t.Fatal("enforced backend lost shell capability")
	}
	result, err := dabackend.ExecuteSandbox(t.Context(), sandbox, "printf allowed", dabackend.ExecuteOptions{})
	if err != nil || result.Output != "allowed" {
		t.Fatalf("allowed result = %#v, error = %v", result, err)
	}
	if _, err := sandbox.Execute(t.Context(), "touch should-not-exist", time.Second); !errors.Is(err, errShellCommandRejected) {
		t.Fatalf("unlisted command error = %v", err)
	}
}

func countStrings(values []string, target string) int {
	count := 0
	for _, value := range values {
		if value == target {
			count++
		}
	}
	return count
}

func shellToolRequest(command string) dagent.ToolCallRequest {
	arguments, _ := json.Marshal(map[string]string{"command": command})
	return dagent.ToolCallRequest{Call: damessage.ToolCall{ID: "execute", Name: "execute", Arguments: arguments}}
}

func toolResultText(result datool.Result) string {
	var output strings.Builder
	for _, block := range result.Content {
		if block.Type == damessage.BlockText {
			output.WriteString(block.Text)
		}
	}
	return output.String()
}

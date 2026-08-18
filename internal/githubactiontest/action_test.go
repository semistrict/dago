package githubactiontest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func runScript(t *testing.T, script string, environment map[string]string) (string, string, error) {
	t.Helper()
	command := exec.Command("bash", filepath.Join(repositoryRoot(t), "scripts", "github-action", script))
	command.Env = append(os.Environ(), mapEnvironment(environment)...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func mapEnvironment(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}

func configureEnvironment(t *testing.T) (map[string]string, string) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	runnerTemp := filepath.Join(root, "runner")
	output := filepath.Join(root, "output")
	for _, directory := range []string{workspace, runnerTemp} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]string{
		"GITHUB_WORKSPACE":        workspace,
		"RUNNER_TEMP":             runnerTemp,
		"GITHUB_OUTPUT":           output,
		"GITHUB_RUN_ID":           "41",
		"GITHUB_RUN_ATTEMPT":      "2",
		"RUNNER_OS":               "Linux",
		"GITHUB_REF_NAME":         "feature/topic",
		"INPUT_ENABLE_MEMORY":     "true",
		"INPUT_MEMORY_SCOPE":      "branch",
		"INPUT_AGENT_NAME":        "reviewer",
		"INPUT_WORKING_DIRECTORY": ".",
	}, output
}

func parseSimpleOutputs(t *testing.T, filename string) map[string]string {
	t.Helper()
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		name, value, found := strings.Cut(line, "=")
		if found {
			result[name] = value
		}
	}
	return result
}

func TestConfigureProducesScopedCacheAndPersistentSession(t *testing.T) {
	environment, output := configureEnvironment(t)
	if _, stderr, err := runScript(t, "configure.sh", environment); err != nil {
		t.Fatalf("configure: %v: %s", err, stderr)
	}
	values := parseSimpleOutputs(t, output)
	if values["cache_enabled"] != "true" {
		t.Fatalf("cache_enabled = %q", values["cache_enabled"])
	}
	if !strings.HasPrefix(values["cache_prefix"], "dago-agent-") || !strings.Contains(values["cache_prefix"], "-reviewer-branch-") {
		t.Fatalf("cache_prefix = %q", values["cache_prefix"])
	}
	if values["cache_key"] != values["cache_prefix"]+"-41-2" {
		t.Fatalf("cache_key = %q", values["cache_key"])
	}
	if !strings.HasPrefix(values["session_id"], "ci-reviewer-") {
		t.Fatalf("session_id = %q", values["session_id"])
	}
	if values["state_dir"] != filepath.Join(environment["RUNNER_TEMP"], "dago-action-state", "reviewer", "branch-"+strings.TrimPrefix(values["session_id"], "ci-reviewer-")) {
		t.Fatalf("state_dir = %q", values["state_dir"])
	}

	secondOutput := filepath.Join(filepath.Dir(output), "output-2")
	environment["GITHUB_OUTPUT"] = secondOutput
	environment["GITHUB_RUN_ID"] = "42"
	if _, stderr, err := runScript(t, "configure.sh", environment); err != nil {
		t.Fatalf("configure second run: %v: %s", err, stderr)
	}
	second := parseSimpleOutputs(t, secondOutput)
	if second["cache_prefix"] != values["cache_prefix"] || second["session_id"] != values["session_id"] {
		t.Fatalf("durable identity changed across runs: first=%v second=%v", values, second)
	}
	if second["cache_key"] == values["cache_key"] {
		t.Fatal("save key must be unique per run")
	}

	thirdOutput := filepath.Join(filepath.Dir(output), "output-3")
	environment["GITHUB_OUTPUT"] = thirdOutput
	environment["INPUT_MEMORY_SCOPE"] = "repo"
	if _, stderr, err := runScript(t, "configure.sh", environment); err != nil {
		t.Fatalf("configure different scope: %v: %s", err, stderr)
	}
	third := parseSimpleOutputs(t, thirdOutput)
	if third["state_dir"] == values["state_dir"] || third["session_id"] == values["session_id"] || third["cache_prefix"] == values["cache_prefix"] {
		t.Fatalf("memory scopes are not isolated: branch=%v repo=%v", values, third)
	}
}

func TestConfigureRejectsInjectionAndWorkspaceEscape(t *testing.T) {
	environment, _ := configureEnvironment(t)
	environment["INPUT_AGENT_NAME"] = "agent\nkey=forged"
	if _, _, err := runScript(t, "configure.sh", environment); err == nil {
		t.Fatal("newline agent name was accepted")
	}

	environment, _ = configureEnvironment(t)
	outside := t.TempDir()
	link := filepath.Join(environment["GITHUB_WORKSPACE"], "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	environment["INPUT_WORKING_DIRECTORY"] = "outside"
	if _, _, err := runScript(t, "configure.sh", environment); err == nil {
		t.Fatal("workspace symlink escape was accepted")
	}

	environment, _ = configureEnvironment(t)
	environment["INPUT_MEMORY_SCOPE"] = "global"
	if _, _, err := runScript(t, "configure.sh", environment); err == nil {
		t.Fatal("unknown memory scope was accepted")
	}
}

func fakeAgent(t *testing.T, body string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(filename, []byte("#!/usr/bin/env bash\nset -euo pipefail\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return filename
}

func agentEnvironment(t *testing.T, binary string) (map[string]string, string, string, string) {
	t.Helper()
	root := t.TempDir()
	output := filepath.Join(root, "github-output")
	args := filepath.Join(root, "args")
	stdin := filepath.Join(root, "stdin")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"DAGO_ACTION_BINARY":      binary,
		"INPUT_PROMPT":            "inspect; touch /tmp/never-created\nsecond line",
		"INPUT_WORKING_DIRECTORY": root,
		"INPUT_STATE_DIR":         state,
		"INPUT_SESSION_ID":        "ci-reviewer-deadbeef",
		"INPUT_MODEL":             "test:model; echo unsafe",
		"INPUT_APPROVAL_MODEL":    "review:model",
		"INPUT_SHELL_ALLOW_LIST":  "recommended,git,gh",
		"INPUT_MAX_TURNS":         "8",
		"INPUT_TIMEOUT":           "90",
		"INPUT_QUIET":             "true",
		"GITHUB_OUTPUT":           output,
		"RUNNER_TEMP":             root,
		"FAKE_ARGS":               args,
		"FAKE_STDIN":              stdin,
	}, output, args, stdin
}

func nulArguments(t *testing.T, filename string) []string {
	t.Helper()
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(content), "\x00")
	return parts[:len(parts)-1]
}

func argumentValue(t *testing.T, arguments []string, flag string) string {
	t.Helper()
	for index := range arguments {
		if arguments[index] == flag && index+1 < len(arguments) {
			return arguments[index+1]
		}
	}
	t.Fatalf("missing argument %s in %q", flag, arguments)
	return ""
}

func TestRunAgentBuildsLiteralCommandAndSafeOutputs(t *testing.T) {
	binary := fakeAgent(t, `printf '%s\0' "$@" > "$FAKE_ARGS"
cat > "$FAKE_STDIN"
printf 'answer\nDAGO_fixed\nevil=1\n'`)
	environment, output, argsFile, stdinFile := agentEnvironment(t, binary)
	if _, stderr, err := runScript(t, "run-agent.sh", environment); err != nil {
		t.Fatalf("run: %v: %s", err, stderr)
	}
	arguments := nulArguments(t, argsFile)
	wantValues := map[string]string{
		"--cwd":              environment["INPUT_WORKING_DIRECTORY"],
		"--state-dir":        environment["INPUT_STATE_DIR"],
		"--resume":           environment["INPUT_SESSION_ID"],
		"--max-turns":        "8",
		"--timeout":          "90",
		"--shell-allow-list": "recommended,git,gh",
		"--model":            "test:model; echo unsafe",
		"--approval-model":   "review:model",
	}
	for flag, want := range wantValues {
		if got := argumentValue(t, arguments, flag); got != want {
			t.Errorf("%s value = %q, want %q", flag, got, want)
		}
	}
	for _, flag := range []string{"--stdin", "--quiet"} {
		if !contains(arguments, flag) {
			t.Errorf("missing %s in %q", flag, arguments)
		}
	}
	prompt, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(prompt) != environment["INPUT_PROMPT"] {
		t.Fatalf("stdin = %q", prompt)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.HasPrefix(text, "exit_code=0\nresponse<<DAGO_") || !strings.Contains(text, "\nanswer\nDAGO_fixed\nevil=1\n\nDAGO_") {
		t.Fatalf("unsafe or incomplete action output:\n%s", text)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestRunAgentValidatesDefaultsAndPropagatesFailure(t *testing.T) {
	binary := fakeAgent(t, `cat >/dev/null
printf 'failed response'
exit 7`)
	environment, output, _, _ := agentEnvironment(t, binary)
	environment["INPUT_MODEL"] = ""
	environment["INPUT_APPROVAL_MODEL"] = ""
	_, _, err := runScript(t, "run-agent.sh", environment)
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 7 {
		t.Fatalf("exit error = %v", err)
	}
	content, readErr := os.ReadFile(output)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(content), "exit_code=7") || !strings.Contains(string(content), "failed response") {
		t.Fatalf("output = %q", content)
	}

	for name, value := range map[string]string{"INPUT_MAX_TURNS": "0", "INPUT_TIMEOUT": "1.5", "INPUT_QUIET": "yes"} {
		invalid := mapsClone(environment)
		invalid[name] = value
		if _, _, validationErr := runScript(t, "run-agent.sh", invalid); validationErr == nil {
			t.Errorf("accepted %s=%q", name, value)
		}
	}
}

func mapsClone(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func TestRunAgentForwardsCancellation(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "terminated")
	binary := fakeAgent(t, `trap 'printf terminated > "$FAKE_MARKER"; exit 143' TERM
printf ready > "$FAKE_READY"
while :; do sleep 1; done`)
	environment, output, _, _ := agentEnvironment(t, binary)
	ready := filepath.Join(filepath.Dir(marker), "ready")
	environment["FAKE_MARKER"] = marker
	environment["FAKE_READY"] = ready
	command := exec.Command("bash", filepath.Join(repositoryRoot(t), "scripts", "github-action", "run-agent.sh"))
	command.Env = append(os.Environ(), mapEnvironment(environment)...)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatal("fake agent did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := command.Wait()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 143 {
		t.Fatalf("cancellation exit = %v", err)
	}
	if content, readErr := os.ReadFile(marker); readErr != nil || string(content) != "terminated" {
		t.Fatalf("child did not receive termination: content=%q err=%v", content, readErr)
	}
	content, readErr := os.ReadFile(output)
	if readErr != nil || !strings.Contains(string(content), "exit_code=143") {
		t.Fatalf("cancellation output = %q, %v", content, readErr)
	}
}

func fakeCloneTools(t *testing.T, mode string) (string, string) {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "clone-args")
	gh := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
printf '%%s\0' "$@" > %s
destination="$4"
mkdir -p "$destination/review"
printf '%s' > "$destination/review/SKILL.md"
`, strconv.Quote(log), mode)
	if mode == "symlink" {
		gh += "ln -s /etc/passwd \"$destination/review/leak\"\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestInstallSkillsValidatesAndInstallsWithoutShellEvaluation(t *testing.T) {
	workspace := t.TempDir()
	bin, log := fakeCloneTools(t, "valid")
	environment := map[string]string{
		"PATH":                    bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"RUNNER_TEMP":             t.TempDir(),
		"INPUT_WORKING_DIRECTORY": workspace,
		"INPUT_SKILLS_REPO":       "owner/repository@v1.2.3",
		"INPUT_GITHUB_TOKEN":      "secret",
	}
	if _, stderr, err := runScript(t, "install-skills.sh", environment); err != nil {
		t.Fatalf("install: %v: %s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".deepagents", "skills", "review", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	arguments := nulArguments(t, log)
	if !contains(arguments, "owner/repository") || argumentValue(t, arguments, "--branch") != "v1.2.3" {
		t.Fatalf("clone arguments = %q", arguments)
	}

	unsafe := mapsClone(environment)
	unsafe["INPUT_SKILLS_REPO"] = "owner/repository; touch injected"
	if _, _, err := runScript(t, "install-skills.sh", unsafe); err == nil {
		t.Fatal("unsafe repository input was accepted")
	}
	unsafe["INPUT_SKILLS_REPO"] = "--help/repository"
	if _, _, err := runScript(t, "install-skills.sh", unsafe); err == nil {
		t.Fatal("option-shaped repository input was accepted")
	}
	if _, err := os.Stat(filepath.Join(workspace, "injected")); !os.IsNotExist(err) {
		t.Fatalf("injection marker exists: %v", err)
	}
}

func TestInstallSkillsRejectsUntrustedContentAndCollisions(t *testing.T) {
	workspace := t.TempDir()
	bin, _ := fakeCloneTools(t, "symlink")
	environment := map[string]string{
		"PATH":                    bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"RUNNER_TEMP":             t.TempDir(),
		"INPUT_WORKING_DIRECTORY": workspace,
		"INPUT_SKILLS_REPO":       "owner/repository",
	}
	if _, _, err := runScript(t, "install-skills.sh", environment); err == nil {
		t.Fatal("skill containing symlink was accepted")
	}

	bin, _ = fakeCloneTools(t, "valid")
	environment["PATH"] = bin + string(os.PathListSeparator) + os.Getenv("PATH")
	existing := filepath.Join(workspace, ".deepagents", "skills", "review")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runScript(t, "install-skills.sh", environment); err == nil {
		t.Fatal("existing skill collision was accepted")
	}
}

func TestInstallSkillsPropagatesCheckoutFailure(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/usr/bin/env bash\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	environment := map[string]string{
		"PATH":                    bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"RUNNER_TEMP":             t.TempDir(),
		"INPUT_WORKING_DIRECTORY": workspace,
		"INPUT_SKILLS_REPO":       "owner/repository",
	}
	if _, stderr, err := runScript(t, "install-skills.sh", environment); err == nil || !strings.Contains(stderr, "failed to clone") {
		t.Fatalf("checkout failure = %v, stderr = %q", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".deepagents", "skills")); !os.IsNotExist(err) {
		t.Fatalf("failed checkout changed skills directory: %v", err)
	}
}

func TestActionContractConnectsCacheSkillsRunAndFailure(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(repositoryRoot(t), "action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	action := string(content)
	var manifest struct {
		Inputs map[string]struct {
			Required bool `yaml:"required"`
		} `yaml:"inputs"`
		Runs struct {
			Using string `yaml:"using"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(content, &manifest); err != nil {
		t.Fatalf("parse action.yml: %v", err)
	}
	if manifest.Runs.Using != "composite" {
		t.Fatalf("runs.using = %q", manifest.Runs.Using)
	}
	for _, input := range []string{"prompt", "openai_api_key"} {
		if !manifest.Inputs[input].Required {
			t.Errorf("required input %q is not explicit", input)
		}
	}
	for _, required := range []string{
		"prompt:\n    description:",
		"openai_api_key:\n    description:",
		"required: true",
		"actions/cache/restore@6849a6489940f00c2f30c0fb92c6274307ccb58a",
		"actions/cache/save@6849a6489940f00c2f30c0fb92c6274307ccb58a",
		"INPUT_SKILLS_REPO: ${{ inputs.skills_repo }}",
		"continue-on-error: true",
		"Propagate agent result",
	} {
		if !strings.Contains(action, required) {
			t.Errorf("action contract missing %q", required)
		}
	}
	if strings.Contains(action, "actions/cache/restore@v") || strings.Contains(action, "actions/cache/save@v") {
		t.Fatal("cache actions must be pinned to immutable revisions")
	}
	if strings.Contains(action, "path: ${{ steps.config.outputs.state_dir }}\n") {
		t.Fatal("the cache must not persist credentials from the whole state directory")
	}
	for _, databaseFile := range []string{"threads.db", "threads.db-wal", "threads.db-shm"} {
		if strings.Count(action, "/"+databaseFile+"\n") != 2 {
			t.Errorf("cache paths do not restore and save %s", databaseFile)
		}
	}
	runScript, err := os.ReadFile(filepath.Join(repositoryRoot(t), "scripts", "github-action", "run-agent.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runScript), `--resume "$INPUT_SESSION_ID"`) {
		t.Fatal("runner does not resume the cache-scoped durable session")
	}
}

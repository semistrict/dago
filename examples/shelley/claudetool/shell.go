package claudetool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"

	"github.com/semistrict/dago/examples/shelley/claudetool/bashkit"
)

// ShellTool runs commands without unconditionally killing
// commands on timeout. Instead, if the command does not finish within
// yield_time_seconds, the tool returns to the LLM with the tail of output,
// the PID of the still-running process, and the path to a temp log file the
// agent can use (via the bash tool) to poll, wait, or kill.
//
// This lets the agent supervise long-running but bounded jobs (builds,
// tests, big rsync) without committing the whole tool call to a hard timeout.
type ShellTool struct {
	// CheckPermission is called before running any command, if set.
	CheckPermission PermissionCallback
	// EnableJITInstall enables just-in-time tool installation for missing commands.
	EnableJITInstall bool
	// WorkingDir is the shared mutable working directory.
	WorkingDir *MutableWorkingDir
	// LLMProvider provides access to LLM services for tool validation.
	LLMProvider LLMServiceProvider
	// Env holds the conversation context exposed to invoked commands as
	// SHELLEY_* environment variables.
	Env ShelleyEnv
	// BackgroundCtx is the long-lived context that owns spawned processes.
	// When nil, defaults to context.Background(): yielded jobs survive the
	// per-call ctx ending. Set this to a server- or conversation-lifetime
	// context to have shelley reap background jobs on shutdown.
	BackgroundCtx context.Context
	// DefaultYield is the default yield_time_seconds (default 30s).
	DefaultYield time.Duration
	// MaxYield is the upper cap on yield_time_seconds (default 10m).
	MaxYield time.Duration
	// TempDir overrides the directory used for log files (default os.TempDir).
	TempDir string
}

const (
	shellName         = "shell"
	shellDefaultYield = 30 * time.Second
	shellMaxYield     = 10 * time.Minute
	shellMinYield     = 1 * time.Second
	shellTailBytes    = 8 * 1024
	shellWaitDelay    = 15 * time.Second
)

const shellDescription = `Executes shell commands via bash --login -c, returning combined stdout/stderr.
State (cwd, vars, aliases) does not persist between calls; use change_dir for cwd.

If the command does not finish within yield_time_seconds, this tool returns
the output so far, the PID, and the log file path; the process keeps running
in the background.

For long-lived processes (servers, watchers), prefer tmux.

To wake yourself later (longer than the max yield), detach a tmux session that
sleeps then calls the Shelley client. Use double quotes so THIS shell expands
$SHELLEY_CONVERSATION_ID (tmux's server env may be stale):
  tmux new-session -d "sleep 3600 && shelley client chat -c $SHELLEY_CONVERSATION_ID -p 'Resume: <what next>'"

Destructive commands (deleting .git, home dirs, broad wildcards) require
explicit paths and user confirmation.

Keep commands under 60k tokens; for complex scripts, write a file and run it.

Don't pipe to head/tail/sed -n just to truncate output; the tool already tails
on yield and otherwise returns it in full. Filter only when targeting a
specific pattern (e.g. grep).
`

type shellInput struct {
	Command          string `json:"command" description:"Shell command to execute"`
	YieldTimeSeconds int    `json:"yield_time_seconds,omitempty" jsonschema:"minimum=1"`
}

// ShellDisplayData is the display data sent to the UI for shell tool results.
type ShellDisplayData struct {
	WorkingDir string `json:"workingDir"`
	PID        int    `json:"pid,omitempty"`
	LogPath    string `json:"logPath,omitempty"`
	Yielded    bool   `json:"yielded,omitempty"`
}

type shellExecution struct {
	Output  string
	Display ShellDisplayData
}

// secondsCeil rounds a duration up to whole seconds.
func secondsCeil(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}

func (s *ShellTool) defaultYield() time.Duration {
	if s.DefaultYield > 0 {
		return s.DefaultYield
	}
	return shellDefaultYield
}

func (s *ShellTool) maxYield() time.Duration {
	if s.MaxYield > 0 {
		return s.MaxYield
	}
	return shellMaxYield
}

func (s *ShellTool) tempDir() string {
	if s.TempDir != "" {
		return s.TempDir
	}
	return os.TempDir()
}

func (s *ShellTool) backgroundCtx() context.Context {
	if s.BackgroundCtx != nil {
		return s.BackgroundCtx
	}
	return context.Background()
}

func (s *ShellTool) yieldDuration(req shellInput) time.Duration {
	d := s.defaultYield()
	if req.YieldTimeSeconds > 0 {
		d = time.Duration(req.YieldTimeSeconds) * time.Second
	}
	if d < shellMinYield {
		d = shellMinYield
	}
	if d > s.maxYield() {
		d = s.maxYield()
	}
	return d
}

// NativeTool executes shell commands through dago's tool contract.
func (s *ShellTool) NativeTool() datool.Tool {
	def := max(secondsCeil(s.defaultYield()), 1)
	maximum := max(secondsCeil(s.maxYield()), 1)
	if def > maximum {
		def = maximum
	}
	return datool.MustNew(shellName, strings.TrimSpace(shellDescription), func(ctx context.Context, input shellInput) (datool.Result, error) {
		execution, err := s.execute(ctx, input, true)
		if err != nil {
			return datool.Result{}, err
		}
		artifact, err := json.Marshal(execution.Display)
		if err != nil {
			return datool.Result{}, fmt.Errorf("encode shell display: %w", err)
		}
		return datool.Result{
			Content:  []dmessage.ContentBlock{{Type: dmessage.BlockText, Text: execution.Output}},
			Artifact: artifact,
		}, nil
	},
		datool.WithPropertyValue("yield_time_seconds", "maximum", maximum),
		datool.WithPropertyValue("yield_time_seconds", "description", fmt.Sprintf("Seconds to wait synchronously before yielding control while the command continues running in the background. Default %d. Maximum %d.", def, maximum)),
	)
}

func (s *ShellTool) execute(ctx context.Context, req shellInput, nativeModelCalls bool) (shellExecution, error) {
	wd := s.WorkingDir.Get()
	if _, err := os.Stat(wd); err != nil {
		if os.IsNotExist(err) {
			return shellExecution{}, fmt.Errorf("working directory does not exist: %s (use change_dir to switch to a valid directory)", wd)
		}
		return shellExecution{}, fmt.Errorf("cannot access working directory %s: %w", wd, err)
	}

	if err := bashkit.Check(req.Command); err != nil {
		return shellExecution{}, err
	}
	if s.CheckPermission != nil {
		if err := s.CheckPermission(req.Command); err != nil {
			return shellExecution{}, err
		}
	}

	if s.EnableJITInstall {
		installer := commandInstaller{LLMProvider: s.LLMProvider}
		installErr := installer.checkAndInstallMissingTools(ctx, req.Command)
		if installErr != nil {
			slog.DebugContext(ctx, "failed to auto-install missing tools", "error", installErr)
		}
	}

	if !isNoTrailerSet() {
		req.Command = bashkit.AddCoauthorTrailer(req.Command, "Co-authored-by: Shelley <shelley@example.invalid>")
	}

	yield := s.yieldDuration(req)

	// Open a temp log file. We want a deterministic, pid-based name so the
	// agent has a stable path to reference; create with CreateTemp first so
	// we don't race for a name, then rename to a pid-based path after Start.
	tmpFile, err := os.CreateTemp(s.tempDir(), "shelley-shell-*.log")
	if err != nil {
		return shellExecution{}, fmt.Errorf("failed to create log file: %w", err)
	}
	logPath := tmpFile.Name()

	cmd := exec.CommandContext(s.backgroundCtx(), "bash", "--login", "-c", req.Command)
	cmd.Dir = wd
	cmd.Stdin = nil
	cmd.Stdout = tmpFile
	cmd.Stderr = tmpFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = shellWaitDelay

	env := stripShelleyEnv(os.Environ())
	env = append(
		env,
		"SKETCH=1",
		"EDITOR=/bin/false",
		`GIT_SEQUENCE_EDITOR=echo "To do an interactive rebase, run it in a tmux session." && exit 1`,
	)
	env = append(env, s.Env.Environ(cmd.Dir)...)
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		tmpFile.Close()
		os.Remove(logPath)
		return shellExecution{}, fmt.Errorf("command failed to start: %w", err)
	}
	pid := cmd.Process.Pid
	pgid := pid // Setpgid: true => pgid == pid

	// Rename to a pid-based path. The fd inside the child is unaffected.
	pidPath := filepath.Join(filepath.Dir(logPath), fmt.Sprintf("shelley-shell-%d.log", pid))
	if err := os.Rename(logPath, pidPath); err == nil {
		logPath = pidPath
	}

	// The child has its own dup of the fd; we no longer need ours.
	tmpFile.Close()

	// Live tail to UI while we wait.
	progressDone := make(chan struct{})
	progressStop := make(chan struct{})
	go shellProgressLoop(ctx, logPath, progressStop, progressDone)
	stopProgress := func() {
		select {
		case <-progressStop:
		default:
			close(progressStop)
		}
		<-progressDone
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	timer := time.NewTimer(yield)
	defer timer.Stop()

	display := ShellDisplayData{WorkingDir: wd, PID: pid, LogPath: logPath}

	select {
	case waitErr := <-done:
		stopProgress()
		out, ferr := readAndFormatShellOutput(logPath)
		if ferr != nil {
			return shellExecution{}, fmt.Errorf("failed to read shell output: %w", ferr)
		}
		if waitErr != nil {
			return shellExecution{}, fmt.Errorf("[command failed: %w]\n%s", waitErr, out)
		}
		return shellExecution{Output: out, Display: display}, nil

	case <-ctx.Done():
		// Per-call cancel: kill the pgroup and wait briefly for cleanup.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		stopProgress()
		out, _ := readAndFormatShellOutput(logPath)
		return shellExecution{}, fmt.Errorf("[command cancelled: %w]\n%s", ctx.Err(), out)

	case <-timer.C:
		stopProgress()
		display.Yielded = true
		tail := readTailString(logPath, shellTailBytes)
		payload := buildYieldPayload(req.Command, pid, pgid, logPath, tail, yield)
		// After the yielded process eventually exits, schedule the log file
		// for deletion. The grace period is generous so agents that come
		// back later (after polling, waiting, or working on something else)
		// can still read it.
		go func() {
			<-done
			select {
			case <-time.After(15 * time.Minute):
			case <-s.backgroundCtx().Done():
			}
			_ = os.Remove(logPath)
		}()
		return shellExecution{Output: payload, Display: display}, nil
	}
}

// readTailString returns up to maxBytes from the end of the file (best-effort).
func readTailString(path string, maxBytes int64) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("(could not open log: %v)", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("(could not stat log: %v)", err)
	}
	size := st.Size()
	if size == 0 {
		return ""
	}
	start := int64(0)
	truncated := false
	if size > maxBytes {
		start = size - maxBytes
		truncated = true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return fmt.Sprintf("(could not seek log: %v)", err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return fmt.Sprintf("(could not read log: %v)", err)
	}
	out := string(b)
	if truncated {
		// Drop a possibly-partial first line.
		if i := strings.IndexByte(out, '\n'); i >= 0 && i < len(out)-1 {
			out = out[i+1:]
		}
	}
	return out
}

// readAndFormatShellOutput reads the full log and runs it through the
// large-output formatter shared with the bash tool.
func readAndFormatShellOutput(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return formatShellForegroundOutput(string(b))
}

func buildYieldPayload(command string, pid, pgid int, logPath, tail string, yield time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[yielded after %s; still running] PID=%d PGID=%d log=%s\n", yield, pid, pgid, logPath)
	fmt.Fprintf(&b, "command: %s\n", command)
	b.WriteString("--- last output ---\n")
	if tail == "" {
		b.WriteString("(no output yet)\n")
	} else {
		b.WriteString(tail)
		if !strings.HasSuffix(tail, "\n") {
			b.WriteByte('\n')
		}
	}
	b.WriteString("--- end ---\n\n")
	b.WriteString("To check status without waiting:\n")
	fmt.Fprintf(&b, "  kill -0 %d 2>/dev/null && echo running || echo exited; tail -c 8192 %s\n\n", pid, logPath)
	b.WriteString("To kill the process and its children:\n")
	fmt.Fprintf(&b, "  kill -TERM -- -%d; sleep 1; kill -KILL -- -%d 2>/dev/null; true\n\n", pgid, pgid)
	b.WriteString("Prefer tmux for long-lived processes (servers, watchers).\n")
	return b.String()
}

const shellProgressInterval = 500 * time.Millisecond

func shellProgressLoop(ctx context.Context, path string, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(shellProgressInterval)
	defer ticker.Stop()
	last := ""
	emit := func() {
		t := readTailString(path, shellProgressMaxBytes)
		if t != last {
			last = t
			_ = datool.EmitProgress(ctx, datool.Progress{Name: shellName, Output: t})
		}
	}
	for {
		select {
		case <-stop:
			emit()
			return
		case <-ticker.C:
			emit()
		}
	}
}

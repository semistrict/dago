package dacode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
	checkpointsqlite "github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/daworkspace"
)

const defaultModel = "gpt-5.6-terra"

const workspaceGuidancePrompt = `<workspace_instructions>
{agent_memory}
</workspace_instructions>

<workspace_instruction_rules>
These files contain project instructions. Follow the instructions that apply to each file you touch. More deeply scoped files take precedence, while direct user instructions take precedence over workspace files. Read listed subdirectory guidance before editing within its directory.
</workspace_instruction_rules>`

type eventStream interface {
	Next(context.Context) (dagent.Event, error)
	Result(context.Context) (dagent.Result, error)
	Close() error
}

type agentRunner interface {
	Start(context.Context, dagent.Input) eventStream
	Cancel(context.Context, string) error
	Review(context.Context, approvalReviewRequest) (approvalReviewResult, error)
}

type dagoRunner struct {
	agent    *dagent.Agent
	reviewer *dagent.Agent
}

func (runner *dagoRunner) Start(ctx context.Context, input dagent.Input) eventStream {
	return runner.agent.Stream(ctx, input)
}

func (runner *dagoRunner) Cancel(ctx context.Context, threadID string) error {
	_, err := runner.agent.Cancel(ctx, dagent.Input{Config: dacheckpoint.Config{ThreadID: threadID}})
	return err
}

type runnerOptions struct {
	Authentication modelAuthentication
	BaseURL        string
	Model          string
	WorkingDir     string
	StateDir       string
	ReviewTools    bool
	Shell          bool
	AutoReview     bool
	ReviewModel    string
}

func newRunner(options runnerOptions) (agentRunner, io.Closer, error) {
	model, err := options.Authentication.newModel(options.Model, options.BaseURL)
	if err != nil {
		return nil, nil, err
	}

	var backend dabackend.Backend
	if options.Shell {
		backend, err = dabackend.NewLocalShell(dabackend.LocalShellOptions{
			Filesystem: dabackend.FilesystemOptions{Root: options.WorkingDir},
			InheritEnv: true,
		})
	} else {
		backend, err = dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: options.WorkingDir})
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open workspace: %w", err)
	}

	if err := os.MkdirAll(options.StateDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create state directory: %w", err)
	}
	saver, err := checkpointsqlite.Open(filepath.Join(options.StateDir, "threads.db"))
	if err != nil {
		return nil, nil, err
	}

	filesystem := dago.Filesystem{}
	var interruptOn []dagent.ApprovalRule
	if options.ReviewTools {
		interruptOn = mutatingToolApprovalRules()
	}

	memory, guidanceSummary := workspaceContext(options.WorkingDir)
	skillSources := existingVirtualDirectories(options.WorkingDir,
		".agents/skills", ".claude/skills", ".deepagents/skills")

	systemText := `You are dacode, an interactive coding agent. Work as a careful senior engineer inside the configured workspace. Filesystem tool paths are virtual: use / for the workspace root and never pass a host filesystem path. Inspect relevant files before making claims, use the available filesystem and shell tools to complete requested work, preserve unrelated user changes, and verify edits with focused tests. Keep final responses concise and concrete.`
	systemText += guidanceSummary
	system := damessage.System(systemText)
	agent := dago.New(model, dago.Options{
		Name: "dacode", SystemMessage: system, Backend: backend,
		Filesystem: filesystem, Skills: dago.Skills{Sources: skillSources}, Memory: memory,
		EnableTodo: true, InterruptOn: interruptOn, Saver: saver, RetainThreadState: true,
	})
	runner := &dagoRunner{agent: agent}
	if options.AutoReview {
		reviewModel, reviewErr := options.Authentication.newModel(options.ReviewModel, options.BaseURL)
		if reviewErr != nil {
			_ = saver.Close()
			return nil, nil, reviewErr
		}
		readOnly, reviewErr := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: options.WorkingDir})
		if reviewErr != nil {
			_ = saver.Close()
			return nil, nil, fmt.Errorf("open review workspace: %w", reviewErr)
		}
		runner.reviewer = newApprovalReviewer(reviewModel, readOnly)
	}
	return runner, saver, nil
}

func mutatingToolApprovalRules() []dagent.ApprovalRule {
	decisions := []dagent.ApprovalDecision{dagent.ApprovalApprove, dagent.ApprovalReject}
	return []dagent.ApprovalRule{
		{Pattern: "write_file", Description: "Allow this file write?", AllowedDecisions: decisions},
		{Pattern: "edit_file", Description: "Allow this file edit?", AllowedDecisions: decisions},
		{Pattern: "delete", Description: "Allow this file deletion?", AllowedDecisions: decisions},
		{Pattern: "execute", Description: "Allow this shell command?", AllowedDecisions: decisions},
	}
}

func workspaceContext(root string) (dago.Memory, string) {
	virtualRoot, err := filepath.Abs(root)
	if err != nil {
		virtualRoot = filepath.Clean(root)
	}
	guidance := daworkspace.DiscoverGuidance(context.Background(), daworkspace.GuidanceOptions{
		Root: root, WorkingDirectory: root, TrustWorkspace: true,
	})
	memory := dago.Memory{}
	if len(guidance.Root) > 0 {
		memory.Sources = make([]string, 0, len(guidance.Root))
		memory.Contents = make(map[string]string, len(guidance.Root))
		memory.SystemPrompt = dago.PromptTemplate{Mode: dago.PromptCustom, Text: workspaceGuidancePrompt}
		for _, file := range guidance.Root {
			virtualPath, ok := virtualWorkspacePath(virtualRoot, file.Path)
			if !ok {
				continue
			}
			memory.Sources = append(memory.Sources, virtualPath)
			memory.Contents[virtualPath] = file.Content
		}
	}
	virtualSubdirectories := make([]string, 0, len(guidance.Subdirectories))
	for _, filePath := range guidance.Subdirectories {
		virtualPath, ok := virtualWorkspacePath(virtualRoot, filePath)
		if ok {
			virtualSubdirectories = append(virtualSubdirectories, virtualPath)
		}
	}
	return memory, daworkspace.FormatSubdirectoryGuidance(virtualSubdirectories, 10)
}

func virtualWorkspacePath(root, filePath string) (string, bool) {
	relative, err := filepath.Rel(root, filePath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	if relative == "." {
		return "/", true
	}
	return "/" + strings.TrimPrefix(filepath.ToSlash(relative), "/"), true
}

func existingVirtualDirectories(root string, names ...string) []string {
	candidates := make([]string, 0, len(names))
	for _, name := range names {
		candidates = append(candidates, filepath.Join(root, filepath.FromSlash(name)))
	}
	existing := daworkspace.ExistingDirectories(candidates...)
	result := make([]string, 0, len(existing))
	for _, directory := range existing {
		relative, err := filepath.Rel(root, directory)
		if err == nil {
			result = append(result, "/"+strings.TrimPrefix(filepath.ToSlash(relative), "/"))
		}
	}
	return result
}

func newThreadID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate thread id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

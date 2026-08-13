package claudetool

import (
	"context"
	"errors"
	"os"
	"sync"

	dago "github.com/semistrict/dago"
	dbackend "github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"

	"github.com/semistrict/dago/examples/shelley/claudetool/browse"
)

// WorkingDir is a thread-safe mutable working directory.
type MutableWorkingDir struct {
	mu  sync.RWMutex
	dir string
}

// NewMutableWorkingDir creates a new MutableWorkingDir with the given initial directory.
func NewMutableWorkingDir(dir string) *MutableWorkingDir {
	return &MutableWorkingDir{dir: dir}
}

// Get returns the current working directory.
func (w *MutableWorkingDir) Get() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.dir
}

// Set updates the working directory.
func (w *MutableWorkingDir) Set(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dir = dir
}

// ToolSetConfig contains configuration for creating a ToolSet.
type ToolSetConfig struct {
	// TrustWorkspaceGuidance allows repository guidance files and repository
	// skills to influence the agent. Keep this false for unreviewed checkouts.
	TrustWorkspaceGuidance bool
	// WorkingDir is the initial working directory for tools.
	WorkingDir string
	// LLMProvider provides access to LLM services for tool validation.
	LLMProvider LLMServiceProvider
	// EnableJITInstall enables just-in-time tool installation.
	EnableJITInstall bool
	// EnableBrowser enables browser tools.
	EnableBrowser bool
	// ModelID is the model being used for this conversation.
	// Used to determine tool configuration (e.g., simplified patch schema for weaker models).
	ModelID string
	// ReasoningLevel is the parent conversation's user-facing reasoning/thinking
	// level (one of "off", "minimal", "low", "medium", "high", "xhigh", or ""
	// for the service default). Subagents inherit this when their "reasoning"
	// parameter is not specified.
	ReasoningLevel string
	// OnWorkingDirChange is called when the working directory changes.
	// This can be used to persist the change to a database.
	OnWorkingDirChange func(newDir string)
	// SubagentRunner is the runner for subagent conversations.
	// If set, the subagent tool will be available.
	SubagentRunner SubagentRunner
	// SubagentDB is the database for subagent conversations.
	SubagentDB SubagentDB
	// ParentConversationID is the ID of the parent conversation (for subagent tool).
	ParentConversationID string
	// ConversationID is the ID of the conversation these tools belong to.
	// This is exposed to bash commands via the SHELLEY_CONVERSATION_ID environment variable.
	ConversationID string
	// Env holds additional conversation context (slug, model, user email,
	// server port) exposed to bash/shell commands as SHELLEY_* environment
	// variables, matching the variables injected into interactive "!"
	// terminals. ConversationID above is authoritative for
	// SHELLEY_CONVERSATION_ID and overrides Env.ConversationID.
	Env ShelleyEnv
	// SubagentDepth is the nesting depth of this conversation.
	// 0 = top-level conversation, 1 = subagent, 2 = sub-subagent, etc.
	SubagentDepth int
	// MaxSubagentDepth is the maximum nesting depth for subagents.
	// Subagent tool is only available when SubagentDepth < MaxSubagentDepth.
	// A value of 0 means no limit (but SubagentRunner/SubagentDB must still be set).
	// Set to 1 to allow only top-level conversations (depth 0) to spawn subagents.
	MaxSubagentDepth int
	// BuildAvailableModels, if set, is called by NewToolSet to compute the
	// list of models that subagent / llm_one_shot tools can choose from.
	// It is invoked each time a ToolSet is built so new conversations pick
	// up custom models added at runtime, instead of being stuck with a
	// snapshot taken at server start. If nil, the list is built from
	// LLMProvider.GetAvailableModels() (without display names).
	BuildAvailableModels func() []AvailableModel
	// ToolOverrides maps tool name to "on" or "off". Tools not listed use their default.
	ToolOverrides map[string]string
	// DisableAllTools disables every tool by default; ToolOverrides with "on" re-enable.
	DisableAllTools bool
}

// ToolSet holds a set of tools for a single conversation.
// Each conversation should have its own ToolSet.
type ToolSet struct {
	definitions []datool.Definition
	nativeTools []datool.Tool
	filesystem  []string
	cleanup     func()
	wd          *MutableWorkingDir
}

// Tools returns display-safe definitions for all local and provider-hosted tools.
func (ts *ToolSet) Tools() []datool.Definition {
	return append([]datool.Definition(nil), ts.definitions...)
}

// NativeTools returns production dago executables. A fresh slice prevents
// callers from changing the tool set after construction.
func (ts *ToolSet) NativeTools() []datool.Tool {
	return append([]datool.Tool(nil), ts.nativeTools...)
}

// FilesystemTools returns the canonical dago filesystem tool names selected by
// Shelley's user-facing tool settings.
func (ts *ToolSet) FilesystemTools() []string {
	if ts.filesystem == nil {
		return nil
	}
	result := make([]string, len(ts.filesystem))
	copy(result, ts.filesystem)
	return result
}

// Cleanup releases resources held by the tools (e.g., browser).
func (ts *ToolSet) Cleanup() {
	if ts.cleanup != nil {
		ts.cleanup()
	}
}

// WorkingDir returns the shared working directory.
func (ts *ToolSet) WorkingDir() *MutableWorkingDir {
	return ts.wd
}

// serverSideTools returns display definitions for provider-hosted tools.
// Server-side tools are executed on the LLM provider's infrastructure.
func serverSideTools(profile damodel.Profile) []datool.Definition {
	if !profile.SupportsWebSearch {
		return nil
	}
	if profile.Provider == "openai" {
		return []datool.Definition{{Name: "web_search", Description: "Search the web using the model provider."}}
	}
	return nil
}

// NewToolSet creates a new set of tools for a conversation.
func NewToolSet(ctx context.Context, cfg ToolSetConfig) *ToolSet {
	workingDir := cfg.WorkingDir
	if workingDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			workingDir = home
		} else {
			workingDir = "/"
		}
	}
	wd := NewMutableWorkingDir(workingDir)

	env := cfg.Env
	env.ConversationID = cfg.ConversationID

	changeDirTool := &ChangeDirTool{
		WorkingDir: wd,
		OnChange:   cfg.OnWorkingDirChange,
	}

	outputIframeTool := &OutputIframeTool{WorkingDir: wd}

	nativeTools := []datool.Tool{changeDirTool.NativeTool(), outputIframeTool.NativeTool()}
	filesystemTools := selectedNativeFilesystemTools(cfg.ToolOverrides, cfg.DisableAllTools)
	// Shelley's yielding shell remains an application-specific opt-in. The
	// ordinary command path is dago's execute tool.
	if IsToolEnabled("shell", cfg.ToolOverrides, cfg.DisableAllTools) {
		nativeTools = append(nativeTools, (&ShellTool{
			WorkingDir: wd, LLMProvider: cfg.LLMProvider, EnableJITInstall: cfg.EnableJITInstall,
			Env: env, BackgroundCtx: ctx,
		}).NativeTool())
	}

	// Build the available models list (shared by subagent and llm_one_shot tools).
	// Resolved fresh on each ToolSet construction so new conversations see
	// custom models added since server start.
	var availableModels []AvailableModel
	if cfg.BuildAvailableModels != nil {
		availableModels = cfg.BuildAvailableModels()
	} else if cfg.LLMProvider != nil {
		for _, id := range cfg.LLMProvider.GetAvailableModels() {
			availableModels = append(availableModels, AvailableModel{ID: id})
		}
	}

	// Add subagent tool if configured and depth limit not reached.
	// MaxSubagentDepth of 0 means no limit; otherwise, only add if depth < max.
	canSpawnSubagents := cfg.SubagentRunner != nil && cfg.SubagentDB != nil && cfg.ParentConversationID != ""
	if canSpawnSubagents && (cfg.MaxSubagentDepth == 0 || cfg.SubagentDepth < cfg.MaxSubagentDepth) {
		nativeTools = append(nativeTools, dago.ConversationSubagentTool(cfg.SubagentDB, cfg.SubagentRunner, wd.Get, dago.ConversationSubagentOptions{
			ParentConversationID: cfg.ParentConversationID,
			ModelID:              cfg.ModelID,
			AvailableModels:      availableModels,
			ParentReasoning:      cfg.ReasoningLevel,
		}))
	}

	// Add LLM one-shot tool if LLM provider is configured
	if cfg.LLMProvider != nil {
		llmOneShotTool := &LLMOneShotTool{
			LLMProvider:     cfg.LLMProvider,
			ModelID:         cfg.ModelID,
			WorkingDir:      wd,
			AvailableModels: availableModels,
		}
		nativeTools = append(nativeTools, llmOneShotTool.NativeTool())
	}

	var cleanup func()
	anyBrowserToolEnabled := false
	for _, name := range []string{"browser", "read_image"} {
		if IsToolEnabled(name, cfg.ToolOverrides, cfg.DisableAllTools) {
			anyBrowserToolEnabled = true
			break
		}
	}
	if cfg.EnableBrowser && anyBrowserToolEnabled {
		nativeBrowserTools, browserCleanup := browse.RegisterBrowserTools(ctx)
		if len(nativeBrowserTools) > 0 {
			// If the model doesn't support image inputs, drop read_image — it
			// returns image content the model cannot consume. The `browser`
			// tool's screenshot action also returns images, but it self-gates
			// at run time via the native model profile in its tool context,
			// so the combined browser tool stays available.
			modelSupportsImages := true
			if cfg.LLMProvider != nil && cfg.ModelID != "" {
				if chat, err := cfg.LLMProvider.GetChat(cfg.ModelID); err == nil {
					modelSupportsImages = chat.Profile().SupportsImages
				}
			}
			for _, bt := range nativeBrowserTools {
				if bt.Definition().Name == "read_image" && !modelSupportsImages {
					continue
				}
				nativeTools = append(nativeTools, bt)
			}
		}
		cleanup = browserCleanup
	}

	definitions := make([]datool.Definition, 0, len(nativeTools)+1)
	nativeTools = FilterTools(nativeTools, cfg.ToolOverrides, cfg.DisableAllTools)
	for _, item := range nativeTools {
		definitions = append(definitions, item.Definition())
	}
	filesystemDefinitions := dagoFilesystemDefinitions(workingDir, filesystemTools)
	// Preserve the legacy settings label at the application metadata boundary
	// when it is the sole explicitly enabled command tool. Execution still uses
	// dago's canonical execute tool.
	if cfg.DisableAllTools && cfg.ToolOverrides["bash"] == "on" {
		if _, canonical := cfg.ToolOverrides["execute"]; !canonical {
			for index := range filesystemDefinitions {
				if filesystemDefinitions[index].Name == "execute" {
					filesystemDefinitions[index].Name = "bash"
				}
			}
		}
	}
	definitions = append(definitions, filesystemDefinitions...)

	// Add provider-hosted tools to display metadata. They are configured on the
	// model and are not dispatched through the local tool executor.
	if cfg.LLMProvider != nil && cfg.ModelID != "" {
		if chat, err := cfg.LLMProvider.GetChat(cfg.ModelID); err == nil {
			for _, definition := range serverSideTools(chat.Profile()) {
				if IsToolEnabled(definition.Name, cfg.ToolOverrides, cfg.DisableAllTools) {
					definitions = append(definitions, definition)
				}
			}
		}
	}
	return &ToolSet{
		definitions: definitions,
		nativeTools: nativeTools,
		filesystem:  filesystemTools,
		cleanup:     cleanup,
		wd:          wd,
	}
}

func selectedNativeFilesystemTools(overrides map[string]string, disableAll bool) []string {
	aliases := map[string]string{
		"execute":    "bash",
		"write_file": "patch", "edit_file": "patch", "delete": "patch",
		"glob": "keyword_search", "grep": "keyword_search",
	}
	names := []string{"ls", "read_file", "write_file", "edit_file", "delete", "glob", "grep", "execute"}
	selected := make([]string, 0, len(names))
	for _, name := range names {
		if value, exists := overrides[name]; exists && (value == "on" || value == "off") {
			if value == "on" {
				selected = append(selected, name)
			}
			continue
		}
		alias := aliases[name]
		if alias != "" {
			if IsToolEnabled(alias, overrides, disableAll) {
				selected = append(selected, name)
			}
			continue
		}
		if !disableAll {
			selected = append(selected, name)
		}
	}
	if len(selected) > 0 {
		hasRead := false
		for _, name := range selected {
			hasRead = hasRead || name == "read_file"
		}
		if !hasRead {
			selected = append([]string{"read_file"}, selected...)
		}
	}
	return selected
}

func dagoFilesystemDefinitions(root string, names []string) []datool.Definition {
	files, err := dbackend.NewLocalShell(dbackend.LocalShellOptions{Filesystem: dbackend.FilesystemOptions{Root: root}})
	if err != nil {
		return nil
	}
	compiled := dago.New(filesystemSchemaModel{}, dago.Options{
		Backend: files, Filesystem: dago.Filesystem{Tools: names}, DisableSubagents: true, DisableSummary: true,
	})
	tools := compiled.Tools()
	byName := make(map[string]datool.Definition, len(tools))
	for _, executable := range tools {
		definition := executable.Definition()
		byName[definition.Name] = definition
	}
	result := make([]datool.Definition, 0, len(names))
	for _, name := range names {
		if definition, ok := byName[name]; ok {
			result = append(result, definition)
		}
	}
	return result
}

type filesystemSchemaModel struct{}

func (filesystemSchemaModel) Invoke(context.Context, damodel.Request) (damodel.Response, error) {
	return damodel.Response{}, errors.New("filesystem schema model cannot be invoked")
}

func (filesystemSchemaModel) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return nil, errors.New("filesystem schema model cannot be streamed")
}

func (filesystemSchemaModel) Profile() damodel.Profile { return damodel.Profile{} }

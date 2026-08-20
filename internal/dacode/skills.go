package dacode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/daskill"
)

const (
	skillTrustDirectory = "skill-trust"
	skillTrustFilename  = "approvals.json"
)

func skillTrustPath(stateDir string) string {
	return filepath.Join(stateDir, skillTrustDirectory, skillTrustFilename)
}

type skillRunner interface {
	ListSkills(context.Context) ([]daskill.Entry, error)
	LoadSkill(context.Context, string) (daskill.Entry, error)
	TrustSkill(context.Context, string) error
}

type skillsLoadedMsg struct {
	entries []daskill.Entry
	err     error
}

type skillLoadedMsg struct {
	name, request, display string
	entry                  daskill.Entry
	err                    error
}

type skillTrustedMsg struct {
	name, request, display, target string
	err                            error
}

type skillTrustState struct {
	name, request, display, target string
	saving                         bool
}

func (runner *dagoRunner) ListSkills(ctx context.Context) ([]daskill.Entry, error) {
	return effectiveSkillEntries(ctx, newSkillManager(runner.workingDir, runner.stateDir, runner.AgentName()))
}

func (runner *dagoRunner) LoadSkill(ctx context.Context, name string) (daskill.Entry, error) {
	return loadEffectiveSkill(ctx, newSkillManager(runner.workingDir, runner.stateDir, runner.AgentName()), name)
}

func (runner *dagoRunner) TrustSkill(ctx context.Context, target string) error {
	return daskill.NewTrustStore(skillTrustPath(runner.stateDir)).Trust(ctx, target)
}

func listSkillsCommand(ctx context.Context, runner agentRunner) tea.Cmd {
	return func() tea.Msg {
		capability, ok := runner.(skillRunner)
		if !ok {
			return skillsLoadedMsg{err: errors.New("skills are unavailable")}
		}
		entries, err := capability.ListSkills(ctx)
		return skillsLoadedMsg{entries: entries, err: err}
	}
}

func loadSkillCommand(ctx context.Context, runner agentRunner, name, request, display string) tea.Cmd {
	return func() tea.Msg {
		capability, ok := runner.(skillRunner)
		if !ok {
			return skillLoadedMsg{name: name, request: request, display: display, err: errors.New("skills are unavailable")}
		}
		entry, err := capability.LoadSkill(ctx, name)
		return skillLoadedMsg{name: name, request: request, display: display, entry: entry, err: err}
	}
}

func trustSkillCommand(ctx context.Context, runner agentRunner, state skillTrustState) tea.Cmd {
	return func() tea.Msg {
		capability, ok := runner.(skillRunner)
		if !ok {
			return skillTrustedMsg{name: state.name, request: state.request, display: state.display, target: state.target, err: errors.New("skills are unavailable")}
		}
		err := capability.TrustSkill(ctx, state.target)
		return skillTrustedMsg{name: state.name, request: state.request, display: state.display, target: state.target, err: err}
	}
}

func (model *tuiModel) finishSkillsList(message skillsLoadedMsg) {
	if message.err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Could not list skills: " + message.err.Error()})
		model.refreshTranscript()
		return
	}
	lines := []string{"Available skills:"}
	for _, entry := range message.entries {
		description := entry.Skill.Description
		if entry.TrustRequired {
			description = "external symlink; approval required on invocation"
		}
		lines = append(lines, "- "+entry.Skill.Name+" ["+entry.Source+"] "+unicodeUIGlyphs.Dash+" "+description)
	}
	model.appendItem(transcriptItem{kind: itemNotice, text: strings.Join(lines, "\n")})
	model.refreshTranscript()
}

func (model *tuiModel) finishSkillLoad(message skillLoadedMsg) tea.Cmd {
	if message.err != nil {
		var trustErr *daskill.TrustRequiredError
		if errors.As(message.err, &trustErr) {
			model.skillTrust = &skillTrustState{name: message.name, request: message.request, display: message.display, target: trustErr.TargetDir}
			model.relayout()
			model.refreshTranscript()
			return nil
		}
		model.appendItem(transcriptItem{kind: itemError, text: "Could not load skill: " + message.err.Error()})
		model.refreshTranscript()
		return nil
	}
	model.appendItem(transcriptItem{
		kind: itemSkill, name: message.entry.Skill.Name, source: message.entry.Source,
		detail: message.entry.Skill.Description, request: message.request, text: message.entry.Skill.Body,
	})
	model.currentAssistant = -1
	prompt := "I'm invoking the skill `" + message.entry.Skill.Name + "`. Follow these full instructions to complete the task.\n\n---\n" +
		message.entry.Skill.Body + "\n---"
	if strings.TrimSpace(message.request) != "" {
		prompt += "\n\nUser request: " + message.request
	}
	return model.startStream(dagent.Input{
		Config:   dacheckpoint.Config{ThreadID: model.threadID},
		Messages: []damessage.Message{damessage.Human(prompt)}, SkipValueEvents: true,
	})
}

func (model *tuiModel) finishSkillTrust(message skillTrustedMsg) tea.Cmd {
	if model.skillTrust == nil || model.skillTrust.target != message.target {
		return nil
	}
	model.skillTrust = nil
	if message.err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Could not trust skill target: " + message.err.Error()})
		model.refreshTranscript()
		return nil
	}
	model.appendItem(transcriptItem{kind: itemNotice, text: "Trusted this exact skill target. Loading skill."})
	model.refreshTranscript()
	return loadSkillCommand(model.ctx, model.runner, message.name, message.request, message.display)
}

func (model *tuiModel) handleSkillTrustKey(message tea.KeyPressMsg) (tea.Cmd, bool) {
	state := model.skillTrust
	if state == nil {
		return nil, false
	}
	if state.saving {
		return nil, true
	}
	switch message.String() {
	case "enter", "y", "Y":
		state.saving = true
		return trustSkillCommand(model.ctx, model.runner, *state), true
	case "esc", "n", "N", "q", "ctrl+c":
		model.skillTrust = nil
		model.appendItem(transcriptItem{kind: itemNotice, text: "Skill invocation cancelled; no trust was granted."})
		model.relayout()
		model.refreshTranscript()
		return model.drainInputQueue(), true
	default:
		return nil, true
	}
}

func (model *tuiModel) renderSkillTrust() string {
	state := model.skillTrust
	if state == nil {
		return ""
	}
	contentWidth := min(max(model.width-16, 36), 80)
	hint := "Enter/Y to trust this exact target  " + model.glyphs.Bullet + "  N/Esc to cancel"
	if state.saving {
		hint = "Saving trust" + model.glyphs.Ellipsis
	}
	body := "The skill `" + state.name + "` is a symbolic link outside configured skill directories.\n\nResolved target:\n" + state.target + "\n\nOnly approve if you recognize and trust this directory. Future link changes require a new approval."
	lines := []string{
		lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Align(lipgloss.Center).Width(contentWidth).Render("Trust external skill?"), "",
		lipgloss.NewStyle().Foreground(colorBody).Width(contentWidth).Render(body), "",
		lipgloss.NewStyle().Foreground(colorMuted).Italic(true).Align(lipgloss.Center).Width(contentWidth).Render(hint),
	}
	panel := lipgloss.NewStyle().Border(model.uiBorder()).BorderForeground(colorWarning).Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(model.width, model.height, lipgloss.Center, lipgloss.Center, panel)
}

type skillCLIOptions struct {
	workingDir string
	stateDir   string
	agent      string
	project    bool
	json       bool
}

func defaultSkillCLIOptions() (skillCLIOptions, error) {
	workingDir, err := os.Getwd()
	if err != nil {
		return skillCLIOptions{}, err
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return skillCLIOptions{}, err
	}
	return skillCLIOptions{workingDir: workingDir, stateDir: filepath.Join(configDir, "dacode"), agent: defaultAgentName}, nil
}

func skillSources(workingDir, stateDir, agent string) []daskill.Source {
	home, _ := os.UserHomeDir()
	return []daskill.Source{
		{Name: "agent-profile", Dir: filepath.Join(stateDir, agent, agentSkillsDirectory)},
		{Name: "user-agents", Dir: filepath.Join(home, ".agents", "skills")},
		{Name: "project-deepagents", Dir: filepath.Join(workingDir, ".deepagents", "skills")},
		{Name: "project-agents", Dir: filepath.Join(workingDir, ".agents", "skills")},
		{Name: "user-claude", Dir: filepath.Join(home, ".claude", "skills")},
		{Name: "project-claude", Dir: filepath.Join(workingDir, ".claude", "skills")},
	}
}

func newSkillManager(workingDir, stateDir, agent string) *daskill.Manager {
	return daskill.NewManager(skillSources(workingDir, stateDir, agent), daskill.NewTrustStore(skillTrustPath(stateDir)), daskill.ManagerOptions{})
}

func runSkillsCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	defaults, err := defaultSkillCLIOptions()
	if err != nil {
		return err
	}
	if len(arguments) == 0 {
		arguments = []string{"list"}
	}
	command := arguments[0]
	if command == "ls" {
		command = "list"
	}
	if command == "trust" {
		return runSkillTrustCommand(ctx, defaults, arguments[1:], stdout, stderr)
	}
	if command == "inspect-thread" {
		return runThreadInspectorCommand(ctx, defaults, arguments[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("skills "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := defaults
	flags.StringVar(&options.workingDir, "working-dir", defaults.workingDir, "workspace directory")
	flags.StringVar(&options.stateDir, "state-dir", defaults.stateDir, "local state directory")
	flags.StringVar(&options.agent, "agent", defaults.agent, "named agent profile")
	flags.BoolVar(&options.project, "project", false, "use the project .agents/skills directory")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	workingDir, err := filepath.Abs(options.workingDir)
	if err != nil {
		return err
	}
	if err := validateAgentName(options.agent); err != nil {
		return err
	}
	manager := newSkillManager(workingDir, options.stateDir, options.agent)
	switch command {
	case "list":
		return listSkillsCLI(ctx, manager, options.json, stdout)
	case "info":
		if flags.NArg() != 1 {
			return errors.New("usage: dacode skills info NAME")
		}
		return skillInfoCLI(ctx, manager, flags.Arg(0), options.json, stdout)
	case "create":
		if flags.NArg() != 1 {
			return errors.New("usage: dacode skills create NAME [--project]")
		}
		root := writableSkillRoot(workingDir, options.stateDir, options.agent, options.project)
		path, err := daskill.CreateTemplate(root, flags.Arg(0))
		if err != nil {
			return err
		}
		if options.json {
			return json.NewEncoder(stdout).Encode(map[string]string{"path": path})
		}
		_, err = fmt.Fprintln(stdout, path)
		return err
	case "delete":
		if flags.NArg() != 1 {
			return errors.New("usage: dacode skills delete NAME [--project]")
		}
		return deleteSkillCLI(workingDir, options.stateDir, options.agent, flags.Arg(0), options.project, options.json, stdout)
	default:
		return fmt.Errorf("unknown skills command %q (use list, create, info, delete, trust, or inspect-thread)", command)
	}
}

func listSkillsCLI(ctx context.Context, manager *daskill.Manager, asJSON bool, stdout io.Writer) error {
	entries, err := effectiveSkillEntries(ctx, manager)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(entries)
	}
	for _, entry := range entries {
		description := entry.Skill.Description
		if entry.TrustRequired {
			description = "external symlink requires trust"
		}
		if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\n", entry.Skill.Name, entry.Source, description); err != nil {
			return err
		}
	}
	return nil
}

func effectiveSkillEntries(ctx context.Context, manager *daskill.Manager) ([]daskill.Entry, error) {
	selected := make(map[string]daskill.Entry)
	for _, skill := range daskill.Builtins() {
		selected[skill.Name] = daskill.Entry{Skill: skill, Source: "built-in"}
	}
	entries, err := manager.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		selected[entry.Skill.Name] = entry
	}
	result := make([]daskill.Entry, 0, len(selected))
	for _, entry := range selected {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Skill.Name < result[j].Skill.Name })
	return result, nil
}

func skillInfoCLI(ctx context.Context, manager *daskill.Manager, name string, asJSON bool, stdout io.Writer) error {
	entry, err := loadEffectiveSkill(ctx, manager, name)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(entry)
	}
	_, err = fmt.Fprintf(stdout, "Name: %s\nSource: %s\nDescription: %s\nPath: %s\n\n%s\n", entry.Skill.Name, entry.Source, entry.Skill.Description, entry.Skill.Path, entry.Skill.Body)
	return err
}

func loadEffectiveSkill(ctx context.Context, manager *daskill.Manager, name string) (daskill.Entry, error) {
	entry, err := manager.Load(ctx, name)
	if err == nil {
		return entry, nil
	}
	var trustErr *daskill.TrustRequiredError
	if errors.As(err, &trustErr) {
		return entry, err
	}
	for _, skill := range daskill.Builtins() {
		if skill.Name == name {
			return daskill.Entry{Skill: skill, Source: "built-in"}, nil
		}
	}
	return daskill.Entry{}, err
}

func writableSkillRoot(workingDir, stateDir, agent string, project bool) string {
	if project {
		return filepath.Join(workingDir, ".deepagents", "skills")
	}
	return filepath.Join(stateDir, agent, agentSkillsDirectory)
}

func deleteSkillCLI(workingDir, stateDir, agent, name string, project, asJSON bool, stdout io.Writer) error {
	if err := daskill.ValidateName(name); err != nil {
		return err
	}
	root := writableSkillRoot(workingDir, stateDir, agent, project)
	dir := filepath.Join(root, name)
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || daskill.FindFile(dir) == "" {
		return errors.New("refusing to delete a symlink or non-skill directory")
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(map[string]string{"deleted": dir})
	}
	_, err = fmt.Fprintln(stdout, dir)
	return err
}

func runSkillTrustCommand(ctx context.Context, defaults skillCLIOptions, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("skills trust", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateDir := defaults.stateDir
	asJSON := false
	flags.StringVar(&stateDir, "state-dir", defaults.stateDir, "local state directory")
	flags.BoolVar(&asJSON, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	operation := "list"
	if flags.NArg() > 0 {
		operation = flags.Arg(0)
	}
	store := daskill.NewTrustStore(skillTrustPath(stateDir))
	switch operation {
	case "list":
		if flags.NArg() != 0 && flags.NArg() != 1 {
			return errors.New("usage: dacode skills trust list")
		}
		records, err := store.List(ctx)
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(stdout).Encode(records)
		}
		for _, record := range records {
			if _, err := fmt.Fprintln(stdout, record.Path); err != nil {
				return err
			}
		}
		return nil
	case "add":
		if flags.NArg() != 2 {
			return errors.New("usage: dacode skills trust add PATH")
		}
		return store.Trust(ctx, flags.Arg(1))
	case "revoke":
		if flags.NArg() != 2 {
			return errors.New("usage: dacode skills trust revoke PATH")
		}
		return store.Revoke(ctx, flags.Arg(1))
	case "clear":
		if flags.NArg() != 1 {
			return errors.New("usage: dacode skills trust clear")
		}
		return store.Clear(ctx)
	default:
		return fmt.Errorf("unknown trust command %q", operation)
	}
}

func runThreadInspectorCommand(ctx context.Context, defaults skillCLIOptions, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("skills inspect-thread", flag.ContinueOnError)
	flags.SetOutput(stderr)
	databasePath := filepath.Join(defaults.stateDir, "threads.db")
	mode := "summary"
	flags.StringVar(&databasePath, "db", databasePath, "local checkpoint database")
	flags.StringVar(&mode, "mode", mode, "summary, transcript, or latest-turn")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return err
	}
	dsn := (&url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=ro"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer database.Close()
	inspector := daskill.NewThreadInspector(sqlite.New(database), daskill.ThreadInspectorOptions{})
	if flags.NArg() == 0 {
		threads, err := inspector.List(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(threads)
	}
	if flags.NArg() != 1 {
		return errors.New("usage: dacode skills inspect-thread [THREAD_ID] [--mode summary|transcript|latest-turn]")
	}
	messages, err := inspector.Inspect(ctx, flags.Arg(0))
	if err != nil {
		return err
	}
	switch mode {
	case "transcript":
		return json.NewEncoder(stdout).Encode(messages)
	case "latest-turn":
		messages = latestThreadTurn(messages)
		return json.NewEncoder(stdout).Encode(messages)
	case "summary":
		summary := struct {
			ThreadID string `json:"thread_id"`
			Messages int    `json:"messages"`
			Preview  string `json:"preview,omitempty"`
		}{ThreadID: flags.Arg(0), Messages: len(messages)}
		for index := len(messages) - 1; index >= 0; index-- {
			if text := strings.TrimSpace(messages[index].TextContent()); text != "" {
				summary.Preview = text
				if runes := []rune(summary.Preview); len(runes) > 500 {
					summary.Preview = string(runes[:500]) + "..."
				}
				break
			}
		}
		return json.NewEncoder(stdout).Encode(summary)
	default:
		return fmt.Errorf("unsupported inspection mode %q", mode)
	}
}

func latestThreadTurn(messages []damessage.Message) []damessage.Message {
	start := 0
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == damessage.RoleHuman {
			start = index
			break
		}
	}
	return messages[start:]
}

package damanaged

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/semistrict/dago/daskill"
)

const (
	maxProjectFileBytes = 1 << 20
	maxProjectBytes     = 8 << 20
	maxProjectEntries   = 256
	maxProjectChildren  = 32
)

// Project is a validated managed-agent project loaded from local files.
type Project struct {
	Root         string
	Name         string
	Description  string
	AgentID      string
	Model        string
	Runtime      map[string]any
	Backend      map[string]any
	Permissions  map[string]any
	Extras       map[string]any
	SystemPrompt string
	Tools        map[string]any
	ToolsText    string
	Skills       []ProjectSkill
	Subagents    []ProjectSubagent
}

// ProjectSkill is one validated inline project skill.
type ProjectSkill struct {
	Name         string
	Description  string
	Instructions string
	SkillFile    string
	Files        map[string]string
}

// ProjectSubagent is one validated filesystem subagent.
type ProjectSubagent struct {
	Name         string
	Description  string
	ModelID      string
	Instructions string
	Tools        map[string]any
	ToolsText    string
	ExtraFiles   map[string]string
}

// LoadProject validates and loads a managed-agent project from root. All
// filesystem work is bounded and linked or special entries fail closed.
func LoadProject(root string) (Project, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return Project{}, fmt.Errorf("resolve managed project: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return Project{}, fmt.Errorf("inspect managed project: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return Project{}, errors.New("managed project root must be a real directory")
	}
	opened, err := os.OpenRoot(absolute)
	if err != nil {
		return Project{}, fmt.Errorf("open managed project: %w", err)
	}
	defer opened.Close()
	loader := projectLoader{root: opened, remainingBytes: maxProjectBytes, remainingEntries: maxProjectEntries}
	return loader.load(absolute)
}

type projectLoader struct {
	root             *os.Root
	remainingBytes   int
	remainingEntries int
}

func (loader *projectLoader) load(absolute string) (Project, error) {
	agentText, err := loader.readRegular("agent.json", true)
	if err != nil {
		return Project{}, err
	}
	var config struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		AgentID     string         `json:"agent_id"`
		Model       string         `json:"model"`
		Runtime     map[string]any `json:"runtime"`
		Backend     map[string]any `json:"backend"`
		Permissions map[string]any `json:"permissions"`
		Extras      map[string]any `json:"extras"`
	}
	decoder := json.NewDecoder(strings.NewReader(agentText))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&config); err != nil {
		return Project{}, fmt.Errorf("decode agent.json: %w", err)
	}
	var rawConfig map[string]json.RawMessage
	if err := json.Unmarshal([]byte(agentText), &rawConfig); err != nil || rawConfig == nil {
		return Project{}, errors.New("agent.json must contain one JSON object")
	}
	if err := validateProjectScalar("name", config.Name, true, 128); err != nil {
		return Project{}, err
	}
	if err := validateProjectScalar("description", config.Description, false, 4096); err != nil {
		return Project{}, err
	}
	if err := validateProjectScalar("agent_id", config.AgentID, false, 256); err != nil {
		return Project{}, err
	}
	if err := validateProjectScalar("model", config.Model, false, 512); err != nil {
		return Project{}, err
	}
	for _, field := range []string{"agent_id", "model"} {
		if _, exists := rawConfig[field]; exists {
			value := config.AgentID
			if field == "model" {
				value = config.Model
			}
			if strings.TrimSpace(value) == "" {
				return Project{}, fmt.Errorf("project %s must be non-empty when provided", field)
			}
		}
	}
	if config.Model != "" && config.Runtime != nil {
		return Project{}, errors.New("agent.json must use either model or runtime, not both")
	}
	if config.Runtime != nil {
		if _, legacy := config.Runtime["backend_type"]; legacy {
			return Project{}, errors.New("runtime.backend_type is no longer supported; use top-level backend")
		}
	}
	config.Backend, err = normalizeProjectBackend(config.Backend)
	if err != nil {
		return Project{}, err
	}
	if err := validateProjectPermissions(config.Permissions); err != nil {
		return Project{}, err
	}
	prompt, err := loader.readRegular("AGENTS.md", true)
	if err != nil {
		return Project{}, err
	}
	if strings.TrimSpace(prompt) == "" {
		return Project{}, errors.New("AGENTS.md must not be empty")
	}
	project := Project{
		Root: absolute, Name: strings.TrimSpace(config.Name), Description: strings.TrimSpace(config.Description),
		AgentID: strings.TrimSpace(config.AgentID), Model: strings.TrimSpace(config.Model), Runtime: config.Runtime,
		Backend: config.Backend, Permissions: config.Permissions, Extras: config.Extras, SystemPrompt: prompt,
	}
	toolsText, err := loader.readRegular("tools.json", false)
	if err != nil {
		return Project{}, err
	}
	if toolsText != "" {
		var tools map[string]any
		if err := json.Unmarshal([]byte(toolsText), &tools); err != nil || tools == nil {
			return Project{}, errors.New("tools.json must contain one JSON object")
		}
		if err := validateProjectTools(tools, "tools.json"); err != nil {
			return Project{}, err
		}
		project.Tools, project.ToolsText = tools, toolsText
	}
	project.Skills, err = loader.loadSkills()
	if err != nil {
		return Project{}, err
	}
	project.Subagents, err = loader.loadSubagents()
	if err != nil {
		return Project{}, err
	}
	return project, nil
}

func (loader *projectLoader) loadSkills() ([]ProjectSkill, error) {
	names, err := loader.childDirectories("skills")
	if err != nil {
		return nil, err
	}
	var skills []ProjectSkill
	for _, name := range names {
		base := path.Join("skills", name)
		content, err := loader.readRegular(path.Join(base, "SKILL.md"), true)
		if err != nil {
			return nil, err
		}
		parsed, _, err := daskill.ParseContent(content, path.Join(base, "SKILL.md"))
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", base, err)
		}
		if parsed.Name != name {
			return nil, fmt.Errorf("skill %s name must match its directory", name)
		}
		files, err := loader.readTree(base, map[string]bool{"SKILL.md": true})
		if err != nil {
			return nil, err
		}
		skills = append(skills, ProjectSkill{
			Name: parsed.Name, Description: parsed.Description, Instructions: parsed.Body,
			SkillFile: content, Files: files,
		})
	}
	return skills, nil
}

func (loader *projectLoader) loadSubagents() ([]ProjectSubagent, error) {
	names, err := loader.childDirectories("subagents")
	if err != nil {
		return nil, err
	}
	var subagents []ProjectSubagent
	for _, name := range names {
		if err := validateProjectScalar("subagent name", name, true, 128); err != nil {
			return nil, err
		}
		base := path.Join("subagents", name)
		configText, err := loader.readRegular(path.Join(base, "agent.json"), true)
		if err != nil {
			return nil, err
		}
		var config struct {
			Description string `json:"description"`
			Model       string `json:"model"`
		}
		decoder := json.NewDecoder(strings.NewReader(configText))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&config); err != nil {
			return nil, fmt.Errorf("decode %s/agent.json: %w", base, err)
		}
		instructions, err := loader.readRegular(path.Join(base, "AGENTS.md"), true)
		if err != nil {
			return nil, err
		}
		toolsText, err := loader.readRegular(path.Join(base, "tools.json"), false)
		if err != nil {
			return nil, err
		}
		var tools map[string]any
		if toolsText != "" {
			if err := json.Unmarshal([]byte(toolsText), &tools); err != nil || tools == nil {
				return nil, fmt.Errorf("%s/tools.json must contain one JSON object", base)
			}
			if err := validateProjectTools(tools, base+"/tools.json"); err != nil {
				return nil, err
			}
		}
		extra, err := loader.readTree(base, map[string]bool{"agent.json": true, "AGENTS.md": true, "tools.json": true})
		if err != nil {
			return nil, err
		}
		subagents = append(subagents, ProjectSubagent{
			Name: name, Description: strings.TrimSpace(config.Description), ModelID: strings.TrimSpace(config.Model),
			Instructions: instructions, Tools: tools, ToolsText: toolsText, ExtraFiles: extra,
		})
	}
	return subagents, nil
}

func (loader *projectLoader) childDirectories(parent string) ([]string, error) {
	info, err := loader.root.Lstat(parent)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect project directory %s: %w", parent, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("project path %s must be a real directory", parent)
	}
	entries, err := fs.ReadDir(loader.root.FS(), parent)
	if err != nil {
		return nil, fmt.Errorf("read project directory %s: %w", parent, err)
	}
	if len(entries) > maxProjectChildren {
		return nil, fmt.Errorf("project directory %s has too many entries", parent)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		entryInfo, err := loader.root.Lstat(path.Join(parent, entry.Name()))
		if err != nil {
			return nil, err
		}
		if entryInfo.Mode()&fs.ModeSymlink != 0 || !entryInfo.IsDir() {
			return nil, fmt.Errorf("project entry %s/%s must be a real directory", parent, entry.Name())
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func (loader *projectLoader) readTree(base string, excluded map[string]bool) (map[string]string, error) {
	result := map[string]string{}
	err := fs.WalkDir(loader.root.FS(), base, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == base {
			return nil
		}
		relative := strings.TrimPrefix(filePath, base+"/")
		if excluded[relative] {
			return nil
		}
		info, err := loader.root.Lstat(filePath)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("project entry %s must be a regular file or directory", filePath)
		}
		if info.IsDir() {
			if strings.Count(relative, "/") >= 8 {
				return fmt.Errorf("project entry %s exceeds depth limit", filePath)
			}
			return nil
		}
		content, err := loader.readRegular(filePath, true)
		if err != nil {
			return err
		}
		result[relative] = content
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read project tree %s: %w", base, err)
	}
	return result, nil
}

func (loader *projectLoader) readRegular(filePath string, required bool) (string, error) {
	info, err := loader.root.Lstat(filePath)
	if errors.Is(err, fs.ErrNotExist) && !required {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect project file %s: %w", filePath, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("project file %s must be regular", filePath)
	}
	if info.Size() > maxProjectFileBytes || info.Size() > int64(loader.remainingBytes) {
		return "", fmt.Errorf("project file %s exceeds size limit", filePath)
	}
	if loader.remainingEntries <= 0 {
		return "", errors.New("managed project exceeds entry limit")
	}
	content, err := loader.root.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("read project file %s: %w", filePath, err)
	}
	if !utf8.Valid(content) {
		return "", fmt.Errorf("project file %s is not UTF-8 text", filePath)
	}
	loader.remainingEntries--
	loader.remainingBytes -= len(content)
	return string(content), nil
}

func validateProjectScalar(label, value string, required bool, limit int) error {
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return fmt.Errorf("project %s is required", label)
	}
	if len(trimmed) > limit || strings.ContainsAny(trimmed, "\x00\r\n") {
		return fmt.Errorf("project %s is invalid", label)
	}
	return nil
}

func validateProjectTools(tools map[string]any, source string) error {
	items, ok := tools["tools"].([]any)
	if !ok {
		return fmt.Errorf("%s tools must be an array", source)
	}
	if len(items) > 256 {
		return fmt.Errorf("%s has too many tools", source)
	}
	for index, item := range items {
		tool, ok := item.(map[string]any)
		name, nameOK := tool["name"].(string)
		if !ok || !nameOK || strings.TrimSpace(name) == "" || len(name) > 256 || strings.ContainsAny(name, "\x00\r\n") {
			return fmt.Errorf("%s tools[%d] is invalid", source, index)
		}
	}
	return nil
}

func validateProjectPermissions(permissions map[string]any) error {
	if permissions == nil {
		return nil
	}
	allowed := map[string]map[string]bool{
		"identity":            {"personal": true, "shared": true},
		"visibility":          {"tenant": true, "user": true},
		"tenant_access_level": {"read": true, "run": true, "write": true},
	}
	for key, values := range allowed {
		if value, exists := permissions[key]; exists && value != nil {
			text, ok := value.(string)
			if !ok || !values[text] {
				return fmt.Errorf("permissions.%s is invalid", key)
			}
		}
	}
	return nil
}

func normalizeProjectBackend(backend map[string]any) (map[string]any, error) {
	if backend == nil {
		return nil, nil
	}
	backendType, _ := backend["type"].(string)
	if value, exists := backend["type"]; exists {
		if _, ok := value.(string); !ok {
			return nil, errors.New("backend.type must be a string")
		}
	}
	if backendType == "default" {
		backendType = "state"
		backend["type"] = backendType
	}
	defaultScope := ""
	switch backendType {
	case "", "state":
		if backend["sandbox"] != nil || backend["sandbox_config"] != nil {
			return nil, errors.New("sandbox settings require a sandbox backend type")
		}
		return backend, nil
	case "thread_scoped_sandbox":
		defaultScope = "thread"
	case "agent_scoped_sandbox":
		defaultScope = "agent"
	case "sandbox":
		defaultScope = "thread"
	default:
		return nil, fmt.Errorf("backend.type %q is invalid", backendType)
	}
	configuration := map[string]any{}
	for _, key := range []string{"sandbox", "sandbox_config"} {
		if value := backend[key]; value != nil {
			values, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("backend.%s must be an object", key)
			}
			for field, item := range values {
				configuration[field] = item
			}
		}
		delete(backend, key)
	}
	if scope, exists := configuration["scope"]; exists {
		if scope != "thread" && scope != "agent" {
			return nil, errors.New("backend sandbox scope must be thread or agent")
		}
	} else {
		configuration["scope"] = defaultScope
	}
	if policies, exists := configuration["policy_ids"]; exists {
		items, ok := policies.([]any)
		if !ok {
			return nil, errors.New("backend sandbox policy_ids must be an array")
		}
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return nil, errors.New("backend sandbox policy_ids must contain strings")
			}
		}
	}
	for _, field := range []string{"idle_ttl_seconds", "delete_after_stop_seconds"} {
		if value, exists := configuration[field]; exists {
			number, ok := value.(json.Number)
			if !ok {
				return nil, fmt.Errorf("backend sandbox %s must be an integer", field)
			}
			if _, err := number.Int64(); err != nil {
				return nil, fmt.Errorf("backend sandbox %s must be an integer", field)
			}
		}
	}
	backend["type"] = "sandbox"
	backend["sandbox_config"] = configuration
	return backend, nil
}

// CreatePayload builds the pinned initial agent payload.
func (project Project) CreatePayload() map[string]any {
	payload := project.MetadataPayload()
	payload["system_prompt"] = project.SystemPrompt
	if project.Tools != nil {
		payload["tools"] = project.Tools
	}
	if len(project.Skills) > 0 {
		skills := make([]map[string]any, 0, len(project.Skills))
		for _, skill := range project.Skills {
			item := map[string]any{"type": "inline", "name": skill.Name, "description": skill.Description, "instructions": skill.Instructions}
			if len(skill.Files) > 0 {
				item["files"] = skill.Files
			}
			skills = append(skills, item)
		}
		payload["skills"] = skills
	}
	if len(project.Subagents) > 0 {
		subagents := make([]map[string]any, 0, len(project.Subagents))
		for _, subagent := range project.Subagents {
			item := map[string]any{"name": subagent.Name, "instructions": subagent.Instructions}
			if subagent.Description != "" {
				item["description"] = subagent.Description
			}
			if subagent.ModelID != "" {
				item["model_id"] = subagent.ModelID
			}
			if subagent.Tools != nil {
				item["tools"] = subagent.Tools
			}
			subagents = append(subagents, item)
		}
		payload["subagents"] = subagents
	}
	return payload
}

// MetadataPayload builds the update-safe metadata-only payload.
func (project Project) MetadataPayload() map[string]any {
	payload := map[string]any{"name": project.Name}
	if project.Description != "" {
		payload["description"] = project.Description
	}
	if project.Runtime != nil {
		payload["runtime"] = project.Runtime
	} else if project.Model != "" {
		payload["runtime"] = map[string]any{"model": map[string]any{"model_id": project.Model}}
	}
	if project.Backend != nil {
		payload["backend"] = project.Backend
	}
	if project.Permissions != nil {
		payload["permissions"] = project.Permissions
	}
	if project.Extras != nil {
		payload["extras"] = project.Extras
	}
	return payload
}

// DirectoryFiles returns the complete caller-owned managed directory projection.
func (project Project) DirectoryFiles() map[string]string {
	files := map[string]string{"AGENTS.md": project.SystemPrompt}
	if project.ToolsText != "" {
		files["tools.json"] = project.ToolsText
	}
	for _, skill := range project.Skills {
		base := path.Join("skills", skill.Name)
		files[path.Join(base, "SKILL.md")] = skill.SkillFile
		for relative, content := range skill.Files {
			files[path.Join(base, relative)] = content
		}
	}
	for _, subagent := range project.Subagents {
		base := path.Join("subagents", subagent.Name)
		frontmatter := "---\n"
		if subagent.Description != "" {
			encoded, _ := json.Marshal(subagent.Description)
			frontmatter += "description: " + string(encoded) + "\n"
		}
		if subagent.ModelID != "" {
			encoded, _ := json.Marshal(subagent.ModelID)
			frontmatter += "model_id: " + string(encoded) + "\n"
		}
		files[path.Join(base, "AGENTS.md")] = frontmatter + "---\n\n" + subagent.Instructions
		if subagent.ToolsText != "" {
			files[path.Join(base, "tools.json")] = subagent.ToolsText
		}
		for relative, content := range subagent.ExtraFiles {
			files[path.Join(base, relative)] = content
		}
	}
	return files
}

package dacode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/semistrict/dago/dabackend"
)

const (
	userAgentSkillsMount  = "/skill-sources/user-agents"
	userClaudeSkillsMount = "/skill-sources/user-claude"
)

var errReadOnlySkillSource = errors.New("skill source is read-only")

type readOnlySkillBackend struct{ dabackend.Backend }

func (backend readOnlySkillBackend) Write(context.Context, string, string) (dabackend.WriteResult, error) {
	return dabackend.WriteResult{}, errReadOnlySkillSource
}

func (backend readOnlySkillBackend) Edit(context.Context, string, string, string, bool) (dabackend.EditResult, error) {
	return dabackend.EditResult{}, errReadOnlySkillSource
}

func (backend readOnlySkillBackend) Delete(context.Context, string) (dabackend.DeleteResult, error) {
	return dabackend.DeleteResult{}, errReadOnlySkillSource
}

func (backend readOnlySkillBackend) Upload(_ context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	results := make([]dabackend.UploadResult, len(uploads))
	for index, upload := range uploads {
		results[index] = dabackend.UploadResult{Path: upload.Path, Error: "read_only"}
	}
	return results
}

func runtimeUserSkillRoutes(home string) (map[string]dabackend.Backend, bool, bool, error) {
	if strings.TrimSpace(home) == "" {
		panic("dacode: user home is empty")
	}
	routes := make(map[string]dabackend.Backend, 2)
	add := func(mount, root string) (bool, error) {
		info, statErr := os.Stat(root)
		if errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		if statErr != nil {
			return false, statErr
		}
		if !info.IsDir() {
			return false, errors.New("skill source is not a directory")
		}
		files, openErr := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: root, MaxFileSize: 1 << 20, MaxResults: 1_000})
		if openErr != nil {
			return false, openErr
		}
		routes[mount] = readOnlySkillBackend{Backend: files}
		return true, nil
	}
	userAgents, err := add(userAgentSkillsMount, filepath.Join(home, ".agents", "skills"))
	if err != nil {
		return nil, false, false, err
	}
	userClaude, err := add(userClaudeSkillsMount, filepath.Join(home, ".claude", "skills"))
	if err != nil {
		return nil, false, false, err
	}
	return routes, userAgents, userClaude, nil
}

func orderedRuntimeSkillSources(root string, userAgents, userClaude bool) []string {
	sources := []string{agentMemoryMount + "/" + agentSkillsDirectory}
	if userAgents {
		sources = append(sources, userAgentSkillsMount)
	}
	sources = append(sources, existingVirtualDirectories(root, ".deepagents/skills", ".agents/skills")...)
	if userClaude {
		sources = append(sources, userClaudeSkillsMount)
	}
	return append(sources, existingVirtualDirectories(root, ".claude/skills")...)
}

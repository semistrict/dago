package dacode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/semistrict/dago/dabackend"
)

const (
	agentMemoryMount          = "/agent-memory"
	agentMemorySourcePath     = agentMemoryMount + "/" + agentInstructionsFilename
	sessionAgentNameKey       = "__dacode_agent_name"
	sessionAgentGenerationKey = "__dacode_agent_generation"
)

type agentMemoryBackend struct {
	stateDir string
	files    *dabackend.Filesystem
	identity *agentIdentity
}

type agentMemoryContextKey struct{ backend *agentMemoryBackend }

func openAgentMemory(stateDir string, identity *agentIdentity) (*agentMemoryBackend, error) {
	if identity == nil {
		panic("agent memory identity is nil")
	}
	if err := ensureAgentMemoryFile(stateDir, defaultAgentName); err != nil {
		return nil, err
	}
	name := identity.current()
	if name != defaultAgentName {
		if err := ensureAgentMemoryFile(stateDir, name); err != nil {
			return nil, err
		}
	}
	files, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: stateDir})
	if err != nil {
		return nil, fmt.Errorf("open agent memory storage: %w", err)
	}
	return &agentMemoryBackend{stateDir: stateDir, files: files, identity: identity}, nil
}

func ensureAgentMemoryFile(stateDir, name string) error {
	if err := validateAgentName(name); err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create agent memory storage: %w", err)
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return fmt.Errorf("open agent memory storage: %w", err)
	}
	defer root.Close()
	if err := root.MkdirAll(name, 0o700); err != nil {
		return fmt.Errorf("create agent %q memory directory: %w", name, err)
	}
	directoryInfo, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("inspect agent %q memory directory: %w", name, err)
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return fmt.Errorf("agent %q memory directory is not a confined directory", name)
	}
	for _, child := range []string{agentSkillsDirectory, agentSessionsDirectory} {
		childPath := filepath.Join(name, child)
		if err := root.MkdirAll(childPath, 0o700); err != nil {
			return fmt.Errorf("create agent %q %s directory: %w", name, child, err)
		}
		info, err := root.Lstat(childPath)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			if err == nil {
				err = errors.New("path is not a confined directory")
			}
			return fmt.Errorf("inspect agent %q %s directory: %w", name, child, err)
		}
	}
	filePath := filepath.Join(name, agentInstructionsFilename)
	fileInfo, err := root.Lstat(filePath)
	if err == nil {
		if fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() {
			return fmt.Errorf("agent %q memory is not a regular file", name)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect agent %q memory: %w", name, err)
	}
	file, err := root.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create agent %q memory: %w", name, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync agent %q memory: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close agent %q memory: %w", name, err)
	}
	directory, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("open agent %q memory directory: %w", name, err)
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return fmt.Errorf("sync agent %q memory directory: %w", name, err)
	}
	if closeErr != nil {
		return fmt.Errorf("close agent %q memory directory: %w", name, closeErr)
	}
	return nil
}

func (backend *agentMemoryBackend) selectedName(ctx context.Context) (string, error) {
	name, _ := ctx.Value(agentMemoryContextKey{backend: backend}).(string)
	if name == "" {
		name = backend.identity.current()
	}
	if err := validateAgentName(name); err != nil {
		return "", fmt.Errorf("select agent memory: %w", err)
	}
	return name, nil
}

func (backend *agentMemoryBackend) selectedPath(ctx context.Context, virtualPath string) (string, error) {
	if len(virtualPath) > 4096 || strings.Count(virtualPath, "/") > 32 {
		return "", fmt.Errorf("agent memory path exceeds finite bounds")
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(virtualPath, "/"))
	if cleaned != virtualPath {
		return "", fmt.Errorf("agent memory path is not canonical")
	}
	relative := ""
	switch {
	case cleaned == "/"+agentInstructionsFilename:
		relative = agentInstructionsFilename
	case cleaned == "/"+agentSkillsDirectory || strings.HasPrefix(cleaned, "/"+agentSkillsDirectory+"/"):
		relative = strings.TrimPrefix(cleaned, "/")
	default:
		return "", fmt.Errorf("agent memory exposes only /%s and /%s", agentInstructionsFilename, agentSkillsDirectory)
	}
	name, err := backend.selectedName(ctx)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(backend.stateDir)
	if err != nil {
		return "", fmt.Errorf("open agent memory storage: %w", err)
	}
	defer root.Close()
	directoryInfo, err := root.Lstat(name)
	if err != nil {
		return "", fmt.Errorf("inspect agent %q memory directory: %w", name, err)
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return "", fmt.Errorf("agent %q memory directory is not confined", name)
	}
	filePath := filepath.Join(name, filepath.FromSlash(relative))
	components := strings.Split(filepath.ToSlash(relative), "/")
	current := name
	for index, component := range components {
		current = filepath.Join(current, filepath.FromSlash(component))
		info, statErr := root.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if statErr != nil {
			return "", fmt.Errorf("inspect agent %q profile path: %w", name, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("agent %q profile path is a symbolic link", name)
		}
		if index < len(components)-1 && !info.IsDir() {
			return "", fmt.Errorf("agent %q profile parent is not a directory", name)
		}
	}
	if info, statErr := root.Lstat(filePath); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("agent %q profile path is a symbolic link", name)
		}
		if relative == agentInstructionsFilename && !info.Mode().IsRegular() {
			return "", fmt.Errorf("agent %q memory is not a regular file", name)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("inspect agent %q memory: %w", name, statErr)
	}
	return "/" + filepath.ToSlash(filePath), nil
}

func (backend *agentMemoryBackend) List(ctx context.Context, directory string) (dabackend.ListResult, error) {
	if directory != "/" {
		selected, err := backend.selectedPath(ctx, directory)
		if err != nil {
			return dabackend.ListResult{}, err
		}
		result, err := backend.files.List(ctx, selected)
		if err != nil {
			return dabackend.ListResult{}, err
		}
		prefix := strings.TrimSuffix(selected, "/")
		for index := range result.Entries {
			result.Entries[index].Path = strings.TrimPrefix(result.Entries[index].Path, prefix)
			result.Entries[index].Path = strings.TrimSuffix(directory, "/") + "/" + strings.TrimPrefix(result.Entries[index].Path, "/")
		}
		return result, nil
	}
	selected, err := backend.selectedPath(ctx, "/"+agentInstructionsFilename)
	if err != nil {
		return dabackend.ListResult{}, err
	}
	result, err := backend.files.List(ctx, filepath.ToSlash(filepath.Dir(selected)))
	if err != nil {
		return dabackend.ListResult{}, err
	}
	for _, entry := range result.Entries {
		if entry.Path == selected {
			entry.Path = "/" + agentInstructionsFilename
			return dabackend.ListResult{Entries: []dabackend.FileInfo{entry}}, nil
		}
	}
	return dabackend.ListResult{}, nil
}

func (backend *agentMemoryBackend) Read(ctx context.Context, name string, offset, limit int) (dabackend.ReadResult, error) {
	selected, err := backend.selectedPath(ctx, name)
	if err != nil {
		return dabackend.ReadResult{}, err
	}
	return backend.files.Read(ctx, selected, offset, limit)
}

func (backend *agentMemoryBackend) Write(ctx context.Context, name, content string) (dabackend.WriteResult, error) {
	if name != "/"+agentInstructionsFilename {
		return dabackend.WriteResult{}, fmt.Errorf("agent skills are read-only")
	}
	selected, err := backend.selectedPath(ctx, name)
	if err != nil {
		return dabackend.WriteResult{}, err
	}
	result, err := backend.files.Write(ctx, selected, content)
	result.Path = "/" + agentInstructionsFilename
	return result, err
}

func (backend *agentMemoryBackend) WriteDurable(ctx context.Context, name, content string) (dabackend.WriteResult, error) {
	if name != "/"+agentInstructionsFilename {
		return dabackend.WriteResult{}, fmt.Errorf("agent skills are read-only")
	}
	selected, err := backend.selectedPath(ctx, name)
	if err != nil {
		return dabackend.WriteResult{}, err
	}
	result, err := backend.files.WriteDurable(ctx, selected, content)
	result.Path = "/" + agentInstructionsFilename
	return result, err
}

func (backend *agentMemoryBackend) IsSymlink(ctx context.Context, name string) (bool, error) {
	selected, err := backend.selectedPath(ctx, name)
	if err != nil {
		return false, err
	}
	return backend.files.IsSymlink(ctx, selected)
}

func (backend *agentMemoryBackend) Edit(ctx context.Context, name, old, replacement string, all bool) (dabackend.EditResult, error) {
	if name != "/"+agentInstructionsFilename {
		return dabackend.EditResult{}, fmt.Errorf("agent skills are read-only")
	}
	selected, err := backend.selectedPath(ctx, name)
	if err != nil {
		return dabackend.EditResult{}, err
	}
	result, err := backend.files.Edit(ctx, selected, old, replacement, all)
	result.Path = "/" + agentInstructionsFilename
	return result, err
}

func (backend *agentMemoryBackend) Delete(ctx context.Context, name string) (dabackend.DeleteResult, error) {
	if name != "/"+agentInstructionsFilename {
		return dabackend.DeleteResult{}, fmt.Errorf("agent skills are read-only")
	}
	selected, err := backend.selectedPath(ctx, name)
	if err != nil {
		return dabackend.DeleteResult{}, err
	}
	result, err := backend.files.Delete(ctx, selected)
	result.Path = "/" + agentInstructionsFilename
	return result, err
}

func (backend *agentMemoryBackend) Glob(ctx context.Context, pattern, base string) (dabackend.GlobResult, error) {
	if base != "" && base != "/" {
		return dabackend.GlobResult{}, fmt.Errorf("agent memory glob base must be /")
	}
	selected, err := backend.selectedPath(ctx, "/"+agentInstructionsFilename)
	if err != nil {
		return dabackend.GlobResult{}, err
	}
	result, err := backend.files.Glob(ctx, pattern, filepath.ToSlash(filepath.Dir(selected)))
	if err != nil {
		return dabackend.GlobResult{}, err
	}
	filtered := result.Matches[:0]
	for _, match := range result.Matches {
		if strings.TrimSuffix(match.Path, "/") == selected {
			match.Path = "/" + agentInstructionsFilename
			filtered = append(filtered, match)
		}
	}
	result.Matches = filtered
	return result, nil
}

func (backend *agentMemoryBackend) Grep(ctx context.Context, pattern string, options dabackend.GrepOptions) (dabackend.GrepResult, error) {
	selected, err := backend.selectedPath(ctx, "/"+agentInstructionsFilename)
	if err != nil {
		return dabackend.GrepResult{}, err
	}
	switch options.Path {
	case "", "/":
		options.Path = filepath.ToSlash(filepath.Dir(selected))
	case "/" + agentInstructionsFilename:
		options.Path = selected
	default:
		return dabackend.GrepResult{}, fmt.Errorf("agent memory exposes only /%s", agentInstructionsFilename)
	}
	result, err := backend.files.Grep(ctx, pattern, options)
	for index := range result.Matches {
		result.Matches[index].Path = "/" + agentInstructionsFilename
	}
	return result, err
}

func (backend *agentMemoryBackend) Upload(ctx context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	results := make([]dabackend.UploadResult, len(uploads))
	for index, upload := range uploads {
		results[index].Path = upload.Path
		if upload.Path != "/"+agentInstructionsFilename {
			results[index].Error = "agent skills are read-only"
			continue
		}
		selected, err := backend.selectedPath(ctx, upload.Path)
		if err != nil {
			results[index].Error = err.Error()
			continue
		}
		part := backend.files.Upload(ctx, []dabackend.Upload{{Path: selected, Content: upload.Content}})
		if len(part) != 1 {
			results[index].Error = fmt.Sprintf("backend returned %d upload results for one request", len(part))
			continue
		}
		results[index].Error = part[0].Error
	}
	return results
}

func (backend *agentMemoryBackend) Download(ctx context.Context, names []string) []dabackend.DownloadResult {
	results := make([]dabackend.DownloadResult, len(names))
	for index, name := range names {
		results[index].Path = name
		selected, err := backend.selectedPath(ctx, name)
		if err != nil {
			results[index].Error = err.Error()
			continue
		}
		part := backend.files.Download(ctx, []string{selected})
		if len(part) != 1 {
			results[index].Error = fmt.Sprintf("backend returned %d download results for one request", len(part))
			continue
		}
		results[index].Content = part[0].Content
		results[index].Error = part[0].Error
	}
	return results
}

func (backend *agentMemoryBackend) BindRuntime(ctx context.Context, reader dabackend.StateReader) (context.Context, error) {
	name := ""
	if reader != nil {
		value, exists := reader.Get(sessionAgentNameKey)
		if exists {
			name, _ = value.(string)
			if name == "" {
				return nil, fmt.Errorf("session agent name has type %T", value)
			}
		}
	}
	if name == "" {
		name = backend.identity.current()
	}
	if err := validateAgentName(name); err != nil {
		return nil, fmt.Errorf("bind agent memory: %w", err)
	}
	return context.WithValue(ctx, agentMemoryContextKey{backend: backend}, name), nil
}

func (backend *agentMemoryBackend) RuntimeUpdates(context.Context) map[string]any { return nil }

func (backend *agentMemoryBackend) StateFields() []dabackend.StateField { return nil }

var _ dabackend.RuntimeBackend = (*agentMemoryBackend)(nil)
var _ dabackend.DurableWriter = (*agentMemoryBackend)(nil)

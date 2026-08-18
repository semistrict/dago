// Package dasubagent discovers declarative subagent definitions from bounded,
// confined AGENTS.md files.
package dasubagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/semistrict/dago/daskill"
	"gopkg.in/yaml.v3"
)

// Source identifies the precedence layer that supplied a definition.
type Source string

const (
	UserSource    Source = "user"
	ProjectSource Source = "project"
)

// Definition is one validated declarative subagent. Project definitions
// replace same-name user definitions.
type Definition struct {
	Name         string
	Description  string
	SystemPrompt string
	Model        string
	Source       Source
	Path         string
}

// Diagnostic identifies a skipped definition without including file content.
type Diagnostic struct {
	Path   string
	Reason string
}

// Report is a deterministic discovery result.
type Report struct {
	Definitions []Definition
	Diagnostics []Diagnostic
}

// Options contains optional finite discovery limits. Its zero value is useful.
type Options struct {
	MaxDefinitions      int
	MaxFileBytes        int64
	MaxNameBytes        int
	MaxDescriptionBytes int
	MaxPromptBytes      int
	MaxModelBytes       int
}

func (options Options) withDefaults() Options {
	if options.MaxDefinitions == 0 {
		options.MaxDefinitions = 128
	}
	if options.MaxFileBytes == 0 {
		options.MaxFileBytes = 256 << 10
	}
	if options.MaxNameBytes == 0 {
		options.MaxNameBytes = 128
	}
	if options.MaxDescriptionBytes == 0 {
		options.MaxDescriptionBytes = 4096
	}
	if options.MaxPromptBytes == 0 {
		options.MaxPromptBytes = 256 << 10
	}
	if options.MaxModelBytes == 0 {
		options.MaxModelBytes = 1024
	}
	if options.MaxDefinitions < 1 || options.MaxDefinitions > 4096 ||
		options.MaxFileBytes < 1 || options.MaxFileBytes > 4<<20 ||
		options.MaxNameBytes < 1 || options.MaxNameBytes > 4096 ||
		options.MaxDescriptionBytes < 1 || options.MaxDescriptionBytes > 64<<10 ||
		options.MaxPromptBytes < 1 || options.MaxPromptBytes > 4<<20 ||
		options.MaxModelBytes < 1 || options.MaxModelBytes > 16<<10 {
		panic("dasubagent: invalid discovery limits")
	}
	return options
}

// Discover reads optional user and project agent directories. Empty directory
// strings omit that layer; project definitions take precedence over user ones.
func Discover(ctx context.Context, userDirectory, projectDirectory string, options Options) (Report, error) {
	if ctx == nil {
		panic("dasubagent: context is required")
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	options = options.withDefaults()
	byName := map[string]Definition{}
	report := Report{}
	for _, layer := range []struct {
		path   string
		source Source
	}{{userDirectory, UserSource}, {projectDirectory, ProjectSource}} {
		if layer.path == "" {
			continue
		}
		if !filepath.IsAbs(layer.path) || strings.ContainsRune(layer.path, 0) || len(layer.path) > 4096 {
			panic("dasubagent: directories must be bounded absolute paths")
		}
		definitions, diagnostics, err := discoverLayer(ctx, layer.path, layer.source, options)
		if err != nil {
			return Report{}, err
		}
		report.Diagnostics = append(report.Diagnostics, diagnostics...)
		for _, definition := range definitions {
			byName[definition.Name] = definition
			if len(byName) > options.MaxDefinitions {
				return Report{}, errors.New("resolved subagents exceed definition limit")
			}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	report.Definitions = make([]Definition, 0, len(names))
	for _, name := range names {
		report.Definitions = append(report.Definitions, byName[name])
	}
	sort.Slice(report.Diagnostics, func(i, j int) bool {
		if report.Diagnostics[i].Path == report.Diagnostics[j].Path {
			return report.Diagnostics[i].Reason < report.Diagnostics[j].Reason
		}
		return report.Diagnostics[i].Path < report.Diagnostics[j].Path
	})
	return report, nil
}

func discoverLayer(ctx context.Context, directory string, source Source, options Options) ([]Definition, []Diagnostic, error) {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("inspect %s subagent directory: %w", source, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, nil, fmt.Errorf("%s subagent path must be a non-symlink directory", source)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s subagent directory: %w", source, err)
	}
	defer root.Close()
	directoryFile, err := root.Open(".")
	if err != nil {
		return nil, nil, fmt.Errorf("list %s subagent directory: %w", source, err)
	}
	entries, err := directoryFile.ReadDir(options.MaxDefinitions + 1)
	closeErr := directoryFile.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("list %s subagent directory: %w", source, err)
	}
	if closeErr != nil {
		return nil, nil, fmt.Errorf("close %s subagent directory: %w", source, closeErr)
	}
	if len(entries) > options.MaxDefinitions {
		return nil, nil, errors.New("subagent directory exceeds definition limit")
	}
	byName := map[string]Definition{}
	var diagnostics []Diagnostic
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		entryPath := filepath.Join(directory, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			if strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				diagnostics = append(diagnostics, Diagnostic{Path: entryPath, Reason: "definitions must use NAME/AGENTS.md"})
			}
			continue
		}
		relative := filepath.Join(entry.Name(), "AGENTS.md")
		definitionPath := filepath.Join(directory, relative)
		definition, err := readDefinition(root, relative, definitionPath, entry.Name(), source, options)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{Path: definitionPath, Reason: err.Error()})
			continue
		}
		if previous, exists := byName[definition.Name]; exists {
			diagnostics = append(diagnostics, Diagnostic{
				Path: definition.Path, Reason: "duplicate name also declared by " + previous.Path,
			})
		}
		byName[definition.Name] = definition
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	definitions := make([]Definition, 0, len(names))
	for _, name := range names {
		definitions = append(definitions, byName[name])
	}
	return definitions, diagnostics, nil
}

func readDefinition(root *os.Root, relative, absolute, fallback string, source Source, options Options) (Definition, error) {
	before, err := root.Lstat(relative)
	if err != nil {
		return Definition{}, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() > options.MaxFileBytes {
		return Definition{}, errors.New("definition must be a bounded regular non-symlink file")
	}
	file, err := openDefinition(root, relative)
	if err != nil {
		return Definition{}, fmt.Errorf("open definition: %w", err)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return Definition{}, errors.New("definition changed during open")
	}
	limited := &io.LimitedReader{R: file, N: options.MaxFileBytes + 1}
	content, err := io.ReadAll(limited)
	if err != nil {
		return Definition{}, fmt.Errorf("read definition: %w", err)
	}
	if limited.N == 0 || !utf8.Valid(content) {
		return Definition{}, errors.New("definition is oversized or not UTF-8")
	}
	return parseDefinition(content, absolute, fallback, source, options)
}

func parseDefinition(content []byte, filePath, fallback string, source Source, options Options) (Definition, error) {
	content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	lines := bytes.Split(content, []byte("\n"))
	if len(lines) < 3 || string(bytes.TrimSpace(lines[0])) != "---" {
		return Definition{}, errors.New("missing YAML frontmatter")
	}
	end := -1
	for index := 1; index < len(lines); index++ {
		if string(bytes.TrimSpace(lines[index])) == "---" {
			end = index
			break
		}
	}
	if end < 0 {
		return Definition{}, errors.New("unterminated YAML frontmatter")
	}
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(bytes.Join(lines[1:end], []byte("\n"))))
	if err := decoder.Decode(&document); err != nil {
		return Definition{}, errors.New("invalid YAML frontmatter")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return Definition{}, errors.New("frontmatter must be a mapping")
	}
	values := map[string]string{}
	seenFields := map[string]struct{}{}
	mapping := document.Content[0]
	for index := 0; index < len(mapping.Content); index += 2 {
		key, value := mapping.Content[index], mapping.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Alias != nil {
			return Definition{}, errors.New("frontmatter keys must be plain strings")
		}
		if _, duplicate := seenFields[key.Value]; duplicate {
			return Definition{}, errors.New("frontmatter contains a duplicate field")
		}
		seenFields[key.Value] = struct{}{}
		if key.Value != "name" && key.Value != "description" && key.Value != "model" {
			continue
		}
		if key.Value == "model" && value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
			continue
		}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" || value.Alias != nil {
			return Definition{}, errors.New("name, description, and model must be plain strings")
		}
		values[key.Value] = value.Value
	}
	name := fallback
	if declared, exists := values["name"]; exists {
		name = strings.TrimSpace(declared)
		if name == "" {
			return Definition{}, errors.New("name must be a non-empty string")
		}
	}
	description, exists := values["description"]
	description = strings.TrimSpace(description)
	if !exists || description == "" {
		return Definition{}, errors.New("description must be a non-empty string")
	}
	model := ""
	if configured, exists := values["model"]; exists {
		model = strings.TrimSpace(configured)
	}
	prompt := strings.TrimSpace(string(bytes.Join(lines[end+1:], []byte("\n"))))
	if err := daskill.ValidateName(name); err != nil || len(name) > options.MaxNameBytes {
		return Definition{}, errors.New("name is invalid or oversized")
	}
	if len(description) > options.MaxDescriptionBytes || len(prompt) > options.MaxPromptBytes || len(model) > options.MaxModelBytes ||
		strings.ContainsRune(description, 0) || strings.ContainsRune(prompt, 0) || strings.ContainsRune(model, 0) {
		return Definition{}, errors.New("definition field exceeds its bound")
	}
	return Definition{
		Name: name, Description: description, SystemPrompt: prompt, Model: model,
		Source: source, Path: filePath,
	}, nil
}

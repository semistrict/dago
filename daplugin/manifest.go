package daplugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var componentNamePattern = regexp.MustCompile(`^[^\s]{1,128}$`)

func validateName(value string, allowAt bool) error {
	if !componentNamePattern.MatchString(value) || (!allowAt && strings.Contains(value, "@")) || strings.ContainsAny(value, "\x00/\\") {
		return fmt.Errorf("invalid name %q", value)
	}
	return nil
}

func boundedJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("manifest must be a regular file")
	}
	if info.Size() > MaxManifestBytes {
		return errors.New("manifest exceeds size limit")
	}
	decoder := json.NewDecoder(io.LimitReader(file, MaxManifestBytes+1))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("manifest must contain one JSON value")
	}
	return nil
}

func canonicalRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("plugin root must be a real directory")
	}
	return filepath.EvalSymlinks(abs)
}

func containedPath(root, declaration string) (string, error) {
	if !strings.HasPrefix(declaration, "./") || len(declaration) > 4096 || strings.ContainsRune(declaration, 0) {
		return "", errors.New("component path must start with ./")
	}
	relative := strings.TrimPrefix(declaration, "./")
	if relative == "" || filepath.IsAbs(relative) {
		return "", errors.New("component path is invalid")
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("component path escapes plugin root")
	}
	joined := filepath.Join(root, clean)
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("component path escapes plugin root")
	}
	return resolved, nil
}

// LoadManifest reads the first supported plugin manifest and inventories its
// confined components. Missing manifests are supported using conventional paths.
func LoadManifest(root, fallbackName string) (*Manifest, Inventory, error) {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return nil, Inventory{}, err
	}
	manifestPaths := []string{"plugin.json", filepath.Join(".claude-plugin", "plugin.json"), filepath.Join(".co"+"dex-plugin", "plugin.json")}
	var manifestPath string
	for _, relative := range manifestPaths {
		candidate := filepath.Join(canonical, relative)
		if info, statErr := os.Lstat(candidate); statErr == nil && info.Mode().IsRegular() {
			manifestPath = candidate
			break
		}
	}
	manifest := &Manifest{Name: fallbackName, ComponentPaths: map[string][]string{}, InlineMCP: map[string]json.RawMessage{}}
	warnings := []string{}
	if manifestPath != "" {
		var raw map[string]json.RawMessage
		if err := boundedJSON(manifestPath, &raw); err != nil {
			return nil, Inventory{}, fmt.Errorf("read plugin manifest: %w", err)
		}
		_ = json.Unmarshal(raw["name"], &manifest.Name)
		_ = json.Unmarshal(raw["displayName"], &manifest.DisplayName)
		_ = json.Unmarshal(raw["version"], &manifest.Version)
		var extensions map[string]map[string]any
		_ = json.Unmarshal(raw["extensions"], &extensions)
		if settings := extensions["com.langchain.deepagents.code"]; settings != nil {
			if value, ok := settings["autoUpdate"].(bool); ok && value {
				manifest.AutoUpdate = true
			}
		}
		for _, field := range []string{"skills", "mcpServers", "hooks"} {
			value := raw[field]
			if len(value) == 0 {
				continue
			}
			if field == "mcpServers" {
				var inline map[string]json.RawMessage
				if json.Unmarshal(value, &inline) == nil {
					manifest.InlineMCP = inline
					continue
				}
				var list []map[string]json.RawMessage
				if json.Unmarshal(value, &list) == nil {
					for _, item := range list {
						for key, server := range item {
							manifest.InlineMCP[key] = server
						}
					}
					continue
				}
			}
			if field == "hooks" && len(value) > 0 && value[0] == '{' {
				var object map[string]json.RawMessage
				if json.Unmarshal(value, &object) == nil {
					if _, wrapped := object["hooks"]; wrapped {
						manifest.InlineHooks = append([]byte(nil), value...)
					} else {
						manifest.InlineHooks, _ = json.Marshal(map[string]any{"hooks": object})
					}
					continue
				}
			}
			var single string
			var paths []string
			if json.Unmarshal(value, &single) == nil {
				paths = []string{single}
			} else {
				var mixed []any
				if json.Unmarshal(value, &mixed) != nil {
					warnings = append(warnings, "ignoring malformed "+field)
					continue
				}
				for _, item := range mixed {
					if text, ok := item.(string); ok {
						paths = append(paths, text)
					} else {
						warnings = append(warnings, "ignoring non-string "+field+" path")
					}
				}
			}
			for _, declaration := range paths {
				resolved, resolveErr := containedPath(canonical, declaration)
				if resolveErr != nil {
					warnings = append(warnings, "ignoring "+field+": "+resolveErr.Error())
					continue
				}
				manifest.ComponentPaths[field] = append(manifest.ComponentPaths[field], resolved)
			}
		}
	}
	if err := validateName(manifest.Name, true); err != nil {
		return nil, Inventory{}, err
	}
	inventory := Inventory{Warnings: warnings}
	inventory.Skills = append(inventory.Skills, existingPaths(canonical, "skills")...)
	inventory.Skills = append(inventory.Skills, manifest.ComponentPaths["skills"]...)
	if len(inventory.Skills) == 0 {
		inventory.Skills = append(inventory.Skills, existingPaths(canonical, "SKILL.md")...)
	}
	inventory.MCPFiles = append(inventory.MCPFiles, existingPaths(canonical, ".mcp.json")...)
	inventory.MCPFiles = append(inventory.MCPFiles, manifest.ComponentPaths["mcpServers"]...)
	inventory.HookFiles = append(inventory.HookFiles, existingPaths(canonical, "hooks/hooks.json")...)
	for _, declared := range manifest.ComponentPaths["hooks"] {
		if info, statErr := os.Stat(declared); statErr == nil && info.IsDir() {
			inventory.HookFiles = append(inventory.HookFiles, existingPaths(declared, "hooks.json")...)
		} else {
			inventory.HookFiles = append(inventory.HookFiles, declared)
		}
	}
	inventory.Skills = uniquePaths(inventory.Skills)
	inventory.MCPFiles = uniquePaths(inventory.MCPFiles)
	inventory.HookFiles = uniquePaths(inventory.HookFiles)
	for _, unsupported := range []string{"agents", "commands"} {
		if info, statErr := os.Stat(filepath.Join(canonical, unsupported)); statErr == nil && info.IsDir() {
			inventory.Unsupported = append(inventory.Unsupported, unsupported)
		}
	}
	if len(inventory.Skills)+len(inventory.MCPFiles)+len(inventory.HookFiles) > MaxComponents {
		return nil, Inventory{}, errors.New("plugin exceeds component limit")
	}
	if manifestPath == "" {
		manifest = nil
	}
	return manifest, inventory, nil
}

func uniquePaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		if !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	return result
}

func existingPaths(root string, relatives ...string) []string {
	var result []string
	for _, relative := range relatives {
		candidate := filepath.Join(root, filepath.FromSlash(relative))
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if _, err := os.Stat(resolved); err == nil {
			result = append(result, resolved)
		}
	}
	return result
}

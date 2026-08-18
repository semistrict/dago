package daplugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ParseMarketplaceSource classifies a local, GitHub, git, or HTTPS catalog source.
func ParseMarketplaceSource(raw string) (MarketplaceSource, error) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > 4096 || strings.ContainsRune(value, 0) {
		return MarketplaceSource{}, errors.New("marketplace source is invalid")
	}
	if info, err := os.Stat(value); err == nil {
		absolute, _ := filepath.Abs(value)
		if info.IsDir() {
			return MarketplaceSource{Type: SourceDirectory, Value: absolute}, nil
		}
		if info.Mode().IsRegular() && strings.EqualFold(filepath.Ext(value), ".json") {
			return MarketplaceSource{Type: SourceFile, Value: absolute}, nil
		}
	}
	if strings.HasPrefix(value, "http://") {
		return MarketplaceSource{}, errors.New("remote marketplace sources must use https")
	}
	if strings.HasPrefix(value, "https://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Hostname() == "" || parsed.User != nil {
			return MarketplaceSource{}, errors.New("marketplace URL is invalid")
		}
		ref := parsed.Fragment
		parsed.Fragment = ""
		if strings.HasSuffix(parsed.Path, ".git") || strings.Contains(parsed.Path, "/_git/") {
			return MarketplaceSource{Type: SourceGit, Value: parsed.String(), Ref: ref}, nil
		}
		return MarketplaceSource{Type: SourceURL, Value: parsed.String()}, nil
	}
	base, ref, hasRef := strings.Cut(value, "#")
	parts := strings.Split(base, "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.ContainsAny(value, " :\\") {
		if !hasRef {
			ref = ""
		}
		return MarketplaceSource{Type: SourceGitHub, Value: base, Ref: ref}, nil
	}
	if strings.Contains(value, ":") && !strings.ContainsAny(value, "\r\n") {
		return MarketplaceSource{Type: SourceGit, Value: base, Ref: ref}, nil
	}
	return MarketplaceSource{}, errors.New("unsupported marketplace source")
}

// LoadMarketplace parses a bounded local catalog directory or JSON file.
func LoadMarketplace(location string) (Marketplace, error) {
	absolute, err := filepath.Abs(location)
	if err != nil {
		return Marketplace{}, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return Marketplace{}, err
	}
	root, manifestPath := filepath.Dir(absolute), absolute
	if info.IsDir() {
		if info.Mode()&os.ModeSymlink != 0 {
			return Marketplace{}, errors.New("marketplace root must not be a symlink")
		}
		root, err = canonicalRoot(absolute)
		if err != nil {
			return Marketplace{}, err
		}
		manifestPath = ""
		for _, relative := range []string{filepath.Join(".claude-plugin", "marketplace.json"), filepath.Join(".agents", "plugins", "marketplace.json"), filepath.Join(".agents", "plugins", "api_marketplace.json")} {
			candidate := filepath.Join(root, relative)
			if item, statErr := os.Lstat(candidate); statErr == nil && item.Mode().IsRegular() {
				manifestPath = candidate
				break
			}
		}
		if manifestPath == "" {
			return Marketplace{}, errors.New("marketplace manifest not found")
		}
	} else if !info.Mode().IsRegular() {
		return Marketplace{}, errors.New("marketplace manifest must be regular")
	} else {
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return Marketplace{}, err
		}
		manifestPath, err = filepath.EvalSymlinks(manifestPath)
		if err != nil {
			return Marketplace{}, err
		}
	}
	var raw struct {
		Name     string            `json:"name"`
		Metadata map[string]any    `json:"metadata"`
		Plugins  []json.RawMessage `json:"plugins"`
	}
	if err := boundedJSON(manifestPath, &raw); err != nil {
		return Marketplace{}, fmt.Errorf("read marketplace: %w", err)
	}
	if err := validateName(raw.Name, false); err != nil {
		return Marketplace{}, err
	}
	if raw.Plugins == nil {
		return Marketplace{}, errors.New("marketplace must contain a plugins array")
	}
	if len(raw.Plugins) > MaxPlugins {
		return Marketplace{}, errors.New("marketplace exceeds plugin limit")
	}
	result := Marketplace{Name: raw.Name, Root: root, ManifestPath: manifestPath, Metadata: raw.Metadata}
	seen := map[string]bool{}
	for _, encoded := range raw.Plugins {
		entry, parseErr := parseMarketplaceEntry(encoded)
		if parseErr != nil {
			result.Warnings = append(result.Warnings, parseErr.Error())
			continue
		}
		if seen[entry.Name] {
			result.Warnings = append(result.Warnings, "skipping duplicate plugin "+entry.Name)
			continue
		}
		seen[entry.Name] = true
		result.Plugins = append(result.Plugins, entry)
	}
	return result, nil
}

func parseMarketplaceEntry(encoded json.RawMessage) (MarketplaceEntry, error) {
	var raw struct {
		Name, Description, DisplayName string
		Author                         json.RawMessage
		Source                         json.RawMessage
	}
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return MarketplaceEntry{}, errors.New("skipping malformed plugin entry")
	}
	if err := validateName(raw.Name, true); err != nil {
		return MarketplaceEntry{}, err
	}
	source := PluginSource{}
	var local string
	if json.Unmarshal(raw.Source, &local) == nil {
		source = PluginSource{Type: SourceDirectory, Path: local}
	} else {
		var object struct {
			Source               SourceType `json:"source"`
			Path, Repo, URL, Ref string
		}
		if json.Unmarshal(raw.Source, &object) != nil {
			return MarketplaceEntry{}, fmt.Errorf("skipping plugin %q: invalid source", raw.Name)
		}
		source = PluginSource{Type: object.Source, Path: object.Path, Repo: object.Repo, URL: object.URL, Ref: object.Ref}
		if source.Type == SourceLocal {
			source.Type = SourceLocal
		}
	}
	if (source.Type == SourceLocal || source.Type == SourceDirectory) && source.Path == "" || source.Type == SourceGitHub && source.Repo == "" || (source.Type == SourceGitSubdir || source.Type == SourceURL) && source.URL == "" {
		return MarketplaceEntry{}, fmt.Errorf("skipping plugin %q: incomplete source", raw.Name)
	}
	if source.Type != SourceLocal && source.Type != SourceDirectory && source.Type != SourceGitHub && source.Type != SourceGitSubdir && source.Type != SourceURL {
		return MarketplaceEntry{}, fmt.Errorf("skipping plugin %q: unsupported source", raw.Name)
	}
	return MarketplaceEntry{Name: raw.Name, Description: raw.Description, DisplayName: raw.DisplayName, Author: raw.Author, Source: source}, nil
}

// Package daplugin provides bounded local plugin and marketplace management.
package daplugin

import "encoding/json"

const (
	MaxManifestBytes = 4 << 20
	MaxPlugins       = 512
	MaxComponents    = 256
)

type SourceType string

const (
	SourceDirectory SourceType = "directory"
	SourceFile      SourceType = "file"
	SourceGitHub    SourceType = "github"
	SourceGit       SourceType = "git"
	SourceURL       SourceType = "url"
	SourceLocal     SourceType = "local"
	SourceGitSubdir SourceType = "git-subdir"
)

type MarketplaceSource struct {
	Type  SourceType `json:"source_type"`
	Value string     `json:"source"`
	Ref   string     `json:"ref,omitempty"`
}

type PluginSource struct {
	Type SourceType `json:"source_type"`
	Path string     `json:"path,omitempty"`
	Repo string     `json:"repo,omitempty"`
	URL  string     `json:"url,omitempty"`
	Ref  string     `json:"ref,omitempty"`
}

type MarketplaceEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	DisplayName string          `json:"display_name,omitempty"`
	Author      json.RawMessage `json:"author,omitempty"`
	Source      PluginSource    `json:"source"`
}

type Marketplace struct {
	Name         string             `json:"name"`
	Root         string             `json:"root"`
	ManifestPath string             `json:"manifest_path"`
	Metadata     map[string]any     `json:"metadata,omitempty"`
	Plugins      []MarketplaceEntry `json:"plugins"`
	Warnings     []string           `json:"warnings,omitempty"`
}

type Manifest struct {
	Name           string                     `json:"name"`
	DisplayName    string                     `json:"display_name,omitempty"`
	Version        string                     `json:"version,omitempty"`
	ComponentPaths map[string][]string        `json:"component_paths,omitempty"`
	InlineMCP      map[string]json.RawMessage `json:"inline_mcp,omitempty"`
	InlineHooks    json.RawMessage            `json:"inline_hooks,omitempty"`
	AutoUpdate     bool                       `json:"auto_update,omitempty"`
}

type Inventory struct {
	Skills      []string `json:"skills,omitempty"`
	MCPFiles    []string `json:"mcp_files,omitempty"`
	HookFiles   []string `json:"hook_files,omitempty"`
	Unsupported []string `json:"unsupported,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

type Plugin struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Marketplace string    `json:"marketplace"`
	Version     string    `json:"version,omitempty"`
	Root        string    `json:"root"`
	DataDir     string    `json:"data_dir"`
	Enabled     bool      `json:"enabled"`
	Manifest    *Manifest `json:"manifest,omitempty"`
	Inventory   Inventory `json:"inventory"`
}

type MaterializedPlugin struct {
	Root        string
	CleanupRoot string
}

type Components struct {
	Skills []SkillSource `json:"skills,omitempty"`
	Hooks  []HookSource  `json:"hooks,omitempty"`
	MCP    []MCPSource   `json:"mcp,omitempty"`
}

type SkillSource struct{ PluginID, Root, Path, Namespace string }
type HookSource struct {
	PluginID, Path string
	Inline         json.RawMessage
	Environment    map[string]string
}
type MCPSource struct {
	PluginID, Path string
	Inline         map[string]json.RawMessage
	Environment    map[string]string
}

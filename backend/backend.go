// Package backend defines virtual file and optional shell capabilities used by deep
// agent filesystem middleware.
package backend

import (
	"context"
	"path"
	"time"
)

type Encoding string

const (
	EncodingUTF8   Encoding = "utf-8"
	EncodingBase64 Encoding = "base64"
)

type FileData struct {
	Content    string    `json:"content"`
	Encoding   Encoding  `json:"encoding"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
	ModifiedAt time.Time `json:"modified_at,omitempty"`
}

type FileInfo struct {
	Path       string    `json:"path"`
	IsDir      bool      `json:"is_dir,omitempty"`
	Size       int64     `json:"size,omitempty"`
	ModifiedAt time.Time `json:"modified_at,omitempty"`
}

type ContextLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

type GrepMatch struct {
	Path          string        `json:"path"`
	Line          int           `json:"line"`
	Text          string        `json:"text"`
	ContextBefore []ContextLine `json:"context_before,omitempty"`
	ContextAfter  []ContextLine `json:"context_after,omitempty"`
}

type ReadResult struct {
	Data             *FileData `json:"file_data,omitempty"`
	TotalLines       *int      `json:"total_lines,omitempty"`
	StartLine        *int      `json:"start_line,omitempty"`
	EndLine          *int      `json:"end_line,omitempty"`
	NextOffset       *int      `json:"next_offset,omitempty"`
	NoLinesRequested bool      `json:"no_lines_requested,omitempty"`
}

type ListResult struct {
	Entries []FileInfo `json:"entries"`
}
type GlobResult struct {
	Matches   []FileInfo `json:"matches"`
	Truncated bool       `json:"truncated,omitempty"`
}
type GrepResult struct {
	Matches   []GrepMatch `json:"matches"`
	Truncated bool        `json:"truncated,omitempty"`
}
type WriteResult struct {
	Path string `json:"path"`
}
type EditResult struct {
	Path        string `json:"path"`
	Occurrences int    `json:"occurrences"`
}
type DeleteResult struct {
	Path string `json:"path"`
}

type Upload struct {
	Path    string
	Content []byte
}
type UploadResult struct {
	Path  string `json:"path"`
	Error string `json:"error,omitempty"`
}
type DownloadResult struct {
	Path    string `json:"path"`
	Content []byte `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

type GrepOptions struct {
	Path         string
	Glob         string
	MaxCount     int
	ContextLines int
}

// Backend is the uniform synchronous-by-contract, context-aware file API.
type Backend interface {
	List(context.Context, string) (ListResult, error)
	Read(context.Context, string, int, int) (ReadResult, error)
	Write(context.Context, string, string) (WriteResult, error)
	Edit(context.Context, string, string, string, bool) (EditResult, error)
	Delete(context.Context, string) (DeleteResult, error)
	Glob(context.Context, string, string) (GlobResult, error)
	Grep(context.Context, string, GrepOptions) (GrepResult, error)
	Upload(context.Context, []Upload) []UploadResult
	Download(context.Context, []string) []DownloadResult
}

type ExecuteResult struct {
	Output    string `json:"output"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Sandbox is an explicitly granted extension; a Backend can never gain shell
// authority implicitly.
type Sandbox interface {
	Backend
	ID() string
	Execute(context.Context, string, time.Duration) (ExecuteResult, error)
}

type sandboxResolver interface {
	resolveSandbox() (Sandbox, bool)
}

// SandboxOf resolves command execution through direct and composite backends.
// Composite execution always belongs to its default backend; routed mounts are
// file-only views and do not change the shell environment.
func SandboxOf(value Backend) (Sandbox, bool) {
	if sandbox, ok := value.(Sandbox); ok {
		return sandbox, true
	}
	if resolver, ok := value.(sandboxResolver); ok {
		return resolver.resolveSandbox()
	}
	return nil, false
}

// ShellPathRoute describes whether a composite virtual mount is reachable by
// the default backend's shell and, when it is, which host prefix replaces it.
type ShellPathRoute struct {
	VirtualPrefix string
	HostPrefix    string
	Accessible    bool
}

type localHostSandbox interface {
	Sandbox
	localHostRoot() string
}

type localHostFilesystem interface {
	Backend
	localHostRoot() string
}

type artifactsRootBackend interface {
	backendArtifactsRoot() string
}

// ArtifactsRootOf returns the backend-selected virtual root for generated
// conversation history, media, and large tool outputs.
func ArtifactsRootOf(value Backend) string {
	if rooted, ok := value.(artifactsRootBackend); ok {
		if root := rooted.backendArtifactsRoot(); root != "" {
			return root
		}
	}
	return "/"
}

// ArtifactPath joins a generated-artifact directory to a backend's root.
func ArtifactPath(value Backend, directory string) string {
	return path.Join(ArtifactsRootOf(value), directory)
}

// ShellPathRoutes reports composite mount mappings without exposing backend
// internals. Local mappings are valid only when the default shell and routed
// filesystem share the host.
func ShellPathRoutes(value Backend) []ShellPathRoute {
	composite, ok := value.(*Composite)
	if !ok || len(composite.routes) == 0 {
		return nil
	}
	_, localDefault := composite.defaultBackend.(localHostSandbox)
	result := make([]ShellPathRoute, 0, len(composite.routes))
	for _, route := range composite.routes {
		item := ShellPathRoute{VirtualPrefix: route.prefix}
		if filesystem, ok := route.backend.(localHostFilesystem); ok && localDefault {
			item.HostPrefix = filesystem.localHostRoot()
			item.Accessible = true
		}
		result = append(result, item)
	}
	return result
}

type Capabilities struct {
	Delete  bool
	Execute bool
}

func CapabilitiesOf(value Backend) Capabilities {
	_, execute := SandboxOf(value)
	return Capabilities{Delete: value != nil, Execute: execute}
}

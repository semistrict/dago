// Package backend defines virtual file and optional shell capabilities used by deep
// agent filesystem middleware.
package backend

import (
	"context"
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

type Capabilities struct {
	Delete  bool
	Execute bool
}

func CapabilitiesOf(value Backend) Capabilities {
	_, execute := value.(Sandbox)
	return Capabilities{Delete: value != nil, Execute: execute}
}

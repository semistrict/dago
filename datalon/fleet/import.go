// Package fleet imports Fleet zip exports into a local datalon assistant state
// directory.
package fleet

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	rootPromptName = "AGENTS.md"
	mcpConfigName  = ".mcp.json"
	mcpSetupName   = ".mcp.json.setup"
	copyBufferSize = 1 << 20
)

var (
	// ErrInvalidArchive reports a malformed or incomplete Fleet export.
	ErrInvalidArchive = errors.New("invalid Fleet archive")
	// ErrUnsafeArchive reports an archive entry that could escape or change the
	// type of the materialized assistant state.
	ErrUnsafeArchive = errors.New("unsafe Fleet archive")
	// ErrLimitExceeded reports an archive that exceeds a configured resource bound.
	ErrLimitExceeded = errors.New("Fleet archive limit exceeded")
	// ErrInvalidTools reports a malformed Fleet tools.json manifest.
	ErrInvalidTools = errors.New("invalid Fleet tools manifest")
	// ErrUnsafeTarget reports a target whose managed paths include links or
	// special files.
	ErrUnsafeTarget = errors.New("unsafe Fleet import target")

	agentNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	toolNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,256}$`)
	secretPart       = regexp.MustCompile(`(?i)(?:bearer[-_a-z0-9]*|token[-_a-z0-9]*|key[-_a-z0-9]*|secret[-_a-z0-9]*|cookie[-_a-z0-9]*|oauth[-_a-z0-9]*|sk-[A-Za-z0-9]{20,}|gh[opu]_[A-Za-z0-9]{20,}|lsv2_pt_[A-Za-z0-9]+)`)
	secretMarker     = regexp.MustCompile(`(?i)^(?:bearer|token|access[-_]?token|refresh[-_]?token|api[-_]?key|key|secret|cookie|oauth)$`)
	serverIDPart     = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
	windowsDevice    = regexp.MustCompile(`(?i)^(?:con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\..*)?$`)
)

// Limits bounds archive parsing and materialization. Zero fields select the
// defaults returned by DefaultLimits; limits cannot be disabled accidentally.
type Limits struct {
	MaxArchiveBytes      int64
	MaxEntries           int
	MaxUncompressedBytes uint64
	MaxFileBytes         uint64
	MaxCompressionRatio  uint64
	MaxTools             int
}

// DefaultLimits returns conservative finite import bounds matching the pinned
// Fleet import contract where applicable.
func DefaultLimits() Limits {
	return Limits{
		MaxArchiveBytes:      256 << 20,
		MaxEntries:           10_000,
		MaxUncompressedBytes: 256 << 20,
		MaxFileBytes:         256 << 20,
		MaxCompressionRatio:  100,
		MaxTools:             10_000,
	}
}

// Options controls optional import limits. Its zero value is useful and bounded.
type Options struct {
	Limits Limits
}

// Result describes a completed import.
type Result struct {
	TargetDir        string
	RootPrompts      int
	SubagentPrompts  int
	ConfigIgnored    bool
	MCPConfigWritten bool
	MCPSetupWritten  bool
	InterruptTools   []string
}

type toolRequest struct {
	name       string
	serverURL  string
	serverName string
	scope      string
	interrupt  bool
}

type serverSummary struct {
	serverURL  string
	serverName string
	scopes     map[string]struct{}
	tools      map[string]struct{}
	interrupts map[string]struct{}
}

// Import materializes archivePath into the explicit assistant state directory
// targetDir. Both paths are required positional inputs; all filesystem and
// archive failures are returned to the caller.
func Import(ctx context.Context, archivePath, targetDir string, options Options) (Result, error) {
	options.Limits.validateStaticBounds()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(archivePath) == "" {
		return Result{}, fmt.Errorf("%w: archive path is required", ErrInvalidArchive)
	}
	if strings.TrimSpace(targetDir) == "" {
		return Result{}, fmt.Errorf("%w: target directory is required", ErrUnsafeTarget)
	}
	limits := options.Limits.withDefaults()
	if err := validateLimits(limits); err != nil {
		return Result{}, err
	}
	archive, err := openArchive(archivePath, limits)
	if err != nil {
		return Result{}, err
	}
	defer archive.Close()
	entries, err := validateEntries(ctx, archive, limits)
	if err != nil {
		return Result{}, err
	}
	if _, ok := entries[rootPromptName]; !ok {
		return Result{}, fmt.Errorf("%w: %s is missing", ErrInvalidArchive, rootPromptName)
	}

	target, err := filepath.Abs(targetDir)
	if err != nil {
		return Result{}, fmt.Errorf("%w: resolve target: %v", ErrUnsafeTarget, err)
	}
	if err := prepareTargetParent(target); err != nil {
		return Result{}, err
	}
	workspace, err := os.MkdirTemp(filepath.Dir(target), ".fleet-import-*")
	if err != nil {
		return Result{}, fmt.Errorf("create Fleet import staging directory: %w", err)
	}
	defer os.RemoveAll(workspace)
	if err := os.Chmod(workspace, 0o700); err != nil {
		return Result{}, fmt.Errorf("secure Fleet import staging directory: %w", err)
	}
	payload := filepath.Join(workspace, "payload")
	if err := os.Mkdir(payload, 0o700); err != nil {
		return Result{}, fmt.Errorf("create Fleet import payload: %w", err)
	}

	result := Result{TargetDir: target, RootPrompts: 1}
	if err := materializePrompts(ctx, payload, entries, limits, &result); err != nil {
		return Result{}, err
	}
	summaries, interrupts, err := collectTools(ctx, entries, limits)
	if err != nil {
		return Result{}, err
	}
	result.ConfigIgnored = entries["config.json"] != nil
	result.InterruptTools = interrupts
	if len(summaries) > 0 {
		config, err := formatMCPConfig(summaries)
		if err != nil {
			return Result{}, err
		}
		if err := writePrivateFile(filepath.Join(payload, mcpConfigName), []byte(config)); err != nil {
			return Result{}, err
		}
		if err := writePrivateFile(filepath.Join(payload, mcpSetupName), []byte(formatSetup(filepath.Base(archivePath), summaries))); err != nil {
			return Result{}, err
		}
		result.MCPConfigWritten = true
		result.MCPSetupWritten = true
	}
	if err := commitManagedPaths(ctx, workspace, payload, target); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (limits Limits) validateStaticBounds() {
	switch {
	case limits.MaxArchiveBytes < 0:
		panic("datalon fleet: negative maximum archive size")
	case limits.MaxEntries < 0:
		panic("datalon fleet: negative maximum entry count")
	case limits.MaxTools < 0:
		panic("datalon fleet: negative maximum tool count")
	}
}

func (limits Limits) withDefaults() Limits {
	defaults := DefaultLimits()
	if limits.MaxArchiveBytes == 0 {
		limits.MaxArchiveBytes = defaults.MaxArchiveBytes
	}
	if limits.MaxEntries == 0 {
		limits.MaxEntries = defaults.MaxEntries
	}
	if limits.MaxUncompressedBytes == 0 {
		limits.MaxUncompressedBytes = defaults.MaxUncompressedBytes
	}
	if limits.MaxFileBytes == 0 {
		limits.MaxFileBytes = defaults.MaxFileBytes
	}
	if limits.MaxCompressionRatio == 0 {
		limits.MaxCompressionRatio = defaults.MaxCompressionRatio
	}
	if limits.MaxTools == 0 {
		limits.MaxTools = defaults.MaxTools
	}
	return limits
}

func validateLimits(limits Limits) error {
	if limits.MaxArchiveBytes <= 0 || limits.MaxEntries <= 0 || limits.MaxUncompressedBytes == 0 || limits.MaxFileBytes == 0 || limits.MaxCompressionRatio == 0 || limits.MaxTools <= 0 {
		return fmt.Errorf("%w: limits must be positive", ErrLimitExceeded)
	}
	return nil
}

func openArchive(path string, limits Limits) (*zip.ReadCloser, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("open Fleet archive: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: archive must be a regular file", ErrUnsafeArchive)
	}
	if info.Size() > limits.MaxArchiveBytes {
		return nil, fmt.Errorf("%w: compressed archive exceeds %d bytes", ErrLimitExceeded, limits.MaxArchiveBytes)
	}
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed zip", ErrInvalidArchive)
	}
	return archive, nil
}

func validateEntries(ctx context.Context, archive *zip.ReadCloser, limits Limits) (map[string]*zip.File, error) {
	entries := make(map[string]*zip.File)
	var total uint64
	for _, file := range archive.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name, directory, err := normalizeArchiveName(file.Name)
		if err != nil {
			return nil, err
		}
		mode := file.Mode()
		if directory {
			if mode.Type() != 0 && !mode.IsDir() {
				return nil, fmt.Errorf("%w: %s is not a directory", ErrUnsafeArchive, name)
			}
			continue
		}
		if mode.Type() != 0 {
			return nil, fmt.Errorf("%w: %s is a symlink or special file", ErrUnsafeArchive, name)
		}
		if _, exists := entries[name]; exists {
			return nil, fmt.Errorf("%w: duplicate entry %s", ErrUnsafeArchive, name)
		}
		if len(entries) >= limits.MaxEntries {
			return nil, fmt.Errorf("%w: more than %d entries", ErrLimitExceeded, limits.MaxEntries)
		}
		if file.UncompressedSize64 > limits.MaxFileBytes {
			return nil, fmt.Errorf("%w: %s exceeds the per-file bound", ErrLimitExceeded, name)
		}
		if file.UncompressedSize64 > 0 && file.CompressedSize64 == 0 {
			return nil, fmt.Errorf("%w: %s has an invalid compressed size", ErrUnsafeArchive, name)
		}
		if file.CompressedSize64 > 0 && exceedsRatio(file.UncompressedSize64, file.CompressedSize64, limits.MaxCompressionRatio) {
			return nil, fmt.Errorf("%w: %s compression ratio exceeds %d", ErrLimitExceeded, name, limits.MaxCompressionRatio)
		}
		if file.UncompressedSize64 > limits.MaxUncompressedBytes-total {
			return nil, fmt.Errorf("%w: uncompressed archive exceeds %d bytes", ErrLimitExceeded, limits.MaxUncompressedBytes)
		}
		total += file.UncompressedSize64
		entries[name] = file
	}
	return entries, nil
}

func normalizeArchiveName(raw string) (string, bool, error) {
	if !utf8.ValidString(raw) || strings.IndexByte(raw, 0) >= 0 {
		return "", false, fmt.Errorf("%w: invalid entry name", ErrUnsafeArchive)
	}
	name := strings.ReplaceAll(raw, `\`, "/")
	directory := strings.HasSuffix(name, "/")
	name = strings.TrimSuffix(name, "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "//") {
		return "", false, fmt.Errorf("%w: %q is an unsafe zip path", ErrUnsafeArchive, raw)
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || unsafeWindowsPathPart(part) || strings.ContainsAny(part, "\r\n") {
			return "", false, fmt.Errorf("%w: %q is an unsafe zip path", ErrUnsafeArchive, raw)
		}
	}
	if len(parts[0]) >= 2 && ((parts[0][0] >= 'A' && parts[0][0] <= 'Z') || (parts[0][0] >= 'a' && parts[0][0] <= 'z')) && parts[0][1] == ':' {
		return "", false, fmt.Errorf("%w: %q is an unsafe zip path", ErrUnsafeArchive, raw)
	}
	return strings.Join(parts, "/"), directory, nil
}

func unsafeWindowsPathPart(part string) bool {
	return strings.Contains(part, ":") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || windowsDevice.MatchString(part)
}

func exceedsRatio(uncompressed, compressed, maximum uint64) bool {
	return uncompressed/compressed > maximum || (uncompressed/compressed == maximum && uncompressed%compressed != 0)
}

func materializePrompts(ctx context.Context, payload string, entries map[string]*zip.File, limits Limits, result *Result) error {
	if err := copyEntry(ctx, entries[rootPromptName], filepath.Join(payload, rootPromptName), limits.MaxFileBytes); err != nil {
		return err
	}
	names := sortedEntryNames(entries)
	for _, name := range names {
		entry := entries[name]
		switch {
		case strings.HasPrefix(name, "skills/"):
			if err := copyEntry(ctx, entry, filepath.Join(payload, filepath.FromSlash(name)), limits.MaxFileBytes); err != nil {
				return err
			}
		case isSubagentFile(name, rootPromptName):
			parts := strings.Split(name, "/")
			if err := validateAgentName(parts[1], name); err != nil {
				return err
			}
			if err := copyEntry(ctx, entry, filepath.Join(payload, "agents", parts[1], rootPromptName), limits.MaxFileBytes); err != nil {
				return err
			}
			result.SubagentPrompts++
		case isSubagentFile(name, "tools.json"):
			parts := strings.Split(name, "/")
			if err := validateAgentName(parts[1], name); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyEntry(ctx context.Context, entry *zip.File, target string, maximum uint64) error {
	if entry == nil {
		return fmt.Errorf("%w: missing archive entry", ErrInvalidArchive)
	}
	directory := filepath.Dir(target)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Fleet output directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("secure Fleet output directory: %w", err)
	}
	source, err := entry.Open()
	if err != nil {
		return fmt.Errorf("read Fleet entry %s: %w", entry.Name, err)
	}
	defer source.Close()
	destination, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create Fleet output %s: %w", entry.Name, err)
	}
	ok := false
	defer func() {
		_ = destination.Close()
		if !ok {
			_ = os.Remove(target)
		}
	}()
	buffer := make([]byte, copyBufferSize)
	var copied uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			if uint64(count) > maximum-copied || copied+uint64(count) > entry.UncompressedSize64 {
				return fmt.Errorf("%w: %s expanded beyond its declared size", ErrLimitExceeded, entry.Name)
			}
			if _, err := destination.Write(buffer[:count]); err != nil {
				return fmt.Errorf("write Fleet output %s: %w", entry.Name, err)
			}
			copied += uint64(count)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read Fleet entry %s: %w", entry.Name, readErr)
		}
	}
	if copied != entry.UncompressedSize64 {
		return fmt.Errorf("%w: %s size does not match its header", ErrUnsafeArchive, entry.Name)
	}
	if err := destination.Sync(); err != nil {
		return fmt.Errorf("sync Fleet output %s: %w", entry.Name, err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close Fleet output %s: %w", entry.Name, err)
	}
	ok = true
	return nil
}

func isSubagentFile(name, filename string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 3 && parts[0] == "subagents" && parts[2] == filename
}

func validateAgentName(name, archivePath string) error {
	if name == "." || name == ".." || !agentNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %s has an unsafe subagent name", ErrUnsafeArchive, archivePath)
	}
	return nil
}

func sortedEntryNames(entries map[string]*zip.File) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

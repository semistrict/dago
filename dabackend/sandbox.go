package dabackend

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SandboxBackendProtocol is the minimal transport contract required by
// BaseSandbox. Implementations provide identity, command execution, and file
// upload; BaseSandbox derives the complete Backend file API from those
// primitives.
//
// Execute may use duration zero as its transport-defined default. Transports
// that distinguish an omitted timeout from an explicit zero may additionally
// implement ExecuteWithOptions.
type SandboxBackendProtocol interface {
	ID() string
	Execute(context.Context, string, time.Duration) (ExecuteResult, error)
	Upload(context.Context, []Upload) []UploadResult
}

// BaseSandboxOptions configures a BaseSandbox.
type BaseSandboxOptions struct {
	// EnableCaptureOffload opts into the POSIX shell and coreutils capture
	// wrapper used by ExecuteWithOffload. It is disabled by default because not
	// every sandbox image provides those commands.
	EnableCaptureOffload bool
	// MaxCaptureBytes bounds a captured command stream on the sandbox. Zero
	// selects 10 MiB.
	MaxCaptureBytes int
	// MaxResults bounds Glob results, and Grep results when its caller does not
	// provide a cap. Zero selects 1,000.
	MaxResults int
}

// BaseSandbox derives Backend operations from a small sandbox transport. File
// helpers require python3 in the sandbox image. They do not add isolation:
// every path reachable by the supplied shell is reachable by these operations.
type BaseSandbox struct {
	transport            SandboxBackendProtocol
	enableCaptureOffload bool
	maxCaptureBytes      int
	maxResults           int
}

const (
	defaultSandboxCaptureBytes = 10 * 1024 * 1024
	baseSandboxEditInlineBytes = 32 * 1024
	captureSentinel            = "__DAGO_EXEC_META__"
)

// NewBaseSandbox constructs a complete Sandbox from a minimal transport.
func NewBaseSandbox(transport SandboxBackendProtocol, options BaseSandboxOptions) *BaseSandbox {
	if nilInterface(transport) {
		panic("base sandbox: transport is required")
	}
	if strings.TrimSpace(transport.ID()) == "" {
		panic("base sandbox: transport id is required")
	}
	if options.MaxCaptureBytes < 0 {
		panic("base sandbox: max capture bytes cannot be negative")
	}
	if options.MaxCaptureBytes == 0 {
		options.MaxCaptureBytes = defaultSandboxCaptureBytes
	}
	if options.MaxResults < 0 {
		panic("base sandbox: max results cannot be negative")
	}
	if options.MaxResults == 0 {
		options.MaxResults = 1000
	}
	return &BaseSandbox{
		transport:            transport,
		enableCaptureOffload: options.EnableCaptureOffload,
		maxCaptureBytes:      options.MaxCaptureBytes,
		maxResults:           options.MaxResults,
	}
}

func (sandbox *BaseSandbox) ID() string { return sandbox.transport.ID() }

func (sandbox *BaseSandbox) Execute(ctx context.Context, command string, timeout time.Duration) (ExecuteResult, error) {
	return sandbox.transport.Execute(ctx, command, timeout)
}

func (sandbox *BaseSandbox) ExecuteWithOptions(ctx context.Context, command string, options ExecuteOptions) (ExecuteResult, error) {
	if configurable, ok := sandbox.transport.(interface {
		ExecuteWithOptions(context.Context, string, ExecuteOptions) (ExecuteResult, error)
	}); ok {
		return configurable.ExecuteWithOptions(ctx, command, options)
	}
	if options.Timeout == nil {
		return sandbox.transport.Execute(ctx, command, 0)
	}
	return sandbox.transport.Execute(ctx, command, *options.Timeout)
}

// ExecuteOffloadOptions controls capture-at-source command execution.
type ExecuteOffloadOptions struct {
	MaxInlineBytes  int
	MaxCaptureBytes int
	Timeout         *time.Duration
}

// ExecuteOffloadResult reports whether the complete output was retained at the
// requested sandbox path. When Offloaded is true, Response.Output is a bounded
// head/tail preview.
type ExecuteOffloadResult struct {
	Offloaded bool
	Response  ExecuteResult
}

// CaptureOffloader is the optional extension used by filesystem middleware to
// avoid transporting large command output through the agent process.
type CaptureOffloader interface {
	Sandbox
	ExecuteWithOffload(context.Context, string, string, ExecuteOffloadOptions) (ExecuteOffloadResult, error)
}

// CaptureOffloaderOf resolves capture-at-source only when capturePath belongs
// to the same backend that owns command execution. Composite routed mounts are
// rejected because their files are not necessarily visible to the default
// backend's shell.
func CaptureOffloaderOf(value Backend, capturePath string) (CaptureOffloader, bool) {
	if composite, ok := value.(*Composite); ok {
		selected, inner, prefix := composite.selectBackend(capturePath)
		if prefix != "" || selected != composite.defaultBackend || inner != capturePath {
			return nil, false
		}
		return CaptureOffloaderOf(composite.defaultBackend, capturePath)
	}
	offloader, ok := value.(CaptureOffloader)
	return offloader, ok
}

// ExecuteWithOffload executes command normally unless capture was explicitly
// enabled at construction. With capture enabled, a POSIX wrapper retains large
// output at capturePath and returns only a preview while preserving exit status.
func (sandbox *BaseSandbox) ExecuteWithOffload(ctx context.Context, command, capturePath string, options ExecuteOffloadOptions) (ExecuteOffloadResult, error) {
	if options.MaxInlineBytes <= 0 {
		return ExecuteOffloadResult{}, fmt.Errorf("base sandbox: max inline bytes must be positive")
	}
	if options.MaxCaptureBytes < 0 {
		return ExecuteOffloadResult{}, fmt.Errorf("base sandbox: max capture bytes cannot be negative")
	}
	if !sandbox.enableCaptureOffload {
		response, err := sandbox.ExecuteWithOptions(ctx, command, ExecuteOptions{Timeout: options.Timeout})
		return ExecuteOffloadResult{Response: response}, err
	}
	capBytes := options.MaxCaptureBytes
	if capBytes == 0 {
		capBytes = sandbox.maxCaptureBytes
	}
	wrapper, err := buildCaptureCommand(command, capturePath, options.MaxInlineBytes, capBytes)
	if err != nil {
		return ExecuteOffloadResult{}, err
	}
	response, err := sandbox.ExecuteWithOptions(ctx, wrapper, ExecuteOptions{Timeout: options.Timeout})
	if err != nil {
		return ExecuteOffloadResult{}, err
	}
	return parseCaptureOutput(response), nil
}

func buildCaptureCommand(command, capturePath string, inlineBytes, capBytes int) (string, error) {
	if capturePath == "" {
		return "", fmt.Errorf("base sandbox: capture path is required")
	}
	var random [10]byte
	for {
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("base sandbox: create capture delimiter: %w", err)
		}
		delim := "__DAGO_CMD_" + strings.TrimRight(base32.StdEncoding.EncodeToString(random[:]), "=") + "__"
		if strings.Contains(command, delim) {
			continue
		}
		quotedPath := shellQuote(capturePath)
		return fmt.Sprintf(`__da_f=%s
__da_ecf="$__da_f.ec"
mkdir -p "$(dirname "$__da_f")" 2>/dev/null
__da_cmd=$(cat <<'%s'
%s
%s
)
{ ( eval "$__da_cmd" ); echo "$?" > "$__da_ecf"; } 2>&1 | { head -c %d > "$__da_f"; cat > /dev/null; }
__da_ec=$(cat "$__da_ecf" 2>/dev/null)
: "${__da_ec:=1}"
rm -f "$__da_ecf"
__da_bytes=$(wc -c < "$__da_f" 2>/dev/null | tr -d ' ')
: "${__da_bytes:=0}"
__da_capped=0
[ "$__da_bytes" -ge %d ] && __da_capped=1
if [ "$__da_bytes" -le %d ]; then
  printf '%%s %%s %%s %%s\n' '%s' "$__da_ec" 0 "$__da_capped"
  cat "$__da_f"
  rm -f "$__da_f"
else
  __da_lines=$(wc -l < "$__da_f" 2>/dev/null | tr -d ' ')
  : "${__da_lines:=0}"
  __da_omitted=$((__da_lines - 10))
  printf '%%s %%s %%s %%s\n' '%s' "$__da_ec" 1 "$__da_capped"
  if [ "$__da_omitted" -gt 0 ]; then
    head -c 2000 "$__da_f" | head -n 5
    printf '... [%%s lines truncated] ...\n' "$__da_omitted"
    tail -c 2000 "$__da_f" | tail -n 5
  else
    head -c 4000 "$__da_f"
  fi
fi`, quotedPath, delim, command, delim, capBytes, capBytes, inlineBytes, captureSentinel, captureSentinel), nil
	}
}

func parseCaptureOutput(response ExecuteResult) ExecuteOffloadResult {
	first, body, found := strings.Cut(response.Output, "\n")
	parts := strings.Split(first, " ")
	if !found || len(parts) != 4 || parts[0] != captureSentinel {
		return ExecuteOffloadResult{Response: response}
	}
	exitCode, err := strconv.Atoi(parts[1])
	if err != nil || (parts[2] != "0" && parts[2] != "1") || (parts[3] != "0" && parts[3] != "1") {
		return ExecuteOffloadResult{Response: response}
	}
	response.Output = body
	response.ExitCode = new(exitCode)
	response.Truncated = response.Truncated || parts[3] == "1"
	return ExecuteOffloadResult{Offloaded: parts[2] == "1", Response: response}
}

type sandboxWire struct {
	Error       string            `json:"error"`
	Entries     []sandboxFileWire `json:"entries"`
	Matches     []sandboxFileWire `json:"matches"`
	GrepMatches []sandboxGrepWire `json:"grep_matches"`
	Truncated   bool              `json:"truncated"`
	Content     string            `json:"content"`
	Encoding    Encoding          `json:"encoding"`
	CreatedAt   float64           `json:"created_at"`
	ModifiedAt  float64           `json:"modified_at"`
	TotalLines  *int              `json:"total_lines"`
	StartLine   *int              `json:"start_line"`
	EndLine     *int              `json:"end_line"`
	NextOffset  *int              `json:"next_offset"`
	NoLines     bool              `json:"no_lines"`
	Count       int               `json:"count"`
}

type sandboxFileWire struct {
	Path       string  `json:"path"`
	IsDir      bool    `json:"is_dir"`
	Size       int64   `json:"size"`
	ModifiedAt float64 `json:"modified_at"`
}

type sandboxGrepWire struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Text   string `json:"text"`
	Before []struct {
		Line int    `json:"line"`
		Text string `json:"text"`
	} `json:"before"`
	After []struct {
		Line int    `json:"line"`
		Text string `json:"text"`
	} `json:"after"`
}

func (sandbox *BaseSandbox) runOperation(ctx context.Context, operation string, input any) (sandboxWire, error) {
	payload, err := json.Marshal(map[string]any{"op": operation, "input": input})
	if err != nil {
		return sandboxWire{}, fmt.Errorf("base sandbox %s: encode request: %w", operation, err)
	}
	command := "python3 - " + shellQuote(base64.StdEncoding.EncodeToString(payload)) + " <<'__DAGO_SANDBOX_PY__'\n" + baseSandboxPython + "\n__DAGO_SANDBOX_PY__"
	result, err := sandbox.ExecuteWithOptions(ctx, command, ExecuteOptions{})
	if err != nil {
		return sandboxWire{}, fmt.Errorf("base sandbox %s: %w", operation, err)
	}
	if result.ExitCode != nil && *result.ExitCode != 0 {
		return sandboxWire{}, fmt.Errorf("base sandbox %s: command failed with exit code %d: %s", operation, *result.ExitCode, boundedDetail(result.Output))
	}
	var wire sandboxWire
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Output)), &wire); err != nil {
		return sandboxWire{}, fmt.Errorf("base sandbox %s: invalid response %q: %w", operation, boundedDetail(result.Output), err)
	}
	if wire.Error != "" {
		return sandboxWire{}, fmt.Errorf("base sandbox %s: %s", operation, wire.Error)
	}
	return wire, nil
}

func boundedDetail(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 300 {
		return value[:300] + "..."
	}
	return value
}

func unixTime(value float64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	seconds, fraction := mathModf(value)
	return time.Unix(int64(seconds), int64(fraction*float64(time.Second))).UTC()
}

// mathModf is kept local so the sandbox adapter does not otherwise need
// floating-point helpers.
func mathModf(value float64) (float64, float64) {
	whole := float64(int64(value))
	return whole, value - whole
}

func (sandbox *BaseSandbox) List(ctx context.Context, directory string) (ListResult, error) {
	wire, err := sandbox.runOperation(ctx, "list", map[string]any{"path": directory})
	if err != nil {
		return ListResult{}, err
	}
	entries := make([]FileInfo, len(wire.Entries))
	for index, item := range wire.Entries {
		entries[index] = FileInfo{Path: item.Path, IsDir: item.IsDir, Size: item.Size, ModifiedAt: unixTime(item.ModifiedAt)}
	}
	return ListResult{Entries: entries}, nil
}

func (sandbox *BaseSandbox) Read(ctx context.Context, name string, offset, limit int) (ReadResult, error) {
	return sandbox.read(ctx, name, offset, limit, MaxSandboxBinaryPreviewBytes, false)
}

func (sandbox *BaseSandbox) ReadBinary(ctx context.Context, name string, maxBytes int64) (ReadResult, error) {
	if maxBytes <= 0 {
		maxBytes = MaxSandboxBinaryPreviewBytes
	}
	return sandbox.read(ctx, name, 0, 0, maxBytes, true)
}

func (sandbox *BaseSandbox) read(ctx context.Context, name string, offset, limit int, maxBinary int64, forceBinary bool) (ReadResult, error) {
	wire, err := sandbox.runOperation(ctx, "read", map[string]any{
		"path": name, "offset": offset, "limit": limit, "max_binary": maxBinary, "force_binary": forceBinary,
	})
	if err != nil {
		if strings.Contains(err.Error(), "payload too large") {
			return ReadResult{}, fmt.Errorf("%w: %s", ErrPayloadTooLarge, err)
		}
		return ReadResult{}, err
	}
	data := &FileData{Content: wire.Content, Encoding: wire.Encoding, CreatedAt: unixTime(wire.CreatedAt), ModifiedAt: unixTime(wire.ModifiedAt)}
	return ReadResult{Data: data, TotalLines: wire.TotalLines, StartLine: wire.StartLine, EndLine: wire.EndLine, NextOffset: wire.NextOffset, NoLinesRequested: wire.NoLines}, nil
}

func (sandbox *BaseSandbox) Write(ctx context.Context, name, content string) (WriteResult, error) {
	if _, err := sandbox.runOperation(ctx, "mkdir_parent", map[string]any{"path": name}); err != nil {
		return WriteResult{}, err
	}
	results := sandbox.transport.Upload(ctx, []Upload{{Path: name, Content: []byte(content)}})
	if len(results) != 1 {
		return WriteResult{}, fmt.Errorf("base sandbox write: upload returned %d results, want 1", len(results))
	}
	if results[0].Error != "" {
		return WriteResult{}, fmt.Errorf("base sandbox write %q: %s", name, results[0].Error)
	}
	return WriteResult{Path: name}, nil
}

func (sandbox *BaseSandbox) Edit(ctx context.Context, name, old, replacement string, replaceAll bool) (EditResult, error) {
	if old == "" || old == replacement {
		return EditResult{}, fmt.Errorf("base sandbox edit %q: old string must be non-empty and differ from replacement", name)
	}
	input := map[string]any{"path": name, "old": old, "new": replacement, "replace_all": replaceAll}
	operation := "edit"
	if len(old)+len(replacement) > baseSandboxEditInlineBytes {
		suffix, err := randomSandboxSuffix()
		if err != nil {
			return EditResult{}, err
		}
		oldPath, newPath := "/tmp/.dago_edit_"+suffix+"_old", "/tmp/.dago_edit_"+suffix+"_new"
		results := sandbox.transport.Upload(ctx, []Upload{{Path: oldPath, Content: []byte(old)}, {Path: newPath, Content: []byte(replacement)}})
		if len(results) != 2 {
			return EditResult{}, fmt.Errorf("base sandbox edit %q: upload returned %d results, want 2", name, len(results))
		}
		for _, result := range results {
			if result.Error != "" {
				return EditResult{}, fmt.Errorf("base sandbox edit %q: %s", name, result.Error)
			}
		}
		operation = "edit_uploaded"
		input = map[string]any{"path": name, "old_path": oldPath, "new_path": newPath, "replace_all": replaceAll}
	}
	wire, err := sandbox.runOperation(ctx, operation, input)
	if err != nil {
		return EditResult{}, err
	}
	return EditResult{Path: name, Occurrences: wire.Count}, nil
}

func randomSandboxSuffix() (string, error) {
	var value [10]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("base sandbox: create temporary file name: %w", err)
	}
	return strings.ToLower(strings.TrimRight(base32.StdEncoding.EncodeToString(value[:]), "=")), nil
}

func (sandbox *BaseSandbox) Delete(ctx context.Context, name string) (DeleteResult, error) {
	if _, err := sandbox.runOperation(ctx, "delete", map[string]any{"path": name}); err != nil {
		return DeleteResult{}, err
	}
	return DeleteResult{Path: name}, nil
}

func (sandbox *BaseSandbox) Glob(ctx context.Context, pattern, base string) (GlobResult, error) {
	if base == "" {
		base = "/"
	}
	wire, err := sandbox.runOperation(ctx, "glob", map[string]any{"path": base, "pattern": pattern, "max_results": sandbox.maxResults})
	if err != nil {
		return GlobResult{}, err
	}
	matches := make([]FileInfo, len(wire.Matches))
	for index, item := range wire.Matches {
		matches[index] = FileInfo{Path: item.Path, IsDir: item.IsDir, Size: item.Size, ModifiedAt: unixTime(item.ModifiedAt)}
	}
	return GlobResult{Matches: matches, Truncated: wire.Truncated}, nil
}

func (sandbox *BaseSandbox) Grep(ctx context.Context, pattern string, options GrepOptions) (GrepResult, error) {
	if err := ValidateGrepOptions(options); err != nil {
		return GrepResult{}, err
	}
	base := options.Path
	if base == "" {
		base = "/"
	}
	maxResults := options.MaxCount
	if !options.Uncapped && maxResults == 0 {
		maxResults = sandbox.maxResults
	}
	wire, err := sandbox.runOperation(ctx, "grep", map[string]any{
		"path": base, "pattern": pattern, "glob": options.Glob, "max_results": maxResults, "uncapped": options.Uncapped, "context": options.ContextLines,
	})
	if err != nil {
		return GrepResult{}, err
	}
	matches := make([]GrepMatch, len(wire.GrepMatches))
	for index, item := range wire.GrepMatches {
		match := GrepMatch{Path: item.Path, Line: item.Line, Text: item.Text}
		for _, line := range item.Before {
			match.ContextBefore = append(match.ContextBefore, ContextLine{Line: line.Line, Text: line.Text})
		}
		for _, line := range item.After {
			match.ContextAfter = append(match.ContextAfter, ContextLine{Line: line.Line, Text: line.Text})
		}
		matches[index] = match
	}
	return GrepResult{Matches: matches, Truncated: wire.Truncated}, nil
}

func (sandbox *BaseSandbox) Upload(ctx context.Context, uploads []Upload) []UploadResult {
	return sandbox.transport.Upload(ctx, uploads)
}

func (sandbox *BaseSandbox) Download(ctx context.Context, names []string) []DownloadResult {
	results := make([]DownloadResult, len(names))
	for index, name := range names {
		wire, err := sandbox.runOperation(ctx, "download", map[string]any{"path": name})
		results[index].Path = name
		if err != nil {
			results[index].Error = err.Error()
			continue
		}
		content, err := base64.StdEncoding.DecodeString(wire.Content)
		if err != nil {
			results[index].Error = fmt.Sprintf("base sandbox download: decode %q: %v", name, err)
			continue
		}
		results[index].Content = content
	}
	return results
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

var _ Sandbox = (*BaseSandbox)(nil)
var _ ConfigurableSandbox = (*BaseSandbox)(nil)
var _ BoundedBinaryReader = (*BaseSandbox)(nil)
var _ CaptureOffloader = (*BaseSandbox)(nil)

var baseSandboxPython = strings.NewReplacer(
	"__MAX_OUTPUT__", strconv.Itoa(MaxSandboxReadOutputBytes),
	"__TRUNCATION_MESSAGE__", strconv.Quote(SandboxReadTruncationMessage),
).Replace(`import base64, fnmatch, glob, json, os, pathlib, shutil, sys

request = json.loads(base64.b64decode(sys.argv[1]).decode("utf-8"))
op = request["op"]
x = request["input"]

def emit(value):
    print(json.dumps(value, separators=(",", ":")))

def info(name):
    st = os.stat(name)
    return {"path": name, "is_dir": os.path.isdir(name), "size": st.st_size, "modified_at": st.st_mtime}

try:
    if op == "list":
        values = []
        with os.scandir(x["path"]) as entries:
            for entry in entries:
                values.append(info(os.path.join(x["path"], entry.name)))
        values.sort(key=lambda item: item["path"])
        emit({"entries": values})
    elif op == "read":
        st = os.stat(x["path"])
        if os.path.isdir(x["path"]):
            raise IsADirectoryError(x["path"])
        with open(x["path"], "rb") as source:
            raw = source.read(x["max_binary"] + 1)
        if x["force_binary"]:
            if len(raw) > x["max_binary"]:
                raise ValueError("payload too large")
            emit({"content": base64.b64encode(raw).decode("ascii"), "encoding": "base64", "created_at": st.st_ctime, "modified_at": st.st_mtime})
        else:
          try:
            text = raw.decode("utf-8")
          except UnicodeDecodeError:
            if len(raw) > x["max_binary"]:
                raise ValueError("payload too large")
            emit({"content": base64.b64encode(raw).decode("ascii"), "encoding": "base64", "created_at": st.st_ctime, "modified_at": st.st_mtime})
          else:
            if len(raw) > x["max_binary"]:
                with open(x["path"], "rb") as source:
                    text = source.read().decode("utf-8")
            if text == "":
                emit({"content": "", "encoding": "utf-8", "created_at": st.st_ctime, "modified_at": st.st_mtime})
            elif x["limit"] <= 0:
                emit({"content": "", "encoding": "utf-8", "created_at": st.st_ctime, "modified_at": st.st_mtime, "no_lines": True})
            else:
                lines = text.replace("\r\n", "\n").replace("\r", "\n").split("\n")
                if lines and lines[-1] == "":
                    lines.pop()
                offset = max(x["offset"], 0)
                if offset >= len(lines):
                    raise ValueError("line offset %d exceeds file length (%d lines)" % (offset, len(lines)))
                selected = lines[offset:offset + x["limit"]]
                content = "\n".join(selected)
                cap = __MAX_OUTPUT__ - len(__TRUNCATION_MESSAGE__)
                truncated = len(content.encode("utf-8")) > cap
                if truncated:
                    encoded = content.encode("utf-8")[:cap]
                    while True:
                        try:
                            content = encoded.decode("utf-8")
                            break
                        except UnicodeDecodeError:
                            encoded = encoded[:-1]
                    shown = max(1, content.count("\n") + 1)
                    content += __TRUNCATION_MESSAGE__
                else:
                    shown = len(selected)
                end = offset + shown
                response = {"content": content, "encoding": "utf-8", "created_at": st.st_ctime, "modified_at": st.st_mtime, "total_lines": len(lines), "start_line": offset + 1, "end_line": end}
                if end < len(lines):
                    response["next_offset"] = end
                emit(response)
    elif op == "mkdir_parent":
        os.makedirs(os.path.dirname(x["path"]) or ".", exist_ok=True)
        emit({})
    elif op in ("edit", "edit_uploaded"):
        old_path = x.get("old_path")
        new_path = x.get("new_path")
        try:
            if old_path:
                with open(old_path, "rb") as source:
                    old = source.read().decode("utf-8")
                with open(new_path, "rb") as source:
                    new = source.read().decode("utf-8")
            else:
                old, new = x["old"], x["new"]
            with open(x["path"], "rb") as source:
                text = source.read().decode("utf-8")
            candidates = [(old, new), (old.replace("\r\n", "\n").replace("\n", "\r\n"), new.replace("\r\n", "\n").replace("\n", "\r\n")), (old.replace("\r\n", "\n"), new.replace("\r\n", "\n"))]
            count = 0
            for matched_old, matched_new in candidates:
                count = text.count(matched_old)
                if count:
                    break
            if count == 0:
                raise ValueError("string not found")
            if count > 1 and not x["replace_all"]:
                raise ValueError("string appears multiple times; enable replace_all")
            result = text.replace(matched_old, matched_new) if x["replace_all"] else text.replace(matched_old, matched_new, 1)
            with open(x["path"], "wb") as target:
                target.write(result.encode("utf-8"))
            emit({"count": count})
        finally:
            if old_path:
                for temporary in (old_path, new_path):
                    try:
                        os.unlink(temporary)
                    except OSError:
                        pass
    elif op == "delete":
        name = x["path"]
        if not os.path.lexists(name):
            raise FileNotFoundError(name)
        if os.path.isdir(name) and not os.path.islink(name):
            shutil.rmtree(name)
        else:
            os.unlink(name)
        emit({})
    elif op == "glob":
        root = os.path.realpath(x["path"])
        pattern = x["pattern"].lstrip("/")
        if ".." in pathlib.PurePosixPath(pattern).parts:
            raise ValueError("glob contains path traversal")
        values = []
        for relative in sorted(glob.glob(pattern, root_dir=root, recursive=True)):
            candidate = os.path.realpath(os.path.join(root, relative))
            if candidate != root and not candidate.startswith(root + os.sep):
                continue
            values.append(info(os.path.join(x["path"], relative)))
            if len(values) > x["max_results"]:
                break
        emit({"matches": values[:x["max_results"]], "truncated": len(values) > x["max_results"]})
    elif op == "grep":
        root = x["path"]
        names = []
        if os.path.isfile(root):
            names = [root]
        elif x["glob"]:
            real_root = os.path.realpath(root)
            relative_glob = x["glob"].lstrip("/")
            if ".." in pathlib.PurePosixPath(relative_glob).parts:
                raise ValueError("glob contains path traversal")
            for relative in sorted(glob.glob(relative_glob, root_dir=real_root, recursive=True)):
                name = os.path.realpath(os.path.join(real_root, relative))
                if name != real_root and name.startswith(real_root + os.sep) and os.path.isfile(name):
                    names.append(name)
        else:
            for directory, dirs, files in os.walk(root):
                dirs.sort()
                for filename in sorted(files):
                    names.append(os.path.join(directory, filename))
        matches = []
        cap = None if x["uncapped"] else x["max_results"]
        for name in names:
            try:
                with open(name, "r", encoding="utf-8", errors="ignore") as source:
                    lines = source.read().splitlines()
            except OSError:
                continue
            for index, line in enumerate(lines):
                if x["pattern"] not in line:
                    continue
                radius = x["context"]
                before = [{"line": i + 1, "text": lines[i]} for i in range(max(0, index - radius), index)]
                after = [{"line": i + 1, "text": lines[i]} for i in range(index + 1, min(len(lines), index + radius + 1))]
                matches.append({"path": name, "line": index + 1, "text": line, "before": before, "after": after})
                if cap is not None and len(matches) > cap:
                    break
            if cap is not None and len(matches) > cap:
                break
        emit({"grep_matches": matches[:cap] if cap is not None else matches, "truncated": cap is not None and len(matches) > cap})
    elif op == "download":
        with open(x["path"], "rb") as source:
            emit({"content": base64.b64encode(source.read()).decode("ascii")})
    else:
        raise ValueError("unknown operation")
except Exception as error:
    emit({"error": "%s: %s" % (type(error).__name__, error)})`)

package dago

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
)

const (
	// ManagedMemoryBlockStart begins the machine-owned portion of a memory file.
	ManagedMemoryBlockStart = "<!-- deepagents:onboarding-name:start -->"
	// ManagedMemoryBlockEnd ends the machine-owned portion of a memory file.
	ManagedMemoryBlockEnd = "<!-- deepagents:onboarding-name:end -->"
)

const (
	// The fallback line diff is quadratic. These limits cap its table at about
	// 32 MiB and skip expensive repeated comparisons for unusually large files;
	// the guard rolls the complete file back when either limit is exceeded.
	maxManagedMemoryDiffLines = 2048
	maxManagedMemoryDiffBytes = 4 << 20
)

// ManagedMemoryGuard protects machine-owned blocks in the supplied memory
// paths. The backend and guarded paths are required inputs; paths use the same
// virtual namespace as filesystem tools. The returned middleware is safe to
// share with subagents that share the backend.
//
// The guard must run inside the filesystem middleware so runtime-backed
// backends have already been bound. NewAgent arranges that ordering
// automatically for Memory.Sources.
func ManagedMemoryGuard(backend dabackend.Backend, guardedPath string, additionalPaths ...string) dagent.Middleware {
	if nilInterface(backend) {
		panic("managed memory guard backend is nil")
	}
	guardedPaths := append([]string{guardedPath}, additionalPaths...)
	guarded := make(map[string]bool, len(guardedPaths))
	for _, value := range guardedPaths {
		clean, ok := managedMemoryPath(value)
		if !ok {
			panic(fmt.Sprintf("managed memory guard path %q must be an absolute virtual path without traversal", value))
		}
		guarded[clean] = true
	}
	guard := managedMemoryGuard{backend: backend, guarded: guarded}
	return dagent.Middleware{
		Name:           "managed_memory_guard",
		SerializedName: "ManagedMemoryGuardMiddleware",
		WrapToolCall:   guard.wrapToolCall,
	}
}

type managedMemoryGuard struct {
	backend dabackend.Backend
	guarded map[string]bool
	mu      sync.Mutex
}

type managedBlockState uint8

const (
	managedBlockAbsent managedBlockState = iota
	managedBlockValid
	managedBlockMalformed
)

type managedBlock struct {
	state      managedBlockState
	start, end int
	content    string
}

func (guard *managedMemoryGuard) wrapToolCall(ctx context.Context, request dagent.ToolCallRequest, next dagent.ToolHandler) (dagent.ToolCallResponse, error) {
	if request.Call.Name != "write_file" && request.Call.Name != "edit_file" && request.Call.Name != "delete" {
		return next(ctx, request)
	}
	var input struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(request.Call.Arguments, &input) != nil {
		return next(ctx, request)
	}
	target, ok := managedMemoryPath(input.FilePath)
	if !ok {
		return next(ctx, request)
	}

	// One lock deliberately covers every guarded path. Tool calls may run in
	// parallel, including through subagents, and a delete of a parent directory
	// can overlap writes to several descendants.
	guard.mu.Lock()
	defer guard.mu.Unlock()

	paths := guard.pathsForCall(request.Call.Name, target)
	if request.Call.Name == "delete" && len(paths) == 0 {
		before, exists, err := guard.read(ctx, target)
		if err == nil && exists && inspectManagedBlock(before).state != managedBlockAbsent {
			return managedMemoryRejection(request, nil, target, "this path resolves to a file containing managed markers and must not be deleted through an alias"), nil
		}
	}
	if request.Call.Name != "delete" && len(paths) == 0 {
		// Resolve symlink aliases without trusting path-prefix comparisons: a
		// target that already contains our markers is guarded regardless of the
		// spelling used to reach it.
		before, exists, err := guard.read(ctx, target)
		if err != nil || !exists || inspectManagedBlock(before).state == managedBlockAbsent {
			return next(ctx, request)
		}
		return managedMemoryRejection(request, nil, target, "this path resolves to a file containing a managed block; use its configured memory path and do not edit through an alias"), nil
	}
	if len(paths) == 0 {
		return next(ctx, request)
	}

	before := make(map[string]string, len(paths))
	blocks := make(map[string]managedBlock, len(paths))
	for _, guardedPath := range paths {
		if inspector, ok := guard.backend.(interface {
			IsSymlink(context.Context, string) (bool, error)
		}); ok {
			symlink, inspectErr := inspector.IsSymlink(ctx, guardedPath)
			if inspectErr != nil && request.Call.Name != "delete" {
				return managedMemoryRejection(request, nil, guardedPath, "cannot safely inspect the guarded path; the operation was blocked"), nil
			}
			if symlink {
				return managedMemoryRejection(request, nil, guardedPath, "the configured memory path is a symlink; edit its reviewed canonical target instead"), nil
			}
		}
		content, exists, err := guard.read(ctx, guardedPath)
		if err != nil {
			return managedMemoryRejection(request, nil, guardedPath, "cannot safely inspect the guarded file; the operation was blocked"), nil
		}
		if !exists {
			continue
		}
		block := inspectManagedBlock(content)
		if block.state == managedBlockMalformed {
			return managedMemoryRejection(request, nil, guardedPath, "the managed markers are malformed or duplicated; repair them before editing the file"), nil
		}
		if block.state == managedBlockValid {
			before[guardedPath] = content
			blocks[guardedPath] = block
		}
	}
	if request.Call.Name == "delete" {
		if len(blocks) > 0 {
			for guardedPath := range blocks {
				return managedMemoryRejection(request, nil, guardedPath, "the guarded memory file contains a machine-managed block and must not be deleted"), nil
			}
		}
		return next(ctx, request)
	}
	if len(blocks) == 0 {
		return next(ctx, request)
	}

	response, callErr := next(ctx, request)
	for guardedPath, block := range blocks {
		outcome, keptOtherEdits := guard.restore(ctx, guardedPath, before[guardedPath], block)
		switch outcome {
		case restoreUnchanged:
			continue
		case restoreSucceeded:
			detail := "the managed block was restored and other file edits were kept"
			if !keptOtherEdits {
				detail = "the managed block was restored by rolling back the file because the edit could not be merged safely"
			}
			return managedMemoryRejection(request, &response, guardedPath, detail), nil
		case restoreFailed:
			return managedMemoryRejection(request, &response, guardedPath, "the managed block changed and could not be restored; do not rely on this edit having succeeded"), nil
		}
	}
	return response, callErr
}

func (guard *managedMemoryGuard) pathsForCall(tool, target string) []string {
	if tool != "delete" {
		if guard.guarded[target] {
			return []string{target}
		}
		return nil
	}
	paths := make([]string, 0, len(guard.guarded))
	for guardedPath := range guard.guarded {
		if guardedPath == target || strings.HasPrefix(guardedPath, strings.TrimSuffix(target, "/")+"/") {
			paths = append(paths, guardedPath)
		}
	}
	sort.Strings(paths)
	return paths
}

func (guard *managedMemoryGuard) read(ctx context.Context, filePath string) (string, bool, error) {
	results := guard.backend.Download(ctx, []string{filePath})
	if len(results) != 1 {
		return "", false, fmt.Errorf("managed memory backend returned %d downloads for one path", len(results))
	}
	result := results[0]
	if result.Error == "file_not_found" {
		return "", false, nil
	}
	if result.Error != "" {
		return "", false, fmt.Errorf("read managed memory %s: %s", filePath, result.Error)
	}
	if !utf8.Valid(result.Content) {
		return "", false, fmt.Errorf("read managed memory %s: content is not UTF-8", filePath)
	}
	return string(result.Content), true, nil
}

type restoreOutcome uint8

const (
	restoreUnchanged restoreOutcome = iota
	restoreSucceeded
	restoreFailed
)

func (guard *managedMemoryGuard) restore(ctx context.Context, filePath, before string, block managedBlock) (restoreOutcome, bool) {
	after, exists, err := guard.read(ctx, filePath)
	if err != nil || !exists {
		return restoreFailed, false
	}
	afterBlock := inspectManagedBlock(after)
	if afterBlock.state == managedBlockValid && afterBlock.content == block.content {
		return restoreUnchanged, true
	}
	restored, keptOtherEdits := restoreManagedMemoryContent(before, after, block)
	if inspect := inspectManagedBlock(restored); inspect.state != managedBlockValid || inspect.content != block.content {
		restored, keptOtherEdits = before, false
	}
	if _, err := dabackend.WriteDurable(ctx, guard.backend, filePath, restored); err != nil {
		return restoreFailed, keptOtherEdits
	}
	verified, exists, err := guard.read(ctx, filePath)
	if err != nil || !exists || verified != restored {
		return restoreFailed, keptOtherEdits
	}
	return restoreSucceeded, keptOtherEdits
}

func restoreManagedMemoryContent(before, after string, original managedBlock) (string, bool) {
	current := inspectManagedBlock(after)
	if current.state == managedBlockValid {
		return after[:current.start] + original.content + after[current.end:], true
	}
	cleaned, ok := removeManagedBlockEdits(before, after, original)
	if !ok {
		return before, false
	}
	cleaned = strings.ReplaceAll(cleaned, ManagedMemoryBlockStart, "")
	cleaned = strings.ReplaceAll(cleaned, ManagedMemoryBlockEnd, "")
	return appendManagedBlock(cleaned, original.content, newlineFor(before, after)), true
}

func inspectManagedBlock(content string) managedBlock {
	starts := strings.Count(content, ManagedMemoryBlockStart)
	ends := strings.Count(content, ManagedMemoryBlockEnd)
	if starts == 0 && ends == 0 {
		return managedBlock{state: managedBlockAbsent}
	}
	if starts != 1 || ends != 1 {
		return managedBlock{state: managedBlockMalformed}
	}
	start := strings.Index(content, ManagedMemoryBlockStart)
	endStart := strings.Index(content, ManagedMemoryBlockEnd)
	if start >= endStart {
		return managedBlock{state: managedBlockMalformed}
	}
	end := endStart + len(ManagedMemoryBlockEnd)
	return managedBlock{state: managedBlockValid, start: start, end: end, content: content[start:end]}
}

func managedMemoryPath(value string) (string, bool) {
	if value == "" || !strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') {
		return "", false
	}
	clean := path.Clean(value)
	if clean == "/" || strings.Contains(value, "\\") {
		return "", false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return "", false
		}
	}
	return clean, true
}

func appendUniqueStrings(existing []string, values ...string) []string {
	result := append([]string(nil), existing...)
	seen := make(map[string]bool, len(result)+len(values))
	for _, value := range result {
		seen[value] = true
	}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func managedMemoryRejection(request dagent.ToolCallRequest, previous *dagent.ToolCallResponse, filePath, detail string) dagent.ToolCallResponse {
	response := dagent.ToolCallResponse{}
	if previous != nil {
		response.Call = previous.Call
		response.Result.Update = previous.Result.Update
		response.Result.OtherUsage = previous.Result.OtherUsage
	}
	response.Result.Content = []damessage.ContentBlock{{
		Type: damessage.BlockText,
		Text: fmt.Sprintf("The region between the `%s` and `%s` markers in %s is machine-managed and must not be edited: %s.", ManagedMemoryBlockStart, ManagedMemoryBlockEnd, filePath, detail),
	}}
	response.Result.Status = damessage.ToolStatusError
	return response
}

func appendManagedBlock(content, block, newline string) string {
	base := strings.TrimRight(content, " \t\r\n")
	if base == "" {
		return block + newline
	}
	return base + newline + newline + block + newline
}

func newlineFor(values ...string) string {
	for _, value := range values {
		if strings.Contains(value, "\r\n") {
			return "\r\n"
		}
	}
	return "\n"
}

type diffStep struct {
	kind byte
	i, j int
}

func removeManagedBlockEdits(before, after string, block managedBlock) (string, bool) {
	beforeLines := splitLinesKeepEnds(before)
	afterLines := splitLinesKeepEnds(after)
	if len(before)+len(after) > maxManagedMemoryDiffBytes || len(beforeLines) > maxManagedMemoryDiffLines || len(afterLines) > maxManagedMemoryDiffLines {
		return "", false
	}
	blockStart, blockEnd, ok := managedBlockLineRange(before, block)
	if !ok {
		return "", false
	}
	rows := len(beforeLines) + 1
	columns := len(afterLines) + 1
	dp := make([]int, rows*columns)
	for i := len(beforeLines) - 1; i >= 0; i-- {
		for j := len(afterLines) - 1; j >= 0; j-- {
			index := i*columns + j
			if beforeLines[i] == afterLines[j] {
				dp[index] = dp[(i+1)*columns+j+1] + 1
			} else {
				dp[index] = max(dp[(i+1)*columns+j], dp[i*columns+j+1])
			}
		}
	}
	steps := make([]diffStep, 0, len(beforeLines)+len(afterLines))
	for i, j := 0, 0; i < len(beforeLines) || j < len(afterLines); {
		switch {
		case i < len(beforeLines) && j < len(afterLines) && beforeLines[i] == afterLines[j]:
			steps = append(steps, diffStep{kind: '=', i: i, j: j})
			i++
			j++
		case i < len(beforeLines) && (j == len(afterLines) || dp[(i+1)*columns+j] >= dp[i*columns+j+1]):
			steps = append(steps, diffStep{kind: '-', i: i, j: j})
			i++
		default:
			steps = append(steps, diffStep{kind: '+', i: i, j: j})
			j++
		}
	}
	remove := make([]bool, len(afterLines))
	for cursor := 0; cursor < len(steps); {
		if steps[cursor].kind == '=' {
			step := steps[cursor]
			if step.i >= blockStart && step.i < blockEnd {
				remove[step.j] = true
			}
			cursor++
			continue
		}
		beforeStart, beforeEnd := steps[cursor].i, steps[cursor].i
		afterStart, afterEnd := steps[cursor].j, steps[cursor].j
		for cursor < len(steps) && steps[cursor].kind != '=' {
			step := steps[cursor]
			if step.kind == '-' {
				beforeEnd = max(beforeEnd, step.i+1)
			} else {
				afterEnd = max(afterEnd, step.j+1)
			}
			cursor++
		}
		overlaps := beforeStart < blockEnd && blockStart < beforeEnd
		insertedInside := beforeStart > blockStart && beforeStart < blockEnd
		if overlaps || insertedInside {
			for index := afterStart; index < afterEnd; index++ {
				remove[index] = true
			}
		}
	}
	var output strings.Builder
	for index, line := range afterLines {
		if !remove[index] {
			output.WriteString(line)
		}
	}
	return output.String(), true
}

func managedBlockLineRange(content string, block managedBlock) (int, int, bool) {
	lines := splitLinesKeepEnds(content)
	offset := 0
	startLine, endLine := -1, -1
	for index, line := range lines {
		lineEnd := offset + len(line)
		if startLine < 0 && offset <= block.start && block.start < lineEnd {
			startLine = index
		}
		if offset < block.end && block.end <= lineEnd {
			endLine = index + 1
			break
		}
		offset = lineEnd
	}
	return startLine, endLine, startLine >= 0 && endLine > startLine
}

func splitLinesKeepEnds(value string) []string {
	if value == "" {
		return nil
	}
	lines := make([]string, 0, strings.Count(value, "\n")+1)
	for len(value) > 0 {
		index := strings.IndexByte(value, '\n')
		if index < 0 {
			lines = append(lines, value)
			break
		}
		lines = append(lines, value[:index+1])
		value = value[index+1:]
	}
	return lines
}

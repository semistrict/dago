package claudetool

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	shellLargeOutputThreshold = 50 * 1024
	shellFirstLinesCount      = 2
	shellLastLinesCount       = 5
	shellMaxLineLength        = 200
	shellProgressMaxBytes     = 10 * 1024
)

// formatShellForegroundOutput is shared by Shelley's yielding shell and its
// copied command-output contract tests. Dago owns ordinary execute results.
func formatShellForegroundOutput(out string) (string, error) {
	if len(out) <= shellLargeOutputThreshold {
		return out, nil
	}
	tmpDir, err := os.MkdirTemp("", "shelley-output-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir for large output: %w", err)
	}
	outFile := filepath.Join(tmpDir, "output")
	if err := os.WriteFile(outFile, []byte(out), 0o644); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to write large output to file: %w", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 3 {
		return fmt.Sprintf("[output too large (%s, %d lines), saved to: %s]", shellHumanizeBytes(len(out)), len(lines), outFile), nil
	}
	var result strings.Builder
	fmt.Fprintf(&result, "[output too large (%s, %d lines), saved to: %s]\n\n", shellHumanizeBytes(len(out)), len(lines), outFile)
	result.WriteString("First lines:\n")
	for index := range min(shellFirstLinesCount, len(lines)) {
		fmt.Fprintf(&result, "%5d: %s\n", index+1, shellTruncateLine(lines[index]))
	}
	result.WriteString("\n...\n\nLast lines:\n")
	for index := max(0, len(lines)-shellLastLinesCount); index < len(lines); index++ {
		fmt.Fprintf(&result, "%5d: %s\n", index+1, shellTruncateLine(lines[index]))
	}
	return result.String(), nil
}

func shellTruncateLine(line string) string {
	if len(line) <= shellMaxLineLength {
		return line
	}
	return line[:shellMaxLineLength] + "..."
}

func shellHumanizeBytes(size int) string {
	switch {
	case size < 4*1024:
		return fmt.Sprintf("%dB", size)
	case size < 1024*1024:
		return fmt.Sprintf("%dkB", int(math.Round(float64(size)/1024)))
	case size < 1024*1024*1024:
		return fmt.Sprintf("%dMB", int(math.Round(float64(size)/(1024*1024))))
	default:
		return "more than 1GB"
	}
}

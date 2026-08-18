package fleet

import (
	"fmt"
	"strings"
)

// FormatHandoff renders the operator-facing setup summary for a completed import.
func FormatHandoff(result Result) string {
	lines := []string{
		"Fleet import complete.",
		"Assistant state: " + result.TargetDir,
		fmt.Sprintf("Root prompts written: %d", result.RootPrompts),
		fmt.Sprintf("Subagent prompts written: %d", result.SubagentPrompts),
	}
	if result.ConfigIgnored {
		lines = append(lines, "config.json: ignored")
	} else {
		lines = append(lines, "config.json: not present")
	}
	lines = append(lines, "", "Next steps:")
	if result.MCPConfigWritten {
		lines = append(lines,
			"- Review .mcp.json before starting the assistant.",
			"- Review .mcp.json.setup for requested tools and OAuth setup details.",
		)
	} else {
		lines = append(lines,
			"- No Fleet MCP tool requirements were found.",
			"- Add MCP servers to .mcp.json if this assistant needs local tools.",
		)
	}
	if len(result.InterruptTools) > 0 {
		lines = append(lines, "- Preserve tool approval with "+interruptToolsEnv+"="+strings.Join(result.InterruptTools, ",")+".")
	}
	return strings.Join(lines, "\n") + "\n"
}

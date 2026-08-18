// Package daeval evaluates agent behavior from provider-neutral trajectories.
//
// The package has no provider, network, or tracing dependency. A Run may invoke
// a real agent, but deterministic model scripts are sufficient for normal tests.
package daeval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
)

// Step is one assistant action and the tool observations that immediately
// follow it. Indexes start at one.
type Step struct {
	Index        int                 `json:"index"`
	Action       damessage.Message   `json:"action"`
	Observations []damessage.Message `json:"observations,omitempty"`
}

// Trajectory is the observable behavior scored by an evaluation.
type Trajectory struct {
	Steps []Step            `json:"steps"`
	Files map[string]string `json:"files,omitempty"`
}

// TrajectoryFromResult constructs a trajectory from an agent result and an
// optional snapshot of files. Assistant messages begin steps; following tool
// messages become their observations. Other messages are context, not actions.
// The returned trajectory owns all of its data.
func TrajectoryFromResult(result dagent.Result, files map[string]string) Trajectory {
	trajectory := Trajectory{Files: cloneFiles(files)}
	for _, message := range result.Messages {
		switch message.Role {
		case damessage.RoleAssistant:
			trajectory.Steps = append(trajectory.Steps, Step{
				Index:  len(trajectory.Steps) + 1,
				Action: message.Clone(),
			})
		case damessage.RoleTool:
			if len(trajectory.Steps) == 0 {
				continue
			}
			last := &trajectory.Steps[len(trajectory.Steps)-1]
			last.Observations = append(last.Observations, message.Clone())
		}
	}
	return trajectory
}

// Answer returns the final assistant text, or an empty string when no assistant
// action was captured.
func (trajectory Trajectory) Answer() string {
	if len(trajectory.Steps) == 0 {
		return ""
	}
	return trajectory.Steps[len(trajectory.Steps)-1].Action.TextContent()
}

// ToolCallCount returns the number of requested tool calls across all steps.
func (trajectory Trajectory) ToolCallCount() int {
	total := 0
	for _, step := range trajectory.Steps {
		total += len(step.Action.ToolCalls)
	}
	return total
}

// String returns a stable, human-readable trajectory summary for failures.
func (trajectory Trajectory) String() string {
	var summary strings.Builder
	for _, step := range trajectory.Steps {
		fmt.Fprintf(&summary, "step %d:\n", step.Index)
		for _, call := range step.Action.ToolCalls {
			arguments := string(call.Arguments)
			if len(call.Arguments) == 0 {
				arguments = "{}"
			} else {
				var compact bytes.Buffer
				if json.Compact(&compact, call.Arguments) == nil {
					arguments = compact.String()
				}
			}
			fmt.Fprintf(&summary, "  - %s %s\n", call.Name, arguments)
		}
		if text := strings.TrimSpace(step.Action.TextContent()); text != "" {
			fmt.Fprintf(&summary, "  text: %s\n", strings.ReplaceAll(text, "\n", `\n`))
		}
	}
	return strings.TrimSuffix(summary.String(), "\n")
}

func cloneFiles(files map[string]string) map[string]string {
	if files == nil {
		return nil
	}
	cloned := make(map[string]string, len(files))
	for path, content := range files {
		cloned[path] = content
	}
	return cloned
}

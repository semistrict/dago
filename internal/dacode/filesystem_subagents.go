package dacode

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	dagoapi "github.com/semistrict/dago"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dasubagent"
)

const agentSubagentsDirectory = "agents"

func loadFilesystemSubagents(
	ctx context.Context,
	authentication modelAuthentication,
	baseURL, stateDirectory, projectDirectory, agentName string,
	generalSystem damessage.Message,
) ([]dagoapi.Subagent, error) {
	if ctx == nil {
		panic("dacode: subagent discovery context is required")
	}
	if stateDirectory == "" || projectDirectory == "" || agentName == "" {
		panic("dacode: subagent state, project, and agent names are required")
	}
	report, err := dasubagent.Discover(
		ctx,
		filepath.Join(stateDirectory, agentName, agentSubagentsDirectory),
		filepath.Join(projectDirectory, ".deepagents", agentSubagentsDirectory),
		dasubagent.Options{},
	)
	if err != nil {
		return nil, fmt.Errorf("discover custom subagents: %w", err)
	}
	for _, diagnostic := range report.Diagnostics {
		slog.Warn("Skipping custom subagent", "path", diagnostic.Path, "reason", diagnostic.Reason)
	}
	subagents := make([]dagoapi.Subagent, 0, len(report.Definitions)+1)
	hasGeneral := false
	for _, definition := range report.Definitions {
		var modelOverride damodel.Chat
		if definition.Model != "" {
			modelOverride, err = authentication.resolveModel(ctx, definition.Model, baseURL)
			if err != nil {
				return nil, fmt.Errorf("configure custom subagent %q model: %w", definition.Name, err)
			}
		}
		subagents = append(subagents, dagoapi.NewSubagent(
			definition.Name,
			definition.Description,
			modelOverride,
			dagoapi.WithSystemPrompt(definition.SystemPrompt),
		))
		hasGeneral = hasGeneral || definition.Name == "general-purpose"
	}
	if !hasGeneral {
		subagents = append(subagents, dacodeGeneralSubagent(generalSystem))
	}
	return subagents, nil
}

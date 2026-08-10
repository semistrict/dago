package claudetool

import (
	"time"

	dago "github.com/semistrict/dago"
	dtool "github.com/semistrict/dago/tool"
)

type SubagentRunner = dago.ConversationSubagentRunner
type SubagentDB = dago.ConversationSubagentStore
type AvailableModel = dago.ConversationSubagentModel
type subagentInput = dago.ConversationSubagentInput
type SubagentDisplayData = dago.ConversationSubagentDisplay

var subagentReasoningLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh"}

func isValidReasoningLevel(value string) bool {
	for _, level := range subagentReasoningLevels {
		if value == level {
			return true
		}
	}
	return false
}

type SubagentTool struct {
	DB                   SubagentDB
	ParentConversationID string
	WorkingDir           *MutableWorkingDir
	Runner               SubagentRunner
	ModelID              string
	AvailableModels      []AvailableModel
	ParentReasoning      string
}

const subagentName = "subagent"

const (
	subagentDefaultTimeout = 15 * time.Minute
	subagentMaxTimeout     = 60 * time.Minute
)

func (s *SubagentTool) NativeTool() dtool.Tool {
	var workingDirectory func() string
	if s.WorkingDir != nil {
		workingDirectory = s.WorkingDir.Get
	}
	return dago.ConversationSubagentTool(dago.ConversationSubagentOptions{
		Store:                s.DB,
		Runner:               s.Runner,
		ParentConversationID: s.ParentConversationID,
		WorkingDirectory:     workingDirectory,
		ModelID:              s.ModelID,
		AvailableModels:      s.AvailableModels,
		ParentReasoning:      s.ParentReasoning,
		ReasoningLevels:      subagentReasoningLevels,
		DefaultTimeout:       subagentDefaultTimeout,
		MaxTimeout:           subagentMaxTimeout,
	})
}

func sanitizeSlug(slug string) string {
	return dago.SanitizeSubagentSlug(slug)
}

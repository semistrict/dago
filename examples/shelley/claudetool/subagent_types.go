package claudetool

import dago "github.com/semistrict/dago"

// These aliases are Shelley's application configuration boundary for Dago's
// persistent conversation subagents.
type SubagentRunner = dago.ConversationSubagentRunner
type SubagentDB = dago.ConversationSubagentStore
type AvailableModel = dago.ConversationSubagentModel
type SubagentDisplayData = dago.ConversationSubagentDisplay

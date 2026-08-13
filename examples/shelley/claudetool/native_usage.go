package claudetool

import (
	"time"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
)

func nativePurposedUsage(purpose string, chat damodel.Chat, tokens *dmessage.Usage, started, finished time.Time) []dmessage.PurposedUsage {
	if tokens == nil {
		return nil
	}
	usage := *tokens
	profile := chat.Profile()
	usage.Provider = profile.Provider
	usage.Model = profile.Model
	usage.StartedAt = started.UTC()
	usage.FinishedAt = finished.UTC()
	return []dmessage.PurposedUsage{{Purpose: purpose, Usage: usage}}
}

package claudetool

import (
	"time"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
)

func nativePurposedUsage(purpose string, chat dmodel.Chat, tokens *dmessage.Usage, started, finished time.Time) []dmessage.PurposedUsage {
	if tokens == nil {
		return nil
	}
	usage := *tokens
	profile := chat.Profile()
	usage.Provider = profile.Provider
	usage.Model = profile.Model
	usage.StartedAt = started.UTC().Format(time.RFC3339Nano)
	usage.FinishedAt = finished.UTC().Format(time.RFC3339Nano)
	return []dmessage.PurposedUsage{{Purpose: purpose, Usage: usage}}
}

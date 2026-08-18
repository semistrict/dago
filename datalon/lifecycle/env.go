package lifecycle

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

var errInvalidDuration = errors.New("invalid retention duration")

const (
	CronRetentionEnv  = "DEEPAGENTS_TALON_CRON_RETENTION_DAYS"
	MediaRetentionEnv = "DEEPAGENTS_TALON_INBOUND_MEDIA_RETENTION_HOURS"
	MaxMediaBytesEnv  = "DEEPAGENTS_TALON_MAX_MEDIA_BYTES"
)

// OptionsFromEnv parses the pinned retention and global media limit variables.
// A nil map reads the process environment. External values can be malformed, so
// this loader returns an error while New remains a no-error static constructor.
func OptionsFromEnv(env map[string]string) (Options, error) {
	if env == nil {
		env = processEnvironment()
	}
	options := Options{}
	if raw, ok := env[CronRetentionEnv]; ok {
		value, err := nonNegativeDuration(raw, 24*time.Hour)
		if err != nil {
			return Options{}, fmt.Errorf("%s must be a bounded non-negative integer", CronRetentionEnv)
		}
		if value == 0 {
			options.ImmediateCronCleanup = true
		} else {
			options.CronRetention = value
		}
	}
	if raw, ok := env[MediaRetentionEnv]; ok {
		value, err := nonNegativeDuration(raw, time.Hour)
		if err != nil {
			return Options{}, fmt.Errorf("%s must be a bounded non-negative integer", MediaRetentionEnv)
		}
		if value == 0 {
			options.ImmediateMediaCleanup = true
		} else {
			options.MediaRetention = value
		}
	}
	if raw, ok := env[MaxMediaBytesEnv]; ok {
		value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || value <= 0 {
			return Options{}, fmt.Errorf("%s must be a positive integer byte count", MaxMediaBytesEnv)
		}
		options.MaxArtifactBytes = value
	}
	return options, nil
}

func nonNegativeDuration(raw string, unit time.Duration) (time.Duration, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 0 || value > math.MaxInt64/int64(unit) {
		return 0, errInvalidDuration
	}
	return time.Duration(value) * unit, nil
}

func processEnvironment() map[string]string {
	result := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

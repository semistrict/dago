package whatsapp

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// OpenAcknowledgement is required when ExposureOpen is selected.
	OpenAcknowledgement = "allow-arbitrary-senders"
	// MaxWhatsAppMediaBytes is the hard bridge cap. The Node provider decodes
	// downloads in memory, so larger cross-channel limits are not safe here.
	MaxWhatsAppMediaBytes int64 = 64 << 20
	// DefaultBotHeader distinguishes agent replies in self-message chats.
	DefaultBotHeader = "deepagents bot"

	defaultPollInterval   = time.Second
	defaultHealthInterval = 5 * time.Second
	defaultMaxTextBytes   = 1 << 20
	defaultMaxBridgeBytes = 1 << 20
	defaultMaxMessages    = 1_000
	defaultMaxMediaPaths  = 16
	defaultMaxErrorBytes  = 4_096
	maxTextRunes          = 4_096
)

// ExposureMode controls which inbound senders may invoke the host.
type ExposureMode string

const (
	ExposureSelf      ExposureMode = "self"
	ExposureAllowlist ExposureMode = "allowlist"
	ExposureOpen      ExposureMode = "open"
)

// Exposure is the static inbound trigger policy. OpenAcknowledgement must be
// exactly OpenAcknowledgement for open mode.
type Exposure struct {
	Mode                ExposureMode
	Conversations       []string
	MentionPatterns     []string
	OperatorIDs         []string
	OpenAcknowledgement string
}

// Options configures one attached loopback bridge channel. Zero values select
// bounded pinned defaults.
type Options struct {
	ID                 string
	InboundMediaDir    string
	OutboundMediaRoot  string
	Exposure           Exposure
	BotHeader          string
	MaxMediaBytes      int64
	PollInterval       time.Duration
	HealthInterval     time.Duration
	MaxTextBytes       int
	MaxBridgeBytes     int64
	MaxMessagesPerPoll int
	MaxMediaPaths      int
	MaxErrorBytes      int
}

type exposurePolicy struct {
	mode          ExposureMode
	conversations map[string]struct{}
	operators     map[string]struct{}
	mentions      []*regexp.Regexp
}

func prepareOptions(sessionDir string, options Options) (Options, exposurePolicy) {
	if strings.TrimSpace(sessionDir) == "" {
		panic("whatsapp channel: session directory is required")
	}
	if options.MaxMediaBytes < 0 || options.PollInterval < 0 || options.HealthInterval < 0 || options.MaxTextBytes < 0 || options.MaxBridgeBytes < 0 || options.MaxMessagesPerPoll < 0 || options.MaxMediaPaths < 0 || options.MaxErrorBytes < 0 {
		panic("whatsapp channel: limits cannot be negative")
	}
	if options.ID == "" {
		options.ID = "whatsapp"
	}
	options.ID = strings.TrimSpace(options.ID)
	if options.ID == "" || len(options.ID) > 128 || !utf8.ValidString(options.ID) {
		panic("whatsapp channel: ID must be 1-128 UTF-8 bytes")
	}
	if options.InboundMediaDir == "" {
		options.InboundMediaDir = filepath.Join(filepath.Dir(sessionDir), "media")
	}
	if options.OutboundMediaRoot == "" {
		options.OutboundMediaRoot = "."
	}
	if options.BotHeader == "" {
		options.BotHeader = DefaultBotHeader
	}
	options.BotHeader = strings.TrimSpace(options.BotHeader)
	if options.BotHeader == "" || len(options.BotHeader) > 256 || !utf8.ValidString(options.BotHeader) {
		panic("whatsapp channel: bot header must be 1-256 UTF-8 bytes")
	}
	if options.MaxMediaBytes == 0 || options.MaxMediaBytes > MaxWhatsAppMediaBytes {
		options.MaxMediaBytes = MaxWhatsAppMediaBytes
	}
	if options.PollInterval == 0 {
		options.PollInterval = defaultPollInterval
	}
	if options.HealthInterval == 0 {
		options.HealthInterval = defaultHealthInterval
	}
	if options.MaxTextBytes == 0 {
		options.MaxTextBytes = defaultMaxTextBytes
	}
	if options.MaxBridgeBytes == 0 {
		options.MaxBridgeBytes = defaultMaxBridgeBytes
	}
	if options.MaxMessagesPerPoll == 0 {
		options.MaxMessagesPerPoll = defaultMaxMessages
	}
	if options.MaxMediaPaths == 0 {
		options.MaxMediaPaths = defaultMaxMediaPaths
	}
	if options.MaxErrorBytes == 0 {
		options.MaxErrorBytes = defaultMaxErrorBytes
	}
	return options, prepareExposure(options.Exposure)
}

func prepareExposure(exposure Exposure) exposurePolicy {
	if exposure.Mode == "" {
		exposure.Mode = ExposureSelf
	}
	switch exposure.Mode {
	case ExposureSelf, ExposureAllowlist:
	case ExposureOpen:
		if exposure.OpenAcknowledgement != OpenAcknowledgement {
			panic("whatsapp channel: open exposure requires explicit acknowledgement " + OpenAcknowledgement)
		}
	default:
		panic(fmt.Sprintf("whatsapp channel: unsupported exposure mode %q", exposure.Mode))
	}
	policy := exposurePolicy{
		mode:          exposure.Mode,
		conversations: stringSet(exposure.Conversations, "conversation"),
		operators:     stringSet(exposure.OperatorIDs, "operator ID"),
		mentions:      make([]*regexp.Regexp, 0, len(exposure.MentionPatterns)),
	}
	if len(exposure.MentionPatterns) > 128 {
		panic("whatsapp channel: too many mention patterns")
	}
	for _, pattern := range exposure.MentionPatterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" || len(pattern) > 256 || !utf8.ValidString(pattern) {
			panic("whatsapp channel: mention patterns must be 1-256 UTF-8 bytes")
		}
		compiled, err := regexp.Compile(globRegexp(pattern))
		if err != nil {
			panic("whatsapp channel: compile mention pattern: " + err.Error())
		}
		policy.mentions = append(policy.mentions, compiled)
	}
	return policy
}

func stringSet(values []string, label string) map[string]struct{} {
	if len(values) > 1_000 {
		panic("whatsapp channel: too many " + label + " values")
	}
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 1_024 || !utf8.ValidString(value) {
			panic("whatsapp channel: " + label + " must be 1-1024 UTF-8 bytes")
		}
		result[value] = struct{}{}
	}
	return result
}

func globRegexp(pattern string) string {
	var value strings.Builder
	value.WriteByte('^')
	for _, current := range pattern {
		switch current {
		case '*':
			value.WriteString(".*")
		case '?':
			value.WriteByte('.')
		default:
			value.WriteString(regexp.QuoteMeta(string(current)))
		}
	}
	value.WriteByte('$')
	return value.String()
}

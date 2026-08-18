// Package telegram adapts Telegram's Bot API to datalon.Channel.
package telegram

import (
	"fmt"
	"regexp"
	"strings"
)

// OpenExposureAcknowledgement is required by OpenExposure. Open exposure lets
// arbitrary Telegram users invoke the operator's agent and tools.
const OpenExposureAcknowledgement = "allow-arbitrary-senders"

type ExposureMode string

const (
	ExposureSelf      ExposureMode = "self"
	ExposureAllowlist ExposureMode = "allowlist"
	ExposureOpen      ExposureMode = "open"
)

var (
	userIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
	chatIDPattern = regexp.MustCompile(`^-?[0-9]{1,20}$`)
	tokenPattern  = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,512}$`)
)

// Exposure is an immutable inbound trigger policy. Its zero value is a safe
// self policy with no operators, so it rejects every inbound message.
type Exposure struct {
	mode        ExposureMode
	operatorIDs map[string]struct{}
	userIDs     map[string]struct{}
	chatIDs     map[string]struct{}
}

// SelfExposure allows messages sent by one of the required operator IDs.
func SelfExposure(operatorIDs ...string) Exposure {
	if len(operatorIDs) == 0 {
		panic("telegram: self exposure requires an operator ID")
	}
	return Exposure{mode: ExposureSelf, operatorIDs: idSet("operator", userIDPattern, operatorIDs)}
}

// AllowlistExposure allows private messages from userIDs and channel posts
// from chatIDs. Empty lists form a useful deny-all policy.
func AllowlistExposure(userIDs, chatIDs []string) Exposure {
	return Exposure{
		mode:    ExposureAllowlist,
		userIDs: idSet("user", userIDPattern, userIDs),
		chatIDs: idSet("chat", chatIDPattern, chatIDs),
	}
}

// OpenExposure allows all private messages and channel posts. The explicit
// acknowledgement is positional so enabling this high-authority mode cannot be
// hidden in an options struct.
func OpenExposure(acknowledgement string) Exposure {
	if acknowledgement != OpenExposureAcknowledgement {
		panic("telegram: open exposure requires explicit acknowledgement")
	}
	return Exposure{mode: ExposureOpen}
}

func (exposure Exposure) Mode() ExposureMode {
	if exposure.mode == "" {
		return ExposureSelf
	}
	return exposure.mode
}

func (exposure Exposure) clone() Exposure {
	return Exposure{
		mode:        exposure.mode,
		operatorIDs: cloneSet(exposure.operatorIDs),
		userIDs:     cloneSet(exposure.userIDs),
		chatIDs:     cloneSet(exposure.chatIDs),
	}
}

func (exposure Exposure) allows(chatType, chatID, senderID string, fromSelf bool) bool {
	switch exposure.Mode() {
	case ExposureOpen:
		return true
	case ExposureAllowlist:
		if chatType == "private" {
			_, ok := exposure.userIDs[senderID]
			return ok
		}
		_, ok := exposure.chatIDs[chatID]
		return ok
	default:
		if fromSelf {
			return true
		}
		_, ok := exposure.operatorIDs[senderID]
		return ok
	}
}

func idSet(label string, pattern *regexp.Regexp, values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != strings.TrimSpace(value) || !pattern.MatchString(value) {
			panic(fmt.Sprintf("telegram: invalid %s ID", label))
		}
		result[value] = struct{}{}
	}
	return result
}

func cloneSet(source map[string]struct{}) map[string]struct{} {
	if source == nil {
		return nil
	}
	result := make(map[string]struct{}, len(source))
	for value := range source {
		result[value] = struct{}{}
	}
	return result
}

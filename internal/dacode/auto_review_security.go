package dacode

import (
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const maxAutoReviewReasonRunes = 512

const (
	maxAutoReviewSecretDepth = 4
	maxAutoReviewSecrets     = 32
	minAutoReviewSecretBytes = 8
	maxApprovalSummaryKeys   = 64
	maxApprovalSummaryItems  = 64
	maxApprovalSummaryRunes  = 1024
)

var (
	autoReviewANSIExpression   = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))`)
	autoReviewURLExpression    = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
	autoReviewSecretExpression = regexp.MustCompile(
		`(?i)\b([A-Z][A-Z0-9_]*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)[A-Z0-9_]*)\s*=\s*([^\s,;]+)`,
	)
	autoReviewSecretKeyExpression  = regexp.MustCompile(`(?i)(?:key|token|secret|password|credential|authorization)`)
	autoReviewPrivateKeyExpression = regexp.MustCompile(`(?is)-----BEGIN [^-\r\n]*PRIVATE KEY-----.*?-----END [^-\r\n]*PRIVATE KEY-----`)
	autoReviewHeaderExpression     = regexp.MustCompile(`(?im)^([ \t]*(?:authorization|proxy-authorization|cookie|set-cookie|x-api-key|api-key)[ \t]*:[ \t]*)[^\r\n]*`)
	autoReviewBearerExpression     = regexp.MustCompile(`(?i)\b(Bearer|Basic)[ \t]+[^\s'";,]+`)
)

// sanitizeAutoReason makes untrusted classifier text safe to persist, render,
// and return to the model. URLs retain only their endpoint shape, credential
// assignments and known values are redacted, controls are removed, and the
// result is a bounded single line.
func sanitizeAutoReason(reason string, knownSecrets []string) string {
	reason = autoReviewANSIExpression.ReplaceAllString(reason, "")
	reason = stripAutoReviewControls(reason)
	reason = autoReviewSecretExpression.ReplaceAllString(reason, "$1=[redacted]")
	reason = autoReviewURLExpression.ReplaceAllStringFunc(reason, redactAutoReviewURL)
	for _, secret := range knownSecrets {
		if secret != "" {
			reason = strings.ReplaceAll(reason, secret, "[redacted]")
		}
	}
	reason = strings.Join(strings.Fields(reason), " ")
	runes := []rune(reason)
	if len(runes) > maxAutoReviewReasonRunes {
		reason = string(runes[:maxAutoReviewReasonRunes])
	}
	if reason == "" {
		return "The action was not authorized by the user request."
	}
	return reason
}

func redactAutoClassifierText(value string, knownSecrets []string) string {
	value = autoReviewANSIExpression.ReplaceAllString(value, "")
	value = autoReviewPrivateKeyExpression.ReplaceAllString(value, "[redacted private key]")
	value = autoReviewHeaderExpression.ReplaceAllString(value, "$1[redacted]")
	value = autoReviewBearerExpression.ReplaceAllString(value, "$1 [redacted]")
	value = autoReviewSecretExpression.ReplaceAllString(value, "$1=[redacted]")
	value = autoReviewURLExpression.ReplaceAllStringFunc(value, redactAutoReviewURL)
	for _, secret := range knownSecrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return stripAutoReviewControls(value)
}

func redactedApprovalArguments(arguments json.RawMessage, limit int) string {
	limit = max(limit, 64)
	if len(arguments) == 0 || len(arguments) > 256<<10 {
		return "[arguments unavailable]"
	}
	secrets := knownApprovalSecrets(arguments)
	var value any
	if json.Unmarshal(arguments, &value) != nil {
		return truncateAutoRunes(redactAutoClassifierText(string(arguments), secrets), limit)
	}
	encoded, err := json.Marshal(redactApprovalValue(value, false, secrets, 0))
	if err != nil {
		return "[arguments unavailable]"
	}
	return truncateAutoRunes(string(encoded), limit)
}

func redactApprovalValue(value any, sensitive bool, secrets []string, depth int) any {
	if depth > maxAutoReviewSecretDepth {
		return "[nested value omitted]"
	}
	switch value := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > maxApprovalSummaryKeys {
			keys = keys[:maxApprovalSummaryKeys]
		}
		result := make(map[string]any, len(keys))
		for _, key := range keys {
			child := value[key]
			key = truncateAutoRunes(stripAutoReviewControls(key), 128)
			childSensitive := sensitive || autoReviewSecretKeyExpression.MatchString(key)
			if childSensitive {
				result[key] = "[redacted]"
			} else {
				result[key] = redactApprovalValue(child, false, secrets, depth+1)
			}
		}
		return result
	case []any:
		limit := min(len(value), maxApprovalSummaryItems)
		result := make([]any, 0, limit)
		for _, child := range value[:limit] {
			result = append(result, redactApprovalValue(child, sensitive, secrets, depth+1))
		}
		return result
	case string:
		if sensitive {
			return "[redacted]"
		}
		return truncateAutoRunes(redactAutoClassifierText(value, secrets), maxApprovalSummaryRunes)
	default:
		return value
	}
}

func truncateAutoRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

// knownApprovalSecrets extracts only values explicitly carried in
// credential-shaped tool arguments. It never scans the process environment,
// and its depth/count bounds keep adversarial JSON cheap.
func knownApprovalSecrets(arguments json.RawMessage) []string {
	if len(arguments) == 0 || len(arguments) > 256<<10 {
		return nil
	}
	var value any
	if json.Unmarshal(arguments, &value) != nil {
		return nil
	}
	secrets := make([]string, 0, 4)
	seen := map[string]struct{}{}
	var visit func(any, bool, int)
	visit = func(current any, sensitive bool, depth int) {
		if depth > maxAutoReviewSecretDepth || len(secrets) == maxAutoReviewSecrets {
			return
		}
		switch current := current.(type) {
		case map[string]any:
			for key, child := range current {
				visit(child, sensitive || autoReviewSecretKeyExpression.MatchString(key), depth+1)
				if len(secrets) == maxAutoReviewSecrets {
					return
				}
			}
		case []any:
			for _, child := range current {
				visit(child, sensitive, depth+1)
				if len(secrets) == maxAutoReviewSecrets {
					return
				}
			}
		case string:
			if !sensitive || len(current) < minAutoReviewSecretBytes {
				return
			}
			if _, exists := seen[current]; exists {
				return
			}
			seen[current] = struct{}{}
			secrets = append(secrets, current)
		}
	}
	visit(value, false, 0)
	sort.Slice(secrets, func(left, right int) bool {
		if len(secrets[left]) == len(secrets[right]) {
			return secrets[left] < secrets[right]
		}
		return len(secrets[left]) > len(secrets[right])
	})
	return secrets
}

func stripAutoReviewControls(value string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return -1
		}
		return character
	}, value)
}

func redactAutoReviewURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[redacted URL]"
	}
	if parsed.User != nil {
		parsed.User = url.User("***")
	}
	query := parsed.Query()
	for key := range query {
		query[key] = []string{"[redacted]"}
	}
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	return parsed.String()
}

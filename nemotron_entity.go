package dago

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
)

var (
	nemotronCurrentIDTool = regexp.MustCompile(`^get_current_([a-z_]+)_id$`)
	nemotronDisplayTool   = regexp.MustCompile(`^get_([a-z_]+)_(name|title)$`)
	nemotronRelationTool  = regexp.MustCompile(`^get_([a-z_]+)_([a-z_]+)$`)
	nemotronVisibleID     = regexp.MustCompile(`\b[0-9]{4,}\b`)
)

type nemotronRelationKey struct {
	Source   string
	SourceID int64
	Target   string
}

type nemotronEntityKey struct {
	Entity string
	ID     int64
}

type nemotronResolution struct {
	Entity   string
	ID       int64
	Source   string
	SourceID int64
	Lookup   bool
}

func nemotronEntityResolutionGuard() dagent.Middleware {
	const preKey = "nemotron_entity_pre_nudged"
	const firedKey = "nemotron_entity_guard_fired"
	return dagent.Middleware{
		Name: "EntityResolutionGuardMiddleware",
		Fields: map[string]dagent.StateField{
			preKey:   {Kind: dagent.FieldLast, Contract: "dago.nemotron.entity.pre.v1", Private: true, Clone: nemotronIdentityClone},
			firedKey: {Kind: dagent.FieldLast, Contract: "dago.nemotron.entity.final.v1", Private: true, Clone: nemotronIdentityClone},
		},
		BeforeModel: func(_ context.Context, values dastate.Values, _ dagent.Runtime) (dastate.Values, error) {
			if stateBool(values, preKey) {
				return nil, nil
			}
			messages, err := policyMessages(values[dagent.MessagesKey])
			if err != nil || len(messages) == 0 || messages[len(messages)-1].Role != damessage.RoleTool {
				return nil, err
			}
			missing := missingNemotronResolutions(messages, nemotronExternalHumanText(messages), "")
			if len(missing) == 0 {
				return nil, nil
			}
			text := "Before answering, resolve each current-entity or ID branch with its own lookup result instead of reusing another branch's entity. Missing resolution(s): " + formatNemotronResolutionLabels(missing) + ". Required next lookup(s): " + formatNemotronResolutionSteps(missing) + "."
			update := nemotronNudge(text, nemotronEntitySource, nil)
			update[preKey] = true
			return update, nil
		},
		AfterAgent: func(_ context.Context, values dastate.Values, _ dagent.Runtime) (dastate.Values, error) {
			messages, err := policyMessages(values[dagent.MessagesKey])
			if err != nil || stateBool(values, firedKey) || nemotronRepairLoopRisk(messages) || !nemotronHasFinalAnswer(messages) {
				return nil, err
			}
			userText := nemotronExternalHumanText(messages[:len(messages)-1])
			finalText := messages[len(messages)-1].TextContent()
			missing := missingNemotronResolutions(messages[:len(messages)-1], userText, finalText)
			if len(missing) == 0 {
				return nil, nil
			}
			text := "Your final answer is using or mixing opaque entity IDs before resolving them to user-facing details. Keep each branch bound to the ID that produced it. Resolve these before answering: " + formatNemotronResolutionLabels(missing) + ". If a matching name/details lookup tool is available, call it now, then answer from that result. Required next lookup(s): " + formatNemotronResolutionSteps(missing) + ". Do not reuse a name or details from a different entity or question branch."
			update := nemotronNudge(text, nemotronEntitySource, dagent.JumpUpdate("model"))
			update[firedKey] = true
			return update, nil
		},
	}
}

func missingNemotronResolutions(messages []damessage.Message, userText, finalText string) []nemotronResolution {
	missing := missingNemotronCurrentBranch(messages, userText, finalText)
	missing = append(missing, missingNemotronDisplay(messages, userText, finalText)...)
	seen := map[nemotronResolution]bool{}
	result := make([]nemotronResolution, 0, len(missing))
	for _, item := range missing {
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

func currentNemotronEntityIDs(messages []damessage.Message) map[string]int64 {
	result := map[string]int64{}
	for _, entry := range nemotronToolResults(messages) {
		match := nemotronCurrentIDTool.FindStringSubmatch(entry.Call.Name)
		if len(match) == 0 {
			continue
		}
		if id, ok := coerceNemotronID(entry.Value); ok {
			result[match[1]] = id
		}
	}
	return result
}

func nemotronRelationBindings(messages []damessage.Message) map[nemotronRelationKey]int64 {
	result := map[nemotronRelationKey]int64{}
	for _, entry := range nemotronToolResults(messages) {
		if nemotronDisplayTool.MatchString(entry.Call.Name) {
			continue
		}
		match := nemotronRelationTool.FindStringSubmatch(entry.Call.Name)
		if len(match) == 0 {
			continue
		}
		arguments := nemotronArguments(entry.Call)
		sourceID, sourceOK := coerceNemotronID(arguments[match[1]+"_id"])
		targetID, targetOK := coerceNemotronID(entry.Value)
		if sourceOK && targetOK {
			result[nemotronRelationKey{Source: match[1], SourceID: sourceID, Target: match[2]}] = targetID
		}
	}
	return result
}

func resolvedNemotronEntities(messages []damessage.Message) map[nemotronEntityKey]bool {
	result := map[nemotronEntityKey]bool{}
	for _, call := range nemotronToolCalls(messages) {
		match := nemotronDisplayTool.FindStringSubmatch(call.Name)
		if len(match) == 0 {
			continue
		}
		if id, ok := coerceNemotronID(nemotronArguments(call)[match[1]+"_id"]); ok {
			result[nemotronEntityKey{Entity: match[1], ID: id}] = true
		}
	}
	return result
}

func missingNemotronCurrentBranch(messages []damessage.Message, userText, finalText string) []nemotronResolution {
	current := currentNemotronEntityIDs(messages)
	if len(current) == 0 {
		return nil
	}
	bindings := nemotronRelationBindings(messages)
	resolved := resolvedNemotronEntities(messages)
	visible := visibleNemotronIDs(finalText)
	userLower := strings.ToLower(userText)
	var missing []nemotronResolution
	for source, sourceID := range current {
		if !strings.Contains(userLower, "current "+source) {
			continue
		}
		var targets []string
		for relation := range bindings {
			if relation.Source == source && strings.Contains(userLower, relation.Target) {
				targets = append(targets, relation.Target)
			}
		}
		sort.Strings(targets)
		for _, target := range targets {
			id, exists := bindings[nemotronRelationKey{Source: source, SourceID: sourceID, Target: target}]
			if !exists {
				missing = append(missing, nemotronResolution{Entity: target, Source: source, SourceID: sourceID, Lookup: true})
			} else if !resolved[nemotronEntityKey{Entity: target, ID: id}] {
				missing = append(missing, nemotronResolution{Entity: target, ID: id, Source: source, SourceID: sourceID})
			}
		}
	}
	for relation, targetID := range bindings {
		if visible[targetID] && !resolved[nemotronEntityKey{Entity: relation.Target, ID: targetID}] {
			missing = append(missing, nemotronResolution{Entity: relation.Target, ID: targetID, Source: relation.Source, SourceID: relation.SourceID})
		}
	}
	return missing
}

func missingNemotronDisplay(messages []damessage.Message, userText, finalText string) []nemotronResolution {
	resolved := resolvedNemotronEntities(messages)
	visible := visibleNemotronIDs(finalText)
	userLower := strings.ToLower(userText)
	var missing []nemotronResolution
	for _, call := range nemotronToolCalls(messages) {
		if nemotronDisplayTool.MatchString(call.Name) {
			continue
		}
		match := nemotronRelationTool.FindStringSubmatch(call.Name)
		if len(match) == 0 {
			continue
		}
		entity := match[1]
		asksEntity := regexp.MustCompile(`(?i)\b(which|what) +(\w+ +){0,4}` + regexp.QuoteMeta(entity) + `\b`).MatchString(userText)
		referencesEntity := strings.Contains(userLower, "that "+entity) || strings.Contains(userLower, entity+" with") || strings.Contains(userLower, entity+" whose") || strings.Contains(userLower, "selected "+entity)
		if !asksEntity && !referencesEntity {
			continue
		}
		id, ok := coerceNemotronID(nemotronArguments(call)[entity+"_id"])
		if ok && (visible[id] || referencesEntity) && !resolved[nemotronEntityKey{Entity: entity, ID: id}] {
			missing = append(missing, nemotronResolution{Entity: entity, ID: id})
		}
	}
	return missing
}

func nemotronArguments(call damessage.ToolCall) map[string]any {
	var result map[string]any
	_ = json.Unmarshal(call.Arguments, &result)
	return result
}

func coerceNemotronID(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		integer := int64(typed)
		return integer, float64(integer) == typed
	case json.Number:
		integer, err := typed.Int64()
		return integer, err == nil
	case string:
		integer, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return integer, err == nil
	default:
		return 0, false
	}
}

func visibleNemotronIDs(text string) map[int64]bool {
	result := map[int64]bool{}
	for _, value := range nemotronVisibleID.FindAllString(text, -1) {
		if id, err := strconv.ParseInt(value, 10, 64); err == nil {
			result[id] = true
		}
	}
	return result
}

func formatNemotronResolutionLabels(values []nemotronResolution) string {
	labels := make([]string, 0, len(values))
	for _, value := range values {
		switch {
		case value.Lookup:
			labels = append(labels, fmt.Sprintf("%s lookup for current %s %d", value.Entity, value.Source, value.SourceID))
		case value.Source != "":
			labels = append(labels, fmt.Sprintf("%s_id %d from current %s %d", value.Entity, value.ID, value.Source, value.SourceID))
		default:
			labels = append(labels, fmt.Sprintf("%s_id %d title/name", value.Entity, value.ID))
		}
	}
	return strings.Join(labels, ", ")
}

func formatNemotronResolutionSteps(values []nemotronResolution) string {
	seen := map[string]bool{}
	var steps []string
	for _, value := range values {
		step := ""
		if value.Lookup {
			step = fmt.Sprintf("call get_%s_%s with %s_id=%d", value.Source, value.Entity, value.Source, value.SourceID)
		} else {
			step = fmt.Sprintf("call get_%s_title or get_%s_name with %s_id=%d, whichever tool exists", value.Entity, value.Entity, value.Entity, value.ID)
		}
		if !seen[step] {
			seen[step] = true
			steps = append(steps, step)
		}
	}
	return strings.Join(steps, "; ")
}

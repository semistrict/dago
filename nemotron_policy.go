package dago

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
)

const (
	nemotronBudgetSource       = "nemotron_progress_budget"
	nemotronFinalGuardSource   = "nemotron_final_answer_guard"
	nemotronTransitionSource   = "nemotron_transition_nudge"
	nemotronFollowupSource     = "nemotron_followup_guard"
	nemotronEntitySource       = "nemotron_entity_guard"
	nemotronActionSource       = "nemotron_action_commit_nudge"
	nemotronToolChainSource    = "nemotron_tool_chain_nudge"
	nemotronFilesystemSource   = "nemotron_filesystem_request_nudge"
	nemotronDomainPreferSource = "nemotron_domain_tool_preference"
	nemotronDomainNudgeSource  = "nemotron_domain_tool_nudge"
)

const (
	nemotronMaxRepairModelCalls  = 8
	nemotronMaxRepairToolResults = 28
	nemotronMaxBudgetResults     = 12
	nemotronMaxBudgetResultChars = 500
)

var nemotronInternalMessageNames = map[string]bool{
	nemotronBudgetSource: true, nemotronFinalGuardSource: true,
	nemotronTransitionSource: true, nemotronFollowupSource: true,
	nemotronEntitySource: true, nemotronActionSource: true,
	nemotronToolChainSource: true, nemotronFilesystemSource: true,
	nemotronDomainPreferSource: true, nemotronDomainNudgeSource: true,
}

var nemotronSpace = regexp.MustCompile(`\s+`)

var (
	nemotronFileTask          = regexp.MustCompile(`(?i)\b(file|files|folder|folders|directory|directories|path|paths|read_file|write_file|edit_file|grep|glob|ls|filesystem|codebase|source code)\b`)
	nemotronFilesystemRequest = regexp.MustCompile(`(?i)\b(read|open|inspect|review|summarize|summarise|analyze|analyse|process|edit)\b.{0,160}\b(file|files|document|transcript|log|repository|repo|codebase|source|/[A-Za-z0-9_./-]+|[A-Za-z0-9_.-]+\.[A-Za-z0-9]{1,8})\b`)
	nemotronNewTask           = regexp.MustCompile(`(?i)\b(move on|switching to|switch to|new task|different task|unrelated task|separate task|new topic|different topic|unrelated topic)\b`)
	nemotronLargeRead         = regexp.MustCompile(`(?i)\b(read|summarize|summarise|inspect|analyze|analyse|review|process)\b.{0,120}\b(file|document|transcript|log|repository|codebase|source|/[A-Za-z0-9_./-]+|[A-Za-z0-9_.-]+\.[A-Za-z0-9]{1,8})\b`)
	nemotronFileReference     = regexp.MustCompile(`(/[A-Za-z0-9_./-]+|\b[A-Za-z0-9_.-]+\.[A-Za-z0-9]{1,8}\b)`)
	nemotronFollowOnFile      = regexp.MustCompile(`(?i)\b(do the same|same thing|another|next|also|continue|again)\b.{0,120}\b(file|document|transcript|log|repository|repo|codebase|source|/[A-Za-z0-9_./-]+|[A-Za-z0-9_.-]+\.[A-Za-z0-9]{1,8})\b`)
	nemotronActionRequest     = regexp.MustCompile(`(?i)\b(proceed|go ahead|make it happen|do it|now|please (cancel|book|update|upgrade|send|display|show|retrieve|start|fill|lock|charge)|i want to (cancel|book|update|upgrade|send)|can you please (cancel|book|update|upgrade|send))\b`)
	nemotronChainedAction     = regexp.MustCompile(`(?i)\b(then|and|after|afterward|after that|once)\b.{0,160}\b(email|send|notify|post|message|dm|create|schedule|book|cancel|update)\b`)
	nemotronRecurrence        = regexp.MustCompile(`(?i)\b(daily|weekly|monthly|nightly|morning|evening|every (day|week|month|morning|night)|each (day|week|month)|at [0-9]{1,2}(:[0-9]{2})? *(am|pm)?)\b`)
	nemotronScheduleQuestion  = regexp.MustCompile(`(?i)\b(day/time|timezone|time zone|cadence|frequency|schedule|what time|which day|what day|how often|when should|when do you)\b`)
	nemotronSourceQuestion    = regexp.MustCompile(`(?i)\b(which|what) +(data source|source|sources|scope|folder|folders|inbox|inboxes|labels|senders|project|projects|repository|repositories|system|systems|service|services)\b`)
	nemotronSourceSupplied    = regexp.MustCompile(`(?i)\b(my|our|the|this|these|current|all) +(sources|source|folders|folder|inboxes|inbox|labels|senders|projects|project|repositories|repository|repos|systems|services|workspaces|accounts)\b|\b(from|in|under|inside|within) +(/[A-Za-z0-9_./-]+|[A-Za-z0-9_.-]+\.[A-Za-z0-9]{1,8})\b`)
	nemotronAnalysisRequest   = regexp.MustCompile(`(?i)\b(analyze|analyse|analysis|insight|report|dashboard)\b`)
	nemotronAnalysisGoal      = regexp.MustCompile(`(?i)\b(goal|objective|question|metric|measure|compare|trend|segment|outcome|trying to learn)\b`)
	nemotronSupportRequest    = regexp.MustCompile(`(?i)\b(customer|support|ticket|question|respond|response)\b`)
	nemotronSupportDomain     = regexp.MustCompile(`(?i)\b(domain|product|service|business|industry|customer|customers|user|users)\b`)
	nemotronDeliveryContext   = regexp.MustCompile(`(?i)\b(brief|summary|summaries|report|digest|recurring|daily|weekly|calendar|monitoring)\b`)
	nemotronDeliveryQuestion  = regexp.MustCompile(`(?i)\b(how|where|which|what)\b.{0,80}\b(receive|send|deliver|delivery|channel|email|slack|sms|notify|notification)\b`)
	nemotronQuestionStart     = regexp.MustCompile(`(?im)^\s*[-*]?\s*(what|which|how|where|when|who)\b`)
	nemotronVagueCompletion   = regexp.MustCompile(`(?i)^\s*(done|completed|all set|handled|taken care of|finished)[.!]*\s*$`)
	nemotronExactSingleWord   = regexp.MustCompile(`(?i)\b(reply|respond|return|answer) +with +(the +)?(single +word|one +word) +([A-Za-z0-9_\-\[\]{}]+)\b`)
	nemotronExactPhrase       = regexp.MustCompile(`(?i)\b(reply|respond|return|answer) +with +exactly *:? *["']?([^"'.?!\n]{1,80})`)
	nemotronExactOnly         = regexp.MustCompile(`(?i)\b(reply|respond|return|answer) +with +([A-Za-z0-9_\-\[\]{}]+) +only\b`)
	nemotronVersionLiteral    = regexp.MustCompile(`\bv[0-9]+(\.[0-9]+)+[-._A-Za-z0-9]*\b`)
	nemotronToolToken         = regexp.MustCompile(`[A-Z]?[a-z]+|[A-Z]+|[0-9]+`)
)

var nemotronMutationVerbs = map[string]bool{
	"approve": true, "archive": true, "assign": true, "activate": true,
	"book": true, "cancel": true, "charge": true, "close": true,
	"create": true, "deactivate": true, "delete": true, "disable": true,
	"enable": true, "escalate": true, "grant": true, "invite": true,
	"notify": true, "pay": true, "post": true, "publish": true,
	"reject": true, "refund": true, "remove": true, "reserve": true,
	"revoke": true, "schedule": true, "send": true, "submit": true,
	"terminate": true, "transfer": true, "update": true, "upgrade": true,
	"write": true,
}

var nemotronReadOnlyPrefixes = map[string]bool{
	"count": true, "describe": true, "fetch": true, "find": true,
	"get": true, "list": true, "lookup": true, "read": true,
	"retrieve": true, "search": true,
}

var nemotronBuiltinTools = map[string]bool{
	"ls": true, "read_file": true, "write_file": true, "edit_file": true,
	"delete": true, "glob": true, "grep": true, "compact_conversation": true,
	"execute": true, "task": true, "write_todos": true,
}

type nemotronToolResult struct {
	Call  damessage.ToolCall
	Value any
}

// NemotronProgressBudgetOptions bounds one active user turn.
type NemotronProgressBudgetOptions struct {
	MaxModelCalls        int
	MaxToolResults       int
	MaxRepeatedToolCalls int
}

// NemotronProgressBudget stops runaway loops before another model call and
// returns a compact answer grounded in results already gathered this turn.
func NemotronProgressBudget(options NemotronProgressBudgetOptions) dagent.Middleware {
	if options.MaxModelCalls <= 0 {
		options.MaxModelCalls = 16
	}
	if options.MaxToolResults <= 0 {
		options.MaxToolResults = 48
	}
	if options.MaxRepeatedToolCalls <= 0 {
		options.MaxRepeatedToolCalls = 3
	}
	return dagent.Middleware{Name: "NemotronProgressBudgetMiddleware", WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
		messages := nemotronMessagesSinceLastUser(nemotronRequestMessages(request))
		reason := ""
		if count := countRole(messages, damessage.RoleAssistant); count >= options.MaxModelCalls {
			reason = fmt.Sprintf("%d model turns", count)
		} else if count := countRole(messages, damessage.RoleTool); count >= options.MaxToolResults {
			reason = fmt.Sprintf("%d tool results", count)
		} else if count := maxRepeatedNemotronCalls(messages); count >= options.MaxRepeatedToolCalls {
			reason = fmt.Sprintf("%d repeated identical tool calls", count)
		}
		if reason == "" {
			return next(ctx, request)
		}
		fallback := damessage.Assistant(nemotronBudgetFallback(messages, reason))
		fallback.Name = nemotronBudgetSource
		fallback.ResponseMetadata = map[string]json.RawMessage{"nemotron_progress_budget_reason": mustRawJSON(reason)}
		return dagent.ModelResponse{Messages: []damessage.Message{fallback}}, nil
	}}
}

func nemotronRequestMessages(request dagent.ModelRequest) []damessage.Message {
	if values, err := policyMessages(request.State[dagent.MessagesKey]); err == nil && values != nil {
		return values
	}
	return clonePolicyMessages(request.Messages)
}

func policyMessages(value any) ([]damessage.Message, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case []damessage.Message:
		return clonePolicyMessages(typed), nil
	case dastate.Overwrite:
		return policyMessages(typed.Value)
	default:
		return nil, fmt.Errorf("messages have type %T", value)
	}
}

func clonePolicyMessages(values []damessage.Message) []damessage.Message {
	result := make([]damessage.Message, len(values))
	for index := range values {
		result[index] = values[index].Clone()
	}
	return result
}

func nemotronMessagesSinceLastUser(messages []damessage.Message) []damessage.Message {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == damessage.RoleHuman && !nemotronInternalMessageNames[messages[index].Name] {
			return messages[index:]
		}
	}
	return messages
}

func countRole(messages []damessage.Message, role damessage.Role) int {
	count := 0
	for _, value := range messages {
		if value.Role == role {
			count++
		}
	}
	return count
}

func maxRepeatedNemotronCalls(messages []damessage.Message) int {
	maximum, current, previous := 0, 0, ""
	for _, call := range nemotronToolCalls(messages) {
		signature := call.Name + ":" + canonicalNemotronArguments(call.Arguments)
		if signature == previous {
			current++
		} else {
			previous, current = signature, 1
		}
		if current > maximum {
			maximum = current
		}
	}
	return maximum
}

func canonicalNemotronArguments(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return string(raw)
	}
	return string(encoded)
}

func nemotronToolCalls(messages []damessage.Message) []damessage.ToolCall {
	var result []damessage.ToolCall
	for _, value := range messages {
		if value.Role == damessage.RoleAssistant {
			result = append(result, value.ToolCalls...)
		}
	}
	return result
}

func nemotronToolResults(messages []damessage.Message) []nemotronToolResult {
	calls := map[string]damessage.ToolCall{}
	for _, call := range nemotronToolCalls(messages) {
		if call.ID != "" {
			calls[call.ID] = call
		}
	}
	var results []nemotronToolResult
	for _, value := range messages {
		if value.Role != damessage.RoleTool {
			continue
		}
		call, exists := calls[value.ToolCallID]
		if !exists {
			continue
		}
		results = append(results, nemotronToolResult{Call: call, Value: parseNemotronToolValue(value.TextContent())})
	}
	return results
}

func parseNemotronToolValue(text string) any {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var value any
	if json.Unmarshal([]byte(text), &value) == nil {
		return value
	}
	return text
}

func formatNemotronBudgetValue(value any) string {
	text := ""
	switch typed := value.(type) {
	case nil:
	case string:
		text = typed
	default:
		if encoded, err := json.Marshal(typed); err == nil {
			text = string(encoded)
		} else {
			text = fmt.Sprint(typed)
		}
	}
	text = strings.TrimSpace(nemotronSpace.ReplaceAllString(text, " "))
	if len(text) > nemotronMaxBudgetResultChars {
		text = strings.TrimRight(text[:nemotronMaxBudgetResultChars-3], " ") + "..."
	}
	return text
}

func nemotronBudgetFallback(messages []damessage.Message, reason string) string {
	results := nemotronToolResults(messages)
	if len(results) == 0 {
		return fmt.Sprintf("I could not complete this reliably within the harness step budget (%s).", reason)
	}
	prioritized := make([]nemotronToolResult, 0, len(results))
	for _, result := range results {
		if nemotronBudgetResultInformative(result.Value) {
			prioritized = append(prioritized, result)
		}
	}
	if len(prioritized) < nemotronMaxBudgetResults {
		for _, result := range results {
			if !containsNemotronResult(prioritized, result) {
				prioritized = append(prioritized, result)
			}
		}
	}
	if len(prioritized) > nemotronMaxBudgetResults {
		prioritized = prioritized[:nemotronMaxBudgetResults]
	}
	seen := map[string]bool{}
	var rows []string
	for _, result := range prioritized {
		name := result.Call.Name
		if name == "" {
			name = "tool"
		}
		text := formatNemotronBudgetValue(result.Value)
		key := name + "\x00" + text
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, "- "+name+": "+text)
	}
	return "Using the tool results gathered so far:\n" + strings.Join(rows, "\n")
}

func nemotronBudgetResultInformative(value any) bool {
	text := strings.ToLower(formatNemotronBudgetValue(value))
	if text == "" {
		return false
	}
	for _, prefix := range []string{"no files found", "no matches found", "error:"} {
		if strings.HasPrefix(text, prefix) {
			return false
		}
	}
	return true
}

func containsNemotronResult(values []nemotronToolResult, target nemotronToolResult) bool {
	for _, value := range values {
		if value.Call.ID == target.Call.ID && reflectNemotronValue(value.Value, target.Value) {
			return true
		}
	}
	return false
}

func reflectNemotronValue(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func mustRawJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func nemotronRepairLoopRisk(messages []damessage.Message) bool {
	return countRole(messages, damessage.RoleAssistant) >= nemotronMaxRepairModelCalls || countRole(messages, damessage.RoleTool) >= nemotronMaxRepairToolResults
}

func nemotronExternalHumanText(messages []damessage.Message) string {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == damessage.RoleHuman && !nemotronInternalMessageNames[messages[index].Name] {
			return messages[index].TextContent()
		}
	}
	return ""
}

func nemotronNudge(text, source string, updates dastate.Values) dastate.Values {
	result := updates.Clone()
	if result == nil {
		result = dastate.Values{}
	}
	nudge := damessage.Human(text)
	nudge.Name = source
	result[dagent.MessagesKey] = []damessage.Message{nudge}
	return result
}

func sortedNemotronToolNames(request dagent.ModelRequest) []string {
	result := make([]string, 0, len(request.Tools))
	for _, executable := range request.Tools {
		result = append(result, executable.Definition().Name)
	}
	sort.Strings(result)
	return result
}

func nemotronToolIsDomain(name string) bool {
	return name != "" && !nemotronBuiltinTools[name] && !strings.HasPrefix(name, "__")
}

func nemotronToolIsMutation(name string) bool {
	if !nemotronToolIsDomain(name) {
		return false
	}
	tokens := nemotronToolToken.FindAllString(strings.ReplaceAll(name, "_", " "), -1)
	if len(tokens) == 0 || nemotronReadOnlyPrefixes[strings.ToLower(tokens[0])] {
		return false
	}
	for _, token := range tokens {
		if nemotronMutationVerbs[strings.ToLower(token)] {
			return true
		}
	}
	return false
}

func nemotronToolCallForMessage(messages []damessage.Message, result damessage.Message) *damessage.ToolCall {
	calls := nemotronToolCalls(messages)
	for index := len(calls) - 1; index >= 0; index-- {
		if calls[index].ID == result.ToolCallID {
			call := calls[index]
			return &call
		}
	}
	return nil
}

func nemotronPolicyNudgeMiddleware() dagent.Middleware {
	return dagent.Middleware{
		Name: "NemotronPolicyNudgeMiddleware",
		Fields: map[string]dagent.StateField{
			"nemotron_transition_nudged":  {Kind: dagent.FieldLast, Contract: "dago.nemotron.transition.v1", Private: true, Clone: nemotronIdentityClone},
			"nemotron_action_nudged":      {Kind: dagent.FieldLast, Contract: "dago.nemotron.action.v1", Private: true, Clone: cloneStringSliceValue},
			"nemotron_tool_chain_nudged":  {Kind: dagent.FieldLast, Contract: "dago.nemotron.chain.v1", Private: true, Clone: nemotronIdentityClone},
			"nemotron_domain_tool_nudged": {Kind: dagent.FieldLast, Contract: "dago.nemotron.domain.v1", Private: true, Clone: nemotronIdentityClone},
		},
		WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
			window := nemotronMessagesSinceLastUser(nemotronRequestMessages(request))
			if len(window) == 1 && window[0].Role == damessage.RoleHuman {
				userText := window[0].TextContent()
				names := sortedNemotronToolNames(request)
				hasFilesystem, hasDomain := false, false
				for _, name := range names {
					hasFilesystem = hasFilesystem || nemotronFilesystemTools[name]
					hasDomain = hasDomain || nemotronToolIsDomain(name)
				}
				if hasFilesystem && hasDomain && !nemotronFileTask.MatchString(userText) {
					var domain []string
					for _, name := range names {
						if nemotronToolIsDomain(name) {
							domain = append(domain, name)
						}
					}
					if len(domain) > 12 {
						domain = domain[:12]
					}
					nudge := damessage.Human("This is not a file or repository-content request. Start with the task-specific non-filesystem tools instead of ls, glob, grep, or read_file. For ranking, counting, 'which', or 'most' questions, enumerate or search for candidate entities with the available domain tools, fetch the relevant details or counts, compare those observed results, and then answer. Relevant task tools include: " + strings.Join(domain, ", ") + ".")
					nudge.Name = nemotronDomainPreferSource
					request.Messages = append(request.Messages, nudge)
				}
				if hasFilesystem && nemotronFilesystemRequest.MatchString(userText) {
					nudge := damessage.Human("The user is asking for file or path content, and filesystem tools are available. Do not answer that you lack access before trying the tools. If the user named a file or path, first call read_file with that path and the requested pagination/limit. If that fails or the location is ambiguous, use ls or glob to locate the file, then continue reading until the request is satisfied.")
					nudge.Name = nemotronFilesystemSource
					request.Messages = append(request.Messages, nudge)
				}
			}
			return next(ctx, request)
		},
		BeforeModel: func(_ context.Context, values dastate.Values, _ dagent.Runtime) (dastate.Values, error) {
			messages, err := policyMessages(values[dagent.MessagesKey])
			if err != nil {
				return nil, err
			}
			var nudges []damessage.Message
			update := dastate.Values{}
			appendNudge := func(text, source string) {
				nudge := damessage.Human(text)
				nudge.Name = source
				nudges = append(nudges, nudge)
			}
			if !stateBool(values, "nemotron_transition_nudged") && shouldNemotronCompactTransition(messages) {
				appendNudge("This is a long conversation and the latest user request appears to start a new task or substantial follow-on file work. If compact_conversation is available, call it before starting the new work so prior context is compressed instead of carried forward verbatim.", nemotronTransitionSource)
				update["nemotron_transition_nudged"] = true
			}
			if text, key, ok := nemotronActionNudge(messages, stringSliceState(values, "nemotron_action_nudged")); ok {
				appendNudge(text, nemotronActionSource)
				update["nemotron_action_nudged"] = append(stringSliceState(values, "nemotron_action_nudged"), key)
			}
			if !stateBool(values, "nemotron_tool_chain_nudged") && shouldNemotronNudgeToolChain(messages) {
				appendNudge("The user's request has a chained action after the information lookup. Use the tool results already gathered as the summary source, then call the requested state-changing tool such as email, send, notify, post, create, schedule, book, cancel, or update. Do not repeat the same lookup unless a required argument for the action is still missing.", nemotronToolChainSource)
				update["nemotron_tool_chain_nudged"] = true
			}
			if !stateBool(values, "nemotron_domain_tool_nudged") && shouldNemotronNudgeDomainCompletion(messages) {
				appendNudge("The filesystem search did not find useful files. Continue with the available non-filesystem API/domain tools instead of grepping or listing more files. For lookup, ranking, counting, or 'most' questions, enumerate or search for candidate entities with domain tools, fetch details or counts with the matching domain tools, compare the observed results, and answer from those results.", nemotronDomainNudgeSource)
				update["nemotron_domain_tool_nudged"] = true
			}
			if len(nudges) > 0 {
				update[dagent.MessagesKey] = nudges
			}
			return update, nil
		},
	}
}

func stateBool(values dastate.Values, key string) bool {
	value, _ := values[key].(bool)
	return value
}

func stringSliceState(values dastate.Values, key string) []string {
	return append([]string(nil), stringsFromState(values[key])...)
}

func cloneStringSliceValue(value any) any {
	return append([]string(nil), stringsFromState(value)...)
}

func nemotronIdentityClone(value any) any { return value }

func shouldNemotronCompactTransition(messages []damessage.Message) bool {
	if len(messages) < 6 {
		return false
	}
	userText := nemotronExternalHumanText(messages)
	window := nemotronMessagesSinceLastUser(messages)
	for _, call := range nemotronToolCalls(window) {
		if call.Name == "compact_conversation" {
			return false
		}
	}
	hasPriorFileWork := false
	for _, call := range nemotronToolCalls(messages[:len(messages)-1]) {
		hasPriorFileWork = hasPriorFileWork || nemotronFilesystemTools[call.Name]
	}
	return nemotronNewTask.MatchString(userText) || (hasPriorFileWork && (nemotronFollowOnFile.MatchString(userText) || nemotronLargeRead.MatchString(userText) || nemotronFileReference.MatchString(userText)))
}

func nemotronActionNudge(messages []damessage.Message, nudged []string) (string, string, bool) {
	if len(messages) == 0 || messages[len(messages)-1].Role != damessage.RoleHuman || nemotronInternalMessageNames[messages[len(messages)-1].Name] {
		return "", "", false
	}
	text := messages[len(messages)-1].TextContent()
	if !nemotronActionRequest.MatchString(text) || len(nemotronToolCalls(messages[:len(messages)-1])) == 0 {
		return "", "", false
	}
	key := fmt.Sprintf("%d:%s", len(messages), truncateNemotronText(text, 200))
	for _, existing := range nudged {
		if existing == key {
			return "", "", false
		}
	}
	return "The user is asking you to perform an action now. If the conversation or previous tool results already provide the required identifiers, payment/source details, recipients, or parameters, call the relevant state-changing/API tool instead of replying only with policy explanation or another confirmation request. Ask exactly one missing-field question only if a required argument is still unavailable.", key, true
}

func truncateNemotronText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func shouldNemotronNudgeToolChain(messages []damessage.Message) bool {
	if len(messages) == 0 || messages[len(messages)-1].Role != damessage.RoleTool {
		return false
	}
	window := nemotronMessagesSinceLastUser(messages)
	if !nemotronChainedAction.MatchString(nemotronExternalHumanText(window)) {
		return false
	}
	calls := nemotronToolCalls(window)
	if len(calls) == 0 {
		return false
	}
	for _, call := range calls {
		if nemotronToolIsMutation(call.Name) {
			return false
		}
	}
	return true
}

func shouldNemotronNudgeDomainCompletion(messages []damessage.Message) bool {
	if len(messages) == 0 || messages[len(messages)-1].Role != damessage.RoleTool {
		return false
	}
	last := messages[len(messages)-1]
	if nemotronBudgetResultInformative(last.TextContent()) {
		return false
	}
	window := nemotronMessagesSinceLastUser(messages)
	call := nemotronToolCallForMessage(window, last)
	if call == nil || !nemotronFilesystemTools[call.Name] {
		return false
	}
	for _, candidate := range nemotronToolCalls(window) {
		if nemotronToolIsDomain(candidate.Name) {
			return true
		}
	}
	return false
}

func nemotronFollowupDiscipline() dagent.Middleware {
	const firedKey = "nemotron_followup_guard_fired"
	return dagent.Middleware{
		Name:   "FollowupDisciplineMiddleware",
		Fields: map[string]dagent.StateField{firedKey: {Kind: dagent.FieldLast, Contract: "dago.nemotron.followup.v1", Private: true, Clone: nemotronIdentityClone}},
		AfterAgent: func(_ context.Context, values dastate.Values, _ dagent.Runtime) (dastate.Values, error) {
			messages, err := policyMessages(values[dagent.MessagesKey])
			if err != nil || stateBool(values, firedKey) || nemotronRepairLoopRisk(messages) || !nemotronHasFinalAnswer(messages) {
				return nil, err
			}
			userText := nemotronExternalHumanText(messages[:len(messages)-1])
			finalText := messages[len(messages)-1].TextContent()
			if nemotronSatisfiesExactRequest(userText, finalText) || !nemotronFollowupNeedsRewrite(userText, finalText) {
				return nil, nil
			}
			update := nemotronNudge("Rewrite your follow-up so it asks for the smallest useful missing information. Do not re-ask about schedule, cadence, source, or scope when those are already supplied. For vague analysis requests, ask for both the data source and the analysis goal. For support or customer-response improvement requests, ask about the product/domain and the current support surface. For recurring briefs, reports, or monitoring requests with a stated cadence, ask for the missing delivery channel or content/source detail, not the day/time again.", nemotronFollowupSource, dagent.JumpUpdate("model"))
			update[firedKey] = true
			return update, nil
		},
	}
}

func nemotronHasFinalAnswer(messages []damessage.Message) bool {
	if len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	return last.Role == damessage.RoleAssistant && last.Name != nemotronBudgetSource && len(last.ToolCalls) == 0 && strings.TrimSpace(last.TextContent()) != ""
}

func nemotronFollowupNeedsRewrite(userText, finalText string) bool {
	hasRecurrence := nemotronRecurrence.MatchString(userText)
	asksSchedule := nemotronScheduleQuestion.MatchString(finalText)
	asksTooMany := hasRecurrence && nemotronQuestionCount(finalText) > 2
	asksSource := nemotronSourceQuestion.MatchString(finalText)
	sourceSupplied := nemotronSourceSupplied.MatchString(userText)
	analysisNeedsGoal := nemotronAnalysisRequest.MatchString(userText) && strings.Contains(strings.ToLower(userText), "data") && !nemotronAnalysisGoal.MatchString(finalText)
	supportNeedsDomain := nemotronSupportRequest.MatchString(userText) && !nemotronSupportDomain.MatchString(finalText)
	deliveryContext := nemotronDeliveryContext.MatchString(userText)
	asksDeliveryOrSource := nemotronDeliveryQuestion.MatchString(finalText) || asksSource
	return (hasRecurrence && asksSchedule) || asksTooMany || (asksSource && sourceSupplied) || analysisNeedsGoal || supportNeedsDomain || (deliveryContext && hasRecurrence && !asksDeliveryOrSource)
}

func nemotronQuestionCount(text string) int {
	return max(strings.Count(text, "?"), len(nemotronQuestionStart.FindAllString(text, -1)))
}

func nemotronSatisfiesExactRequest(userText, finalText string) bool {
	var requested []string
	for _, match := range nemotronExactSingleWord.FindAllStringSubmatch(userText, -1) {
		requested = append(requested, normalizeNemotronExact(match[4]))
	}
	for _, match := range nemotronExactPhrase.FindAllStringSubmatch(userText, -1) {
		requested = append(requested, normalizeNemotronExact(match[2]))
	}
	for _, match := range nemotronExactOnly.FindAllStringSubmatch(userText, -1) {
		requested = append(requested, normalizeNemotronExact(match[2]))
	}
	normalized := normalizeNemotronExact(finalText)
	for _, expected := range requested {
		if expected != "" && normalized == expected {
			return true
		}
	}
	return false
}

func normalizeNemotronExact(value string) string {
	return strings.TrimSpace(strings.Trim(nemotronSpace.ReplaceAllString(strings.TrimSpace(value), " "), "\"'` .!?"))
}

func nemotronFinalAnswerGuard() dagent.Middleware {
	const firedKey = "nemotron_final_guard_fired"
	return dagent.Middleware{
		Name:   "FinalAnswerGuardMiddleware",
		Fields: map[string]dagent.StateField{firedKey: {Kind: dagent.FieldLast, Contract: "dago.nemotron.final.v1", Private: true, Clone: nemotronIdentityClone}},
		AfterAgent: func(_ context.Context, values dastate.Values, _ dagent.Runtime) (dastate.Values, error) {
			messages, err := policyMessages(values[dagent.MessagesKey])
			if err != nil || stateBool(values, firedKey) || nemotronRepairLoopRisk(messages) || !nemotronHasFinalAnswer(messages) {
				return nil, err
			}
			finalText := messages[len(messages)-1].TextContent()
			userText := nemotronExternalHumanText(messages[:len(messages)-1])
			if nemotronSatisfiesExactRequest(userText, finalText) {
				return nil, nil
			}
			missing := nemotronMissingMutationLiterals(nemotronToolCalls(messages[:len(messages)-1]), strings.ToLower(finalText))
			text := ""
			if len(missing) > 0 {
				text = "Your final answer omitted exact literal value(s) from the completed tool action: " + strings.Join(missing, ", ") + ". Answer again and include each literal exactly, along with the concrete result."
			} else if result, ok := lastNemotronMutationResult(messages[:len(messages)-1]); ok && nemotronVagueCompletion.MatchString(finalText) {
				text = fmt.Sprintf("Your final answer should communicate the concrete outcome of the completed state-changing tool call. Latest mutation tool: %s. Observed result: %s. Answer again from that result, including what changed and any important status, amount, date/time, identifier, or remaining caveat present in the tool result.", result.Call.Name, formatNemotronBudgetValue(result.Value))
			}
			if text == "" {
				return nil, nil
			}
			update := nemotronNudge(text, nemotronFinalGuardSource, dagent.JumpUpdate("model"))
			update[firedKey] = true
			return update, nil
		},
	}
}

func nemotronMissingMutationLiterals(calls []damessage.ToolCall, finalLower string) []string {
	seen := map[string]bool{}
	var result []string
	for _, call := range calls {
		if !nemotronToolIsMutation(call.Name) {
			continue
		}
		var arguments map[string]any
		if json.Unmarshal(call.Arguments, &arguments) != nil {
			continue
		}
		var literals []string
		for _, value := range arguments {
			if text, ok := value.(string); ok {
				literals = append(literals, nemotronVersionLiteral.FindAllString(text, -1)...)
			}
		}
		for _, key := range []string{"title", "subject"} {
			if text, ok := arguments[key].(string); ok && len(text) >= 3 && len(text) <= 80 {
				literals = append(literals, text)
			}
		}
		for _, literal := range literals {
			if !seen[literal] && !strings.Contains(finalLower, strings.ToLower(literal)) {
				seen[literal] = true
				result = append(result, literal)
			}
		}
	}
	return result
}

func lastNemotronMutationResult(messages []damessage.Message) (nemotronToolResult, bool) {
	results := nemotronToolResults(messages)
	for index := len(results) - 1; index >= 0; index-- {
		if nemotronToolIsMutation(results[index].Call.Name) {
			return results[index], true
		}
	}
	return nemotronToolResult{}, false
}

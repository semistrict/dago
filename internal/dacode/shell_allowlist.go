package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

// recommendedShellCommands contains commands whose ordinary operation is
// read-only. Commands that can interpret code, spawn commands, access the
// network, or write files are deliberately absent.
var recommendedShellCommands = []string{
	"ls", "dir",
	"cat", "head", "tail",
	"grep", "wc", "strings",
	"cut", "tr", "diff", "md5sum", "sha256sum",
	"pwd", "which",
	"uname", "hostname", "whoami", "id", "groups", "uptime", "nproc", "lscpu", "lsmem",
	"ps",
}

type shellAllowList struct {
	commands     []string
	unrestricted bool
}

var errShellCommandRejected = errors.New("shell command rejected by allow-list")

// parseShellAllowList parses external CLI configuration. The zero value is a
// useful fail-closed policy: it is unconfigured and allows no command.
func parseShellAllowList(value string) (shellAllowList, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return shellAllowList{}, nil
	}
	if strings.EqualFold(value, "all") {
		return shellAllowList{unrestricted: true}, nil
	}

	var commands []string
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		command := strings.TrimSpace(part)
		if command == "" {
			continue
		}
		if strings.EqualFold(command, "all") {
			return shellAllowList{}, fmt.Errorf("cannot combine %q with other commands in --shell-allow-list", "all")
		}
		if strings.EqualFold(command, "recommended") {
			for _, recommended := range recommendedShellCommands {
				if !seen[recommended] {
					seen[recommended] = true
					commands = append(commands, recommended)
				}
			}
			continue
		}
		if strings.ContainsAny(command, " \t\r\n;&|<>`$\\\"'") {
			return shellAllowList{}, fmt.Errorf("shell allow-list entry %q must be one command name", command)
		}
		if !seen[command] {
			seen[command] = true
			commands = append(commands, command)
		}
	}
	if len(commands) == 0 {
		return shellAllowList{}, nil
	}
	return shellAllowList{commands: commands}, nil
}

func (allowList shellAllowList) configured() bool {
	return allowList.unrestricted || len(allowList.commands) > 0
}

func (allowList shellAllowList) restrictive() bool {
	return len(allowList.commands) > 0 && !allowList.unrestricted
}

func approvalRulesForShellAllowList(rules []dagent.ApprovalRule, allowList shellAllowList) []dagent.ApprovalRule {
	result := slices.Clone(rules)
	if !allowList.configured() {
		return result
	}
	return slices.DeleteFunc(result, func(rule dagent.ApprovalRule) bool { return rule.Pattern == "execute" })
}

var bareShellVariable = regexp.MustCompile(`\$[A-Za-z_]`)

func containsDangerousShellPattern(command string) bool {
	for _, pattern := range []string{
		"$(", "`", "$'", "\n", "\r", "\t", "<(", ">(", "<<<", "<<", ">>", ">", "<", "${",
	} {
		if strings.Contains(command, pattern) {
			return true
		}
	}
	if bareShellVariable.MatchString(command) {
		return true
	}
	for index := 0; index < len(command); index++ {
		if command[index] != '&' {
			continue
		}
		before := index > 0 && command[index-1] == '&'
		after := index+1 < len(command) && command[index+1] == '&'
		if !before && !after {
			return true
		}
	}
	return false
}

func (allowList shellAllowList) allows(command string) bool {
	if !allowList.configured() || strings.TrimSpace(command) == "" {
		return false
	}
	if allowList.unrestricted {
		return true
	}
	if containsDangerousShellPattern(command) {
		return false
	}
	allowed := map[string]bool{}
	for _, command := range allowList.commands {
		allowed[command] = true
	}
	segments, valid := shellCommandSegments(command)
	if !valid || len(segments) == 0 {
		return false
	}
	for _, segment := range segments {
		name, ok := firstShellWord(segment)
		if !ok || !allowed[name] {
			return false
		}
	}
	return true
}

// shellCommandSegments applies the same deliberately narrow first-token model
// used by the upstream policy. Operator boundaries are recognized even inside
// quotes, which can reject harmless commands but cannot broaden execution.
func shellCommandSegments(command string) ([]string, bool) {
	var segments []string
	start := 0
	for index := 0; index < len(command); {
		width := 0
		switch command[index] {
		case ';', '|':
			width = 1
			if index+1 < len(command) && command[index+1] == command[index] {
				width = 2
			}
		case '&':
			if index+1 < len(command) && command[index+1] == '&' {
				width = 2
			}
		}
		if width == 0 {
			index++
			continue
		}
		if segment := strings.TrimSpace(command[start:index]); segment != "" {
			segments = append(segments, segment)
		}
		index += width
		start = index
	}
	if segment := strings.TrimSpace(command[start:]); segment != "" {
		segments = append(segments, segment)
	}
	return segments, len(segments) > 0
}

func firstShellWord(segment string) (string, bool) {
	segment = strings.TrimLeft(segment, " ")
	if segment == "" {
		return "", false
	}
	var word strings.Builder
	quote := byte(0)
	escaped := false
	first := true
	started := false
	for index := 0; index < len(segment); index++ {
		character := segment[index]
		if escaped {
			if first {
				word.WriteByte(character)
				started = true
			}
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else if first {
				word.WriteByte(character)
				started = true
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			if first {
				started = true
			}
			continue
		}
		if character == ' ' {
			if started {
				first = false
			}
			continue
		}
		if first {
			word.WriteByte(character)
			started = true
		}
	}
	if escaped || quote != 0 || !started || word.Len() == 0 {
		return "", false
	}
	return word.String(), true
}

func shellAllowListMiddleware(allowList shellAllowList) dagent.Middleware {
	if !allowList.restrictive() {
		panic("shell allow-list middleware requires a restrictive policy")
	}
	commands := slices.Clone(allowList.commands)
	return dagent.Middleware{
		Name:           "shell_allow_list",
		SerializedName: "ShellAllowListMiddleware",
		WrapToolCall: func(ctx context.Context, request dagent.ToolCallRequest, next dagent.ToolHandler) (dagent.ToolCallResponse, error) {
			if request.Call.Name != "execute" {
				return next(ctx, request)
			}
			var input struct {
				Command string `json:"command"`
			}
			if json.Unmarshal(request.Call.Arguments, &input) == nil && allowList.allows(input.Command) {
				return next(ctx, request)
			}
			return dagent.ToolCallResponse{Result: datool.Result{
				Content: []damessage.ContentBlock{{
					Type: damessage.BlockText,
					Text: fmt.Sprintf("Shell command rejected by the allow-list. Allowed commands: %s. Use an allowed command or another approach.", strings.Join(commands, ", ")),
				}},
				Status: damessage.ToolStatusError,
			}}, nil
		},
	}
}

// shellAllowListBackend keeps the policy at the execution boundary as well as
// in middleware. That prevents delegated agents or future execution paths from
// bypassing the inline tool wrapper.
type shellAllowListBackend struct {
	dabackend.Backend
	sandbox   dabackend.Sandbox
	allowList shellAllowList
}

func enforceShellAllowList(backend dabackend.Backend, allowList shellAllowList) dabackend.Backend {
	if !allowList.restrictive() {
		panic("shell allow-list backend requires a restrictive policy")
	}
	sandbox, ok := dabackend.SandboxOf(backend)
	if !ok {
		panic("shell allow-list backend requires shell capability")
	}
	return &shellAllowListBackend{Backend: backend, sandbox: sandbox, allowList: allowList}
}

func (backend *shellAllowListBackend) ID() string { return backend.sandbox.ID() }

func (backend *shellAllowListBackend) Execute(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	if !backend.allowList.allows(command) {
		return dabackend.ExecuteResult{}, errShellCommandRejected
	}
	return backend.sandbox.Execute(ctx, command, timeout)
}

func (backend *shellAllowListBackend) ExecuteWithOptions(ctx context.Context, command string, options dabackend.ExecuteOptions) (dabackend.ExecuteResult, error) {
	if !backend.allowList.allows(command) {
		return dabackend.ExecuteResult{}, errShellCommandRejected
	}
	return dabackend.ExecuteSandbox(ctx, backend.sandbox, command, options)
}

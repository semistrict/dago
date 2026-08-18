package datalon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"
)

type pendingApproval struct {
	senderID string
	result   chan approvalResult
	resolved bool
}

type approvalResult struct {
	decision ToolApprovalDecision
	err      error
}

type approvalKey struct {
	channelID      string
	conversationID string
}

type approvalRegistry struct {
	mu      sync.Mutex
	pending map[approvalKey]*pendingApproval
}

func (registry *approvalRegistry) install(key approvalKey, pending *pendingApproval) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.pending == nil {
		registry.pending = make(map[approvalKey]*pendingApproval)
	}
	if _, exists := registry.pending[key]; exists {
		return ErrToolApprovalPending
	}
	registry.pending[key] = pending
	return nil
}

func (registry *approvalRegistry) remove(key approvalKey, pending *pendingApproval) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.pending[key] == pending {
		delete(registry.pending, key)
	}
}

func (registry *approvalRegistry) consume(key approvalKey, message Message) bool {
	decision, recognized := parseToolApprovalReply(message.Text)
	if !recognized {
		return false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	pending := registry.pending[key]
	if pending == nil || pending.resolved {
		return false
	}
	if pending.senderID != "" && message.SenderID != pending.senderID {
		return false
	}
	pending.resolved = true
	pending.result <- approvalResult{decision: decision}
	return true
}

func (registry *approvalRegistry) clear(cause error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, pending := range registry.pending {
		if pending.resolved {
			continue
		}
		pending.resolved = true
		pending.result <- approvalResult{decision: ToolApprovalReject, err: cause}
	}
	clear(registry.pending)
}

func (host *Host) requestToolApproval(
	ctx context.Context,
	channel Channel,
	channelConversationID, agentConversationID, senderID string,
	request ToolApprovalRequest,
	config Config,
) (ToolApprovalDecision, error) {
	if err := ctx.Err(); err != nil {
		return ToolApprovalReject, err
	}
	if request.ConversationID != "" && request.ConversationID != agentConversationID {
		return ToolApprovalReject, fmt.Errorf("%w: conversation does not match the active request", ErrInvalidToolApproval)
	}
	request.ConversationID = agentConversationID
	prompt, err := formatToolApprovalPrompt(request, config)
	if err != nil {
		return ToolApprovalReject, err
	}
	pending := &pendingApproval{senderID: senderID, result: make(chan approvalResult, 1)}
	key := approvalKey{channelID: channel.ID(), conversationID: channelConversationID}
	if err := host.approvals.install(key, pending); err != nil {
		return ToolApprovalReject, err
	}
	defer host.approvals.remove(key, pending)
	if err := host.send(ctx, channel, channelConversationID, prompt, config.SendTimeout); err != nil {
		return ToolApprovalReject, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, config.ApprovalTimeout)
	defer cancel()
	select {
	case result := <-pending.result:
		return result.decision, result.err
	case <-waitCtx.Done():
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return ToolApprovalReject, errors.Join(ErrToolApprovalTimeout, waitCtx.Err())
		}
		return ToolApprovalReject, waitCtx.Err()
	}
}

func formatToolApprovalPrompt(request ToolApprovalRequest, config Config) (string, error) {
	if request.InterruptID == "" || len(request.InterruptID) > 1024 || strings.IndexFunc(request.InterruptID, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: interrupt ID is required and must be at most 1024 safe bytes", ErrInvalidToolApproval)
	}
	if len(request.Actions) == 0 || len(request.Actions) > config.MaxApprovalActions {
		return "", fmt.Errorf("%w: action count must be between 1 and %d", ErrInvalidToolApproval, config.MaxApprovalActions)
	}
	lines := []string{"Tool approval required."}
	for index, action := range request.Actions {
		name := strings.TrimSpace(action.Name)
		if name == "" {
			name = "unknown"
		}
		if len(name) > 256 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return "", fmt.Errorf("%w: action %d has an unsafe name", ErrInvalidToolApproval, index)
		}
		if len(action.ID) > 1024 || strings.IndexFunc(action.ID, unicode.IsControl) >= 0 {
			return "", fmt.Errorf("%w: action %d has an unsafe ID", ErrInvalidToolApproval, index)
		}
		lines = append(lines, fmt.Sprintf("%d. `%s`", index+1, escapeApprovalMarkdown(name)))
		if len(action.Arguments) > 0 {
			if !json.Valid(action.Arguments) {
				return "", fmt.Errorf("%w: action %d arguments are not valid JSON", ErrInvalidToolApproval, index)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, action.Arguments); err != nil {
				return "", fmt.Errorf("%w: compact action %d arguments", ErrInvalidToolApproval, index)
			}
			lines = append(lines, "Args: `"+escapeApprovalMarkdown(compact.String())+"`")
		}
	}
	lines = append(lines, "Reply `👍` / `approve` to run or `👎` / `deny` to skip.")
	prompt := strings.Join(lines, "\n")
	if len(prompt) > config.MaxApprovalPromptBytes {
		return "", fmt.Errorf("%w: prompt exceeds %d bytes", ErrInvalidToolApproval, config.MaxApprovalPromptBytes)
	}
	return prompt, nil
}

func escapeApprovalMarkdown(value string) string {
	return strings.ReplaceAll(value, "`", "\\`")
}

func parseToolApprovalReply(text string) (ToolApprovalDecision, bool) {
	normalized := strings.Trim(strings.ToLower(strings.TrimSpace(text)), ".! ")
	fields := strings.Fields(normalized)
	if len(fields) == 0 {
		return "", false
	}
	first := normalizeApprovalEmoji(fields[0])
	switch first {
	case "approve", "approved", "yes", "y", "👍":
		return ToolApprovalApprove, true
	case "deny", "denied", "reject", "rejected", "no", "n", "👎":
		return ToolApprovalReject, true
	default:
		return "", false
	}
}

func normalizeApprovalEmoji(value string) string {
	return strings.Map(func(char rune) rune {
		if char == '\ufe0f' || (char >= '\U0001f3fb' && char <= '\U0001f3ff') {
			return -1
		}
		return char
	}, strings.TrimSpace(value))
}

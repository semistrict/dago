package daacp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
)

type projector struct {
	connection *acp.AgentSideConnection
	sessionID  acp.SessionId
	tools      map[string]toolProjection
	streamed   bool
	fallback   []damessage.ContentBlock
}

type toolProjection struct {
	name      string
	arguments any
	kind      acp.ToolKind
	locations []acp.ToolCallLocation
}

func newProjector(connection *acp.AgentSideConnection, sessionID acp.SessionId) *projector {
	return &projector{connection: connection, sessionID: sessionID, tools: map[string]toolProjection{}}
}

func (projector *projector) consume(ctx context.Context, stream *dagent.Stream) (dagent.Result, error) {
	if stream == nil {
		return dagent.Result{}, fmt.Errorf("runner returned a nil stream")
	}
	for {
		event, err := stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = stream.Close()
			return dagent.Result{}, err
		}
		if err := projector.event(ctx, event); err != nil {
			_ = stream.Close()
			return dagent.Result{}, err
		}
	}
	if err := stream.Close(); err != nil {
		return dagent.Result{}, err
	}
	result, err := stream.Result(ctx)
	if err != nil {
		return dagent.Result{}, err
	}
	if !projector.streamed {
		if err := projector.finalMessages(ctx); err != nil {
			return dagent.Result{}, err
		}
	}
	return result, nil
}

func (projector *projector) event(ctx context.Context, event dagent.Event) error {
	switch event.Mode {
	case dagent.EventToken:
		if event.Chunk == nil {
			return nil
		}
		for _, block := range event.Chunk.MessageDelta.Content {
			var update acp.SessionUpdate
			switch block.Type {
			case damessage.BlockText:
				if block.Text == "" {
					continue
				}
				projector.streamed = true
				update = acp.UpdateAgentMessageText(block.Text)
			case damessage.BlockReasoning:
				if block.Reasoning == "" {
					continue
				}
				projector.streamed = true
				update = acp.UpdateAgentThoughtText(block.Reasoning)
			default:
				continue
			}
			if err := projector.send(ctx, update); err != nil {
				return err
			}
		}
	case dagent.EventUpdate:
		if err := projector.updateMessages(ctx, event.Update[dagent.MessagesKey]); err != nil {
			return err
		}
		if rawTodos, ok := event.Update["todos"]; ok {
			if err := projector.updatePlan(ctx, rawTodos); err != nil {
				return err
			}
		}
	case dagent.EventToolProgress:
		if event.ToolProgress == nil || event.ToolProgress.CallID == "" {
			return nil
		}
		if _, exists := projector.tools[event.ToolProgress.CallID]; !exists {
			name := event.ToolProgress.Name
			if name == "" {
				name = "Tool"
			}
			projector.tools[event.ToolProgress.CallID] = toolProjection{name: name, kind: toolKind(name)}
			if err := projector.send(ctx, acp.StartToolCall(
				acp.ToolCallId(event.ToolProgress.CallID), name,
				acp.WithStartKind(toolKind(name)), acp.WithStartStatus(acp.ToolCallStatusInProgress),
			)); err != nil {
				return err
			}
		}
		options := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(acp.ToolCallStatusInProgress)}
		if event.ToolProgress.Output != "" {
			options = append(options, acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(event.ToolProgress.Output))}))
		}
		return projector.send(ctx, acp.UpdateToolCall(acp.ToolCallId(event.ToolProgress.CallID), options...))
	}
	return nil
}

func (projector *projector) updateMessages(ctx context.Context, value any) error {
	messages, err := projectionMessages(value)
	if err != nil {
		return err
	}
	for _, message := range messages {
		switch message.Role {
		case damessage.RoleAssistant:
			if len(message.ToolCalls) == 0 {
				projector.fallback = append(projector.fallback, message.Content...)
			}
			for _, call := range message.ToolCalls {
				if _, exists := projector.tools[call.ID]; exists {
					continue
				}
				var arguments any
				if len(call.Arguments) > 0 {
					_ = json.Unmarshal(call.Arguments, &arguments)
				}
				projection := toolProjection{
					name: call.Name, arguments: arguments, kind: toolKind(call.Name),
					locations: toolLocations(arguments),
				}
				projector.tools[call.ID] = projection
				if err := projector.send(ctx, acp.StartToolCall(
					acp.ToolCallId(call.ID), call.Name,
					acp.WithStartKind(projection.kind), acp.WithStartStatus(acp.ToolCallStatusPending),
					acp.WithStartRawInput(arguments), acp.WithStartLocations(projection.locations),
				)); err != nil {
					return err
				}
			}
		case damessage.RoleTool:
			status := acp.ToolCallStatusCompleted
			if message.ToolStatus == damessage.ToolStatusError {
				status = acp.ToolCallStatusFailed
			}
			text := message.TextContent()
			options := []acp.ToolCallUpdateOpt{
				acp.WithUpdateStatus(status), acp.WithUpdateRawOutput(text),
			}
			if text != "" {
				options = append(options, acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(text))}))
			}
			if err := projector.send(ctx, acp.UpdateToolCall(acp.ToolCallId(message.ToolCallID), options...)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (projector *projector) finalMessages(ctx context.Context) error {
	for _, block := range projector.fallback {
		switch block.Type {
		case damessage.BlockText:
			if block.Text != "" {
				if err := projector.send(ctx, acp.UpdateAgentMessageText(block.Text)); err != nil {
					return err
				}
			}
		case damessage.BlockReasoning:
			if block.Reasoning != "" {
				if err := projector.send(ctx, acp.UpdateAgentThoughtText(block.Reasoning)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (projector *projector) updatePlan(ctx context.Context, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode ACP plan: %w", err)
	}
	var todos []dagent.Todo
	if err := json.Unmarshal(encoded, &todos); err != nil {
		return fmt.Errorf("decode ACP plan: %w", err)
	}
	entries := make([]acp.PlanEntry, 0, len(todos))
	for _, todo := range todos {
		status := acp.PlanEntryStatusPending
		switch todo.Status {
		case "in_progress":
			status = acp.PlanEntryStatusInProgress
		case "completed":
			status = acp.PlanEntryStatusCompleted
		}
		entries = append(entries, acp.PlanEntry{Content: todo.Content, Priority: acp.PlanEntryPriorityMedium, Status: status})
	}
	return projector.send(ctx, acp.UpdatePlan(entries...))
}

func (projector *projector) requestApprovals(ctx context.Context, interrupts []dagent.Interrupt) (dagent.ApprovalResponse, error) {
	decisions := map[string]dagent.ApprovalChoice{}
	for _, interrupt := range interrupts {
		if interrupt.ID != "human_approval" {
			return dagent.ApprovalResponse{}, fmt.Errorf("ACP cannot resume interrupt %q", interrupt.ID)
		}
		encoded, err := json.Marshal(interrupt.Value)
		if err != nil {
			return dagent.ApprovalResponse{}, fmt.Errorf("encode ACP approval: %w", err)
		}
		var requests []dagent.ApprovalRequest
		if err := json.Unmarshal(encoded, &requests); err != nil {
			return dagent.ApprovalResponse{}, fmt.Errorf("decode ACP approval: %w", err)
		}
		for _, request := range requests {
			projection := projector.tools[request.Call.ID]
			if projection.name == "" {
				projection.name = request.Call.Name
				projection.kind = toolKind(request.Call.Name)
				_ = json.Unmarshal(request.Call.Arguments, &projection.arguments)
				projection.locations = toolLocations(projection.arguments)
			}
			options := approvalOptions(request.AllowedDecisions)
			if len(options) == 0 {
				return dagent.ApprovalResponse{}, fmt.Errorf("ACP permission UI cannot represent the allowed decisions for tool %q", request.Call.Name)
			}
			title := projection.name
			if request.Description != "" {
				title = request.Description
			}
			response, err := projector.connection.RequestPermission(ctx, acp.RequestPermissionRequest{
				SessionId: projector.sessionID,
				ToolCall: acp.ToolCallUpdate{
					ToolCallId: acp.ToolCallId(request.Call.ID), Title: acp.Ptr(title),
					Kind: acp.Ptr(projection.kind), Status: acp.Ptr(acp.ToolCallStatusPending),
					RawInput: projection.arguments, Locations: projection.locations,
				},
				Options: options,
			})
			if err != nil {
				return dagent.ApprovalResponse{}, fmt.Errorf("request ACP permission: %w", err)
			}
			if response.Outcome.Cancelled != nil {
				return dagent.ApprovalResponse{}, errPermissionCancelled
			}
			if response.Outcome.Selected == nil {
				return dagent.ApprovalResponse{}, fmt.Errorf("ACP permission response omitted its outcome")
			}
			decision := dagent.ApprovalDecision(response.Outcome.Selected.OptionId)
			if decision != dagent.ApprovalApprove && decision != dagent.ApprovalReject {
				return dagent.ApprovalResponse{}, fmt.Errorf("ACP permission selected unknown option %q", response.Outcome.Selected.OptionId)
			}
			decisions[request.Call.ID] = dagent.ApprovalChoice{Decision: decision}
		}
	}
	return dagent.ApprovalResponse{Decisions: decisions}, nil
}

func approvalOptions(decisions []dagent.ApprovalDecision) []acp.PermissionOption {
	var options []acp.PermissionOption
	for _, decision := range decisions {
		switch decision {
		case dagent.ApprovalApprove:
			options = append(options, acp.PermissionOption{
				OptionId: acp.PermissionOptionId(dagent.ApprovalApprove), Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce,
			})
		case dagent.ApprovalReject:
			options = append(options, acp.PermissionOption{
				OptionId: acp.PermissionOptionId(dagent.ApprovalReject), Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce,
			})
		}
	}
	return options
}

func (projector *projector) send(ctx context.Context, update acp.SessionUpdate) error {
	if err := projector.connection.SessionUpdate(ctx, acp.SessionNotification{SessionId: projector.sessionID, Update: update}); err != nil {
		return fmt.Errorf("send ACP session update: %w", err)
	}
	return nil
}

func projectionMessages(value any) ([]damessage.Message, error) {
	if value == nil {
		return nil, nil
	}
	switch typed := value.(type) {
	case dastate.Overwrite:
		messages, err := projectionMessages(typed.Value)
		if err != nil {
			return nil, err
		}
		for index := len(messages) - 1; index >= 0; index-- {
			if messages[index].Role == damessage.RoleAssistant && len(messages[index].ToolCalls) > 0 {
				return messages[index:], nil
			}
		}
		return nil, nil
	case []damessage.Message:
		return typed, nil
	case []any:
		messages := make([]damessage.Message, 0, len(typed))
		for index, item := range typed {
			message, ok := item.(damessage.Message)
			if !ok {
				return nil, fmt.Errorf("ACP messages[%d] has type %T", index, item)
			}
			messages = append(messages, message)
		}
		return messages, nil
	default:
		return nil, fmt.Errorf("ACP messages have type %T", value)
	}
}

func toolKind(name string) acp.ToolKind {
	name = strings.ToLower(name)
	switch {
	case strings.Contains(name, "delete"), strings.Contains(name, "remove"):
		return acp.ToolKindDelete
	case strings.Contains(name, "move"), strings.Contains(name, "rename"):
		return acp.ToolKindMove
	case strings.Contains(name, "write"), strings.Contains(name, "edit"), strings.Contains(name, "patch"):
		return acp.ToolKindEdit
	case strings.Contains(name, "read"), strings.Contains(name, "list"):
		return acp.ToolKindRead
	case strings.Contains(name, "search"), strings.Contains(name, "grep"), strings.Contains(name, "find"):
		return acp.ToolKindSearch
	case strings.Contains(name, "fetch"), strings.Contains(name, "http"), strings.Contains(name, "web"):
		return acp.ToolKindFetch
	case strings.Contains(name, "shell"), strings.Contains(name, "exec"), strings.Contains(name, "terminal"), strings.Contains(name, "command"):
		return acp.ToolKindExecute
	case strings.Contains(name, "think"), strings.Contains(name, "plan"), strings.Contains(name, "todo"):
		return acp.ToolKindThink
	default:
		return acp.ToolKindOther
	}
}

func toolLocations(arguments any) []acp.ToolCallLocation {
	record, ok := arguments.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"path", "file_path", "file", "uri"} {
		value, ok := record[key].(string)
		if ok && value != "" {
			return []acp.ToolCallLocation{{Path: value}}
		}
	}
	return nil
}

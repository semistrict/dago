package daacp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
)

const cancelCleanupTimeout = 10 * time.Second

var errPermissionCancelled = errors.New("ACP permission request cancelled")

type protocolAgent struct {
	root    context.Context
	runner  Runner
	options Options

	connectionReady chan struct{}
	connection      *acp.AgentSideConnection

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	cwd    string
	active *activeTurn
}

type activeTurn struct {
	cancel context.CancelFunc
}

func newProtocolAgent(ctx context.Context, runner Runner, options Options) *protocolAgent {
	return &protocolAgent{
		root: ctx, runner: runner, options: options,
		connectionReady: make(chan struct{}), sessions: map[string]*session{},
	}
}

func (agent *protocolAgent) setConnection(connection *acp.AgentSideConnection) {
	agent.connection = connection
	close(agent.connectionReady)
}

func (agent *protocolAgent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
			PromptCapabilities: acp.PromptCapabilities{
				Image: agent.options.ImagePrompts, Audio: agent.options.AudioPrompts,
				EmbeddedContext: agent.options.EmbeddedContext,
			},
			SessionCapabilities: acp.SessionCapabilities{
				Close: &acp.SessionCloseCapabilities{},
			},
		},
		AgentInfo:   &acp.Implementation{Name: agent.options.Name, Version: agent.options.Version},
		AuthMethods: []acp.AuthMethod{},
	}, nil
}

func (agent *protocolAgent) NewSession(_ context.Context, request acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if !filepath.IsAbs(request.Cwd) {
		return acp.NewSessionResponse{}, acp.NewInvalidParams(map[string]any{"cwd": "must be absolute"})
	}
	for _, directory := range request.AdditionalDirectories {
		if !filepath.IsAbs(directory) {
			return acp.NewSessionResponse{}, acp.NewInvalidParams(map[string]any{"additionalDirectories": "all entries must be absolute"})
		}
	}
	if len(request.AdditionalDirectories) > 0 {
		return acp.NewSessionResponse{}, acp.NewInvalidParams(map[string]any{"additionalDirectories": "additional workspace roots are not supported"})
	}
	if len(request.McpServers) > 0 {
		return acp.NewSessionResponse{}, acp.NewInvalidParams(map[string]any{"mcpServers": "MCP-over-ACP is not supported"})
	}
	id, err := sessionID()
	if err != nil {
		return acp.NewSessionResponse{}, fmt.Errorf("create session id: %w", err)
	}
	agent.mu.Lock()
	agent.sessions[id] = &session{cwd: request.Cwd}
	agent.mu.Unlock()
	return acp.NewSessionResponse{SessionId: acp.SessionId(id)}, nil
}

func (agent *protocolAgent) Prompt(requestContext context.Context, request acp.PromptRequest) (acp.PromptResponse, error) {
	message, err := agent.promptMessage(request.Prompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	snapshot, turnContext, finish, err := agent.beginTurn(requestContext, string(request.SessionId))
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer finish()

	connection, err := agent.waitForConnection(turnContext)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	configurable := cloneConfigurable(agent.options.Configurable)
	if configurable == nil {
		configurable = map[string]any{}
	}
	configurable[ConfigurableCWD] = snapshot.cwd

	input := dagent.Input{
		Config:   dacheckpoint.Config{ThreadID: string(request.SessionId)},
		Messages: []damessage.Message{message}, Configurable: configurable,
	}
	projector := newProjector(connection, request.SessionId)
	for {
		stream := agent.runner.Stream(turnContext, input)
		result, streamErr := projector.consume(turnContext, stream)
		if streamErr != nil {
			if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) || turnContext.Err() != nil {
				agent.cleanupCancelled(input)
				return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: request.MessageId}, nil
			}
			return acp.PromptResponse{}, fmt.Errorf("run ACP prompt: %w", streamErr)
		}
		if len(result.Interrupts) == 0 {
			return acp.PromptResponse{
				StopReason: stopReason(result), UserMessageId: request.MessageId,
			}, nil
		}
		resume, permissionErr := projector.requestApprovals(turnContext, result.Interrupts)
		if permissionErr != nil {
			if errors.Is(permissionErr, errPermissionCancelled) || errors.Is(permissionErr, context.Canceled) {
				agent.cleanupCancelled(input)
				return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: request.MessageId}, nil
			}
			return acp.PromptResponse{}, permissionErr
		}
		input.Messages = nil
		input.Resume = resume
	}
}

func (agent *protocolAgent) Cancel(_ context.Context, request acp.CancelNotification) error {
	agent.cancelSession(string(request.SessionId))
	return nil
}

func (agent *protocolAgent) CloseSession(_ context.Context, request acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	agent.mu.Lock()
	current, ok := agent.sessions[string(request.SessionId)]
	var active *activeTurn
	if ok {
		active = current.active
		delete(agent.sessions, string(request.SessionId))
	}
	agent.mu.Unlock()
	if !ok {
		return acp.CloseSessionResponse{}, acp.NewInvalidParams(map[string]any{"sessionId": "unknown session"})
	}
	if active != nil {
		active.cancel()
	}
	return acp.CloseSessionResponse{}, nil
}

func (*protocolAgent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, acp.NewMethodNotFound(acp.AgentMethodAuthenticate)
}

func (*protocolAgent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}

func (*protocolAgent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
}

func (*protocolAgent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
}

func (*protocolAgent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}

func (*protocolAgent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

func (agent *protocolAgent) beginTurn(requestContext context.Context, id string) (session, context.Context, func(), error) {
	agent.mu.Lock()
	current, ok := agent.sessions[id]
	if !ok {
		agent.mu.Unlock()
		return session{}, nil, nil, acp.NewInvalidParams(map[string]any{"sessionId": "unknown session"})
	}
	turnContext, cancel := context.WithCancel(requestContext)
	stopRoot := context.AfterFunc(agent.root, cancel)
	active := &activeTurn{cancel: cancel}
	previous := current.active
	current.active = active
	snapshot := session{cwd: current.cwd}
	agent.mu.Unlock()
	if previous != nil {
		previous.cancel()
	}
	finish := func() {
		stopRoot()
		cancel()
		agent.mu.Lock()
		if current.active == active {
			current.active = nil
		}
		agent.mu.Unlock()
	}
	return snapshot, turnContext, finish, nil
}

func (agent *protocolAgent) waitForConnection(ctx context.Context) (*acp.AgentSideConnection, error) {
	select {
	case <-agent.connectionReady:
		return agent.connection, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func (agent *protocolAgent) cancelSession(id string) {
	agent.mu.Lock()
	current := agent.sessions[id]
	var active *activeTurn
	if current != nil {
		active = current.active
	}
	agent.mu.Unlock()
	if active != nil {
		active.cancel()
	}
}

func (agent *protocolAgent) cancelAll() {
	agent.mu.Lock()
	active := make([]*activeTurn, 0, len(agent.sessions))
	for _, current := range agent.sessions {
		if current.active != nil {
			active = append(active, current.active)
		}
	}
	agent.mu.Unlock()
	for _, turn := range active {
		turn.cancel()
	}
}

func (agent *protocolAgent) cleanupCancelled(input dagent.Input) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelCleanupTimeout)
	defer cancel()
	_, _ = agent.runner.Cancel(ctx, dagent.Input{Config: input.Config, Configurable: input.Configurable})
}

func (agent *protocolAgent) promptMessage(blocks []acp.ContentBlock) (damessage.Message, error) {
	message := damessage.Message{Role: damessage.RoleHuman}
	for index, block := range blocks {
		switch {
		case block.Text != nil:
			message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: block.Text.Text})
		case block.ResourceLink != nil:
			label := block.ResourceLink.Name
			if block.ResourceLink.Title != nil && *block.ResourceLink.Title != "" {
				label = *block.ResourceLink.Title
			}
			message.Content = append(message.Content, damessage.ContentBlock{
				Type: damessage.BlockText, Text: fmt.Sprintf("[%s](%s)", label, block.ResourceLink.Uri),
			})
		case block.Image != nil:
			if !agent.options.ImagePrompts {
				return damessage.Message{}, acp.NewInvalidParams(map[string]any{"prompt": fmt.Sprintf("block %d is an unadvertised image", index)})
			}
			data, err := base64.StdEncoding.DecodeString(block.Image.Data)
			if err != nil {
				return damessage.Message{}, acp.NewInvalidParams(map[string]any{"prompt": fmt.Sprintf("block %d has invalid image data", index)})
			}
			message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockImage, Data: data, MIMEType: block.Image.MimeType})
		case block.Audio != nil:
			if !agent.options.AudioPrompts {
				return damessage.Message{}, acp.NewInvalidParams(map[string]any{"prompt": fmt.Sprintf("block %d is unadvertised audio", index)})
			}
			data, err := base64.StdEncoding.DecodeString(block.Audio.Data)
			if err != nil {
				return damessage.Message{}, acp.NewInvalidParams(map[string]any{"prompt": fmt.Sprintf("block %d has invalid audio data", index)})
			}
			message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockAudio, Data: data, MIMEType: block.Audio.MimeType})
		case block.Resource != nil:
			if !agent.options.EmbeddedContext {
				return damessage.Message{}, acp.NewInvalidParams(map[string]any{"prompt": fmt.Sprintf("block %d is unadvertised embedded context", index)})
			}
			resource := block.Resource.Resource
			if resource.TextResourceContents != nil {
				item := resource.TextResourceContents
				message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockFile, Text: item.Text, URL: item.Uri, MIMEType: stringValue(item.MimeType)})
			} else if resource.BlobResourceContents != nil {
				item := resource.BlobResourceContents
				data, err := base64.StdEncoding.DecodeString(item.Blob)
				if err != nil {
					return damessage.Message{}, acp.NewInvalidParams(map[string]any{"prompt": fmt.Sprintf("block %d has invalid resource data", index)})
				}
				message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockFile, Data: data, URL: item.Uri, MIMEType: stringValue(item.MimeType)})
			} else {
				return damessage.Message{}, acp.NewInvalidParams(map[string]any{"prompt": fmt.Sprintf("block %d has an empty resource", index)})
			}
		default:
			return damessage.Message{}, acp.NewInvalidParams(map[string]any{"prompt": fmt.Sprintf("block %d has no content variant", index)})
		}
	}
	if err := message.Validate(); err != nil {
		return damessage.Message{}, acp.NewInvalidParams(map[string]any{"prompt": err.Error()})
	}
	return message, nil
}

func sessionID() (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	return "session-" + hex.EncodeToString(value), nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func stopReason(result dagent.Result) acp.StopReason {
	for index := len(result.Messages) - 1; index >= 0; index-- {
		message := result.Messages[index]
		if message.Role != damessage.RoleAssistant {
			continue
		}
		reason, _ := damodel.Outcome(message)
		switch reason {
		case damodel.FinishReasonMaxTokens:
			return acp.StopReasonMaxTokens
		case damodel.FinishReasonRefusal:
			return acp.StopReasonRefusal
		default:
			return acp.StopReasonEndTurn
		}
	}
	return acp.StopReasonEndTurn
}

var _ acp.Agent = (*protocolAgent)(nil)

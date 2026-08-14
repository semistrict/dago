package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/semistrict/dago/damodel"
)

const responsesWebSocketBeta = "responses_websockets=2026-02-06"

type responsesWebSocketPool struct {
	mu          sync.Mutex
	connections []*responsesWebSocketConnection
	disabled    bool
}

func (pool *responsesWebSocketPool) enabled() bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return !pool.disabled
}

func (pool *responsesWebSocketPool) disable() {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.disabled = true
}

type responsesWebSocketConnection struct {
	conn         *websocket.Conn
	inbox        *responsesWebSocketInbox
	busy         bool
	continuation *responsesContinuation
}

type responsesWebSocketMessage struct {
	messageType websocket.MessageType
	data        []byte
	err         error
}

// responsesWebSocketInbox is an unbounded queue so the connection reader never
// stops processing control frames while an application stream is idle or slow.
type responsesWebSocketInbox struct {
	mu     sync.Mutex
	queued []responsesWebSocketMessage
	ready  chan struct{}
}

func newResponsesWebSocketInbox() *responsesWebSocketInbox {
	return &responsesWebSocketInbox{ready: make(chan struct{}, 1)}
}

func (inbox *responsesWebSocketInbox) push(message responsesWebSocketMessage) {
	inbox.mu.Lock()
	inbox.queued = append(inbox.queued, message)
	inbox.mu.Unlock()
	select {
	case inbox.ready <- struct{}{}:
	default:
	}
}

func (inbox *responsesWebSocketInbox) next(ctx context.Context) (responsesWebSocketMessage, error) {
	for {
		if err := ctx.Err(); err != nil {
			return responsesWebSocketMessage{}, err
		}
		inbox.mu.Lock()
		if len(inbox.queued) > 0 {
			message := inbox.queued[0]
			inbox.queued[0] = responsesWebSocketMessage{}
			inbox.queued = inbox.queued[1:]
			more := len(inbox.queued) > 0
			inbox.mu.Unlock()
			if more {
				select {
				case inbox.ready <- struct{}{}:
				default:
				}
			}
			return message, nil
		}
		inbox.mu.Unlock()

		select {
		case <-ctx.Done():
			return responsesWebSocketMessage{}, ctx.Err()
		case <-inbox.ready:
		}
	}
}

func startResponsesWebSocketReader(connection *websocket.Conn) *responsesWebSocketInbox {
	inbox := newResponsesWebSocketInbox()
	go func() {
		for {
			messageType, data, err := connection.Read(context.Background())
			inbox.push(responsesWebSocketMessage{messageType: messageType, data: data, err: err})
			if err != nil {
				return
			}
		}
	}()
	return inbox
}

type responsesContinuation struct {
	responseID string
	prefix     []json.RawMessage
}

type responsesWebSocketRequest struct {
	Type               string `json:"type"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	Generate           *bool  `json:"generate,omitempty"`
	responsesRequest
}

func (client *Client) streamWebSocket(ctx context.Context, request responsesRequest) (damodel.Stream, error) {
	return client.streamWebSocketRequest(ctx, request, nil)
}

func (client *Client) streamWebSocketRequest(ctx context.Context, request responsesRequest, generate *bool) (damodel.Stream, error) {
	for attempt := 0; ; attempt++ {
		stream, err := client.startWebSocketStream(ctx, request, generate)
		if err == nil {
			return stream, nil
		}
		if !client.canRetry(ctx, attempt, err) {
			return nil, err
		}
		if err := client.waitRetry(ctx, attempt, err); err != nil {
			return nil, err
		}
	}
}

func (client *Client) startWebSocketStream(ctx context.Context, request responsesRequest, generate *bool) (damodel.Stream, error) {
	connection, input, err := client.websockets.acquire(request.Input)
	if err != nil {
		return nil, fmt.Errorf("openai: prepare websocket continuation: %w", err)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			client.websockets.release(connection, nil, true)
		}
	}()

	if connection.conn == nil {
		connection.conn, err = client.dialWebSocket(ctx)
		if err != nil {
			return nil, err
		}
		connection.inbox = startResponsesWebSocketReader(connection.conn)
	}
	payload := responsesWebSocketRequest{
		Type:               "response.create",
		PreviousResponseID: input.previousResponseID,
		Generate:           generate,
		responsesRequest:   request,
	}
	payload.Input = input.items
	// Streaming is intrinsic to this transport and is not a response.create
	// field in WebSocket mode.
	payload.Stream = false
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("openai: encode websocket request: %w", err)
	}
	if err := connection.conn.Write(ctx, websocket.MessageText, body); err != nil {
		return nil, fmt.Errorf("openai: write websocket request: %w", err)
	}

	succeeded = true
	return &websocketResponseStream{
		ctx:        ctx,
		client:     client,
		pool:       client.websockets,
		connection: connection,
		inbox:      connection.inbox,
		request:    request,
		parser:     newResponseEventParser(ctx),
	}, nil
}

type websocketInput struct {
	items              []any
	previousResponseID string
}

func (pool *responsesWebSocketPool) acquire(input []any) (*responsesWebSocketConnection, websocketInput, error) {
	canonical, err := canonicalInput(input)
	if err != nil {
		return nil, websocketInput{}, err
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, connection := range pool.connections {
		if connection.busy || connection.conn == nil || connection.continuation == nil {
			continue
		}
		prefix := connection.continuation.prefix
		if len(prefix) > len(canonical) || !equalCanonicalInput(prefix, canonical[:len(prefix)]) {
			continue
		}
		connection.busy = true
		return connection, websocketInput{
			items:              input[len(prefix):],
			previousResponseID: connection.continuation.responseID,
		}, nil
	}
	for _, connection := range pool.connections {
		if connection.busy {
			continue
		}
		connection.busy = true
		connection.continuation = nil
		return connection, websocketInput{items: input}, nil
	}
	connection := &responsesWebSocketConnection{busy: true}
	pool.connections = append(pool.connections, connection)
	return connection, websocketInput{items: input}, nil
}

func (pool *responsesWebSocketPool) release(connection *responsesWebSocketConnection, continuation *responsesContinuation, invalidate bool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if invalidate {
		if connection.conn != nil {
			_ = connection.conn.CloseNow()
		}
		connection.conn = nil
		connection.inbox = nil
		connection.continuation = nil
	} else {
		connection.continuation = continuation
	}
	connection.busy = false
}

func canonicalInput(input []any) ([]json.RawMessage, error) {
	result := make([]json.RawMessage, len(input))
	for index, item := range input {
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("input item %d: %w", index, err)
		}
		result[index] = encoded
	}
	return result, nil
}

func equalCanonicalInput(left, right []json.RawMessage) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !bytes.Equal(left[index], right[index]) {
			return false
		}
	}
	return true
}

func (client *Client) dialWebSocket(ctx context.Context) (*websocket.Conn, error) {
	credentials, err := client.credentials.Credentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("openai: credentials: %w", err)
	}
	if credentials.AccessToken == "" {
		return nil, fmt.Errorf("openai: credential source returned an empty token")
	}
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+credentials.AccessToken)
	header.Set("OpenAI-Beta", responsesWebSocketBeta)
	if credentials.AccountID != "" {
		header.Set("ChatGPT-Account-ID", credentials.AccountID)
	}
	if client.options.Organization != "" {
		header.Set("OpenAI-Organization", client.options.Organization)
	}
	if client.options.Project != "" {
		header.Set("OpenAI-Project", client.options.Project)
	}
	for key, values := range client.options.Headers {
		header.Del(key)
		for _, value := range values {
			header.Add(key, value)
		}
	}
	connection, response, err := websocket.Dial(
		ctx,
		client.options.BaseURL+"/responses",
		websocketDialOptions(client.options.HTTPClient, header),
	)
	if err != nil {
		if response != nil {
			if responseErr := responseError(response); responseErr != nil {
				return nil, client.decorateError(responseErr)
			}
		}
		return nil, fmt.Errorf("openai: websocket handshake: %w", err)
	}
	connection.SetReadLimit(2 << 20)
	return connection, nil
}

type websocketResponseStream struct {
	ctx         context.Context
	client      *Client
	pool        *responsesWebSocketPool
	connection  *responsesWebSocketConnection
	inbox       *responsesWebSocketInbox
	request     responsesRequest
	parser      *responseStream
	releaseOnce sync.Once
}

func newResponseEventParser(ctx context.Context) *responseStream {
	return &responseStream{
		ctx: ctx, calls: map[string]responseOutput{}, emittedCalls: map[string]struct{}{},
		emittedReasoning: map[string]string{}, emittedServer: map[string]struct{}{},
	}
}

func (stream *websocketResponseStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return damodel.Chunks(stream.ctx, stream)
}

func (stream *websocketResponseStream) Next(ctx context.Context) (damodel.Chunk, error) {
	if len(stream.parser.queued) > 0 {
		result := stream.parser.queued[0]
		stream.parser.queued = stream.parser.queued[1:]
		return result, nil
	}
	if stream.parser.done {
		return damodel.Chunk{}, io.EOF
	}
	for {
		message, err := stream.inbox.next(ctx)
		if err != nil {
			stream.invalidate()
			return damodel.Chunk{}, err
		}
		if message.err != nil {
			stream.invalidate()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return damodel.Chunk{}, ctxErr
			}
			return damodel.Chunk{}, fmt.Errorf("openai: read websocket response: %w", message.err)
		}
		if message.messageType != websocket.MessageText {
			stream.invalidate()
			return damodel.Chunk{}, fmt.Errorf("openai: unexpected binary websocket event")
		}
		chunk, emit, err := stream.parser.event(message.data)
		if err != nil {
			stream.invalidate()
			return damodel.Chunk{}, stream.client.decorateError(err)
		}
		if !emit {
			continue
		}
		if chunk.Done {
			if err := stream.complete(); err != nil {
				stream.invalidate()
				return damodel.Chunk{}, err
			}
		}
		return chunk, nil
	}
}

func (stream *websocketResponseStream) complete() error {
	if stream.parser.completed == nil {
		return ErrIncompleteStream
	}
	response, err := normalizeResponse(*stream.parser.completed, nil)
	if err != nil {
		return err
	}
	responseItems, err := inputItems(response.Message)
	if err != nil {
		return err
	}
	prefixItems := make([]any, 0, len(stream.request.Input)+len(responseItems))
	prefixItems = append(prefixItems, stream.request.Input...)
	prefixItems = append(prefixItems, responseItems...)
	prefix, err := canonicalInput(prefixItems)
	if err != nil {
		return err
	}
	var continuation *responsesContinuation
	if stream.parser.completed.ID != "" {
		continuation = &responsesContinuation{responseID: stream.parser.completed.ID, prefix: prefix}
	}
	stream.releaseOnce.Do(func() {
		stream.pool.release(stream.connection, continuation, false)
	})
	return nil
}

func (stream *websocketResponseStream) invalidate() {
	stream.parser.done = true
	stream.releaseOnce.Do(func() {
		stream.pool.release(stream.connection, nil, true)
	})
}

func (stream *websocketResponseStream) Close() error {
	stream.parser.done = true
	stream.invalidate()
	return nil
}

var _ damodel.Stream = (*websocketResponseStream)(nil)

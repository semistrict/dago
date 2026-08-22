package modeltest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
)

const (
	defaultPredictableHistoryLimit = 10
	defaultPredictableResponse     = "No predictable response is configured for that input."
)

// PredictableOptions configures a reusable prompt-driven model double.
//
// A nil Profile uses a capable offline test profile. A non-nil Profile is used
// exactly as supplied so tests can exercise capability-gated behavior.
type PredictableOptions struct {
	Profile         *damodel.Profile
	DefaultResponse string
	HistoryLimit    int
	// ResponseDelay is cancellable and works inside testing/synctest bubbles,
	// allowing realistic model latency without slowing tests.
	ResponseDelay time.Duration
}

// Predictable is a prompt-driven model double for agent examples, integration
// tests, and browser suites. Unlike Scripted, it does not consume a finite list
// of calls: the same small command language can serve any number of requests.
//
// Supported final-human-message forms are:
//   - hello: return a stable greeting
//   - echo: TEXT: return TEXT
//   - bash: COMMAND: call the bash tool with {"command": COMMAND}
//   - tool: NAME JSON: call NAME with the supplied JSON arguments
//   - think: TEXT: return reasoning followed by a stable answer
//   - structured: JSON: return provider-neutral structured output
//   - delay: DURATION: wait for a Go duration (or a bare number of seconds)
//   - error: TEXT: return a stable error
//
// A final tool-result message produces "Done.". Unknown input receives the
// configured default response. Requests and returned history are cloned at the
// boundary so callers cannot mutate recorded evidence.
type Predictable struct {
	mu              sync.Mutex
	profile         damodel.Profile
	defaultResponse string
	historyLimit    int
	responseDelay   time.Duration
	nextID          uint64
	recentRequests  []damodel.Request
}

// NewPredictable constructs a prompt-driven deterministic model.
func NewPredictable(options PredictableOptions) *Predictable {
	if options.HistoryLimit < 0 || options.ResponseDelay < 0 {
		panic("predictable model history limit and response delay cannot be negative")
	}
	profile := damodel.Profile{
		Provider:          "builtin",
		Model:             "predictable-v1",
		ContextWindow:     200_000,
		MaxOutputTokens:   8_192,
		ToolCalling:       true,
		ParallelToolCalls: true,
		StructuredOutput:  true,
		NativeStreaming:   true,
	}
	if options.Profile != nil {
		profile = *options.Profile
	}
	defaultResponse := options.DefaultResponse
	if defaultResponse == "" {
		defaultResponse = defaultPredictableResponse
	}
	historyLimit := options.HistoryLimit
	if historyLimit == 0 {
		historyLimit = defaultPredictableHistoryLimit
	}
	return &Predictable{
		profile:         profile,
		defaultResponse: defaultResponse,
		historyLimit:    historyLimit,
		responseDelay:   options.ResponseDelay,
	}
}

func (predictable *Predictable) Profile() damodel.Profile { return predictable.profile }

func (predictable *Predictable) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	id, delay := predictable.record(request)
	if err := waitPredictable(ctx, delay); err != nil {
		return damodel.Response{}, err
	}

	last, ok := lastConversationMessage(request.Messages)
	if ok && last.Role == damessage.RoleTool {
		return predictable.textResponse(id, "Done.", request), nil
	}
	input := ""
	if ok && last.Role == damessage.RoleHuman {
		input = strings.TrimSpace(last.TextContent())
	}

	switch input {
	case "hello":
		return predictable.textResponse(id, "Well, hi there!", request), nil
	case "Hello":
		return predictable.textResponse(id, "Hello! How can I help you today?", request), nil
	}
	if text, found := strings.CutPrefix(input, "echo: "); found {
		return predictable.textResponse(id, text, request), nil
	}
	if command, found := strings.CutPrefix(input, "bash: "); found {
		arguments, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			return damodel.Response{}, err
		}
		return predictable.toolResponse(id, "bash", arguments, request), nil
	}
	if invocation, found := strings.CutPrefix(input, "tool: "); found {
		name, arguments, err := parsePredictableTool(invocation)
		if err != nil {
			return damodel.Response{}, err
		}
		return predictable.toolResponse(id, name, arguments, request), nil
	}
	if reasoning, found := strings.CutPrefix(input, "think: "); found {
		response := predictable.textResponse(id, "I've considered my approach.", request)
		response.Message.Content = append([]damessage.ContentBlock{{
			Type: damessage.BlockReasoning, Reasoning: reasoning,
		}}, response.Message.Content...)
		return response, nil
	}
	if value, found := strings.CutPrefix(input, "structured: "); found {
		structured := json.RawMessage(value)
		if !json.Valid(structured) {
			return damodel.Response{}, fmt.Errorf("predictable model: structured output is not valid JSON")
		}
		response := predictable.textResponse(id, "Structured response.", request)
		response.Structured = append(json.RawMessage(nil), structured...)
		return response, nil
	}
	if value, found := strings.CutPrefix(input, "delay: "); found {
		duration, err := parsePredictableDuration(strings.TrimSpace(value))
		if err != nil {
			return damodel.Response{}, err
		}
		if err := waitPredictable(ctx, duration); err != nil {
			return damodel.Response{}, err
		}
		return predictable.textResponse(id, "Delayed for "+value, request), nil
	}
	if text, found := strings.CutPrefix(input, "error: "); found {
		return damodel.Response{}, fmt.Errorf("predictable error: %s", text)
	}
	return predictable.textResponse(id, predictable.defaultResponse, request), nil
}

func (predictable *Predictable) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	response, err := predictable.Invoke(ctx, request)
	if err != nil {
		return nil, err
	}
	return &predictableStream{ctx: ctx, chunk: damodel.Chunk{
		MessageDelta: response.Message.Clone(),
		Structured:   append(json.RawMessage(nil), response.Structured...),
		Done:         true,
	}}, nil
}

// CountTokens returns the predictable four-characters-per-token estimate used
// in response usage records. It intentionally favors repeatability over model
// tokenizer fidelity.
func (predictable *Predictable) CountTokens(ctx context.Context, messages []damessage.Message) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	characters := 0
	for _, item := range messages {
		characters += messageCharacters(item)
	}
	return estimatedTokens(characters), nil
}

// RecentRequests returns isolated copies in invocation order.
func (predictable *Predictable) RecentRequests() []damodel.Request {
	predictable.mu.Lock()
	defer predictable.mu.Unlock()
	result := make([]damodel.Request, len(predictable.recentRequests))
	for index := range predictable.recentRequests {
		result[index] = cloneModelRequest(predictable.recentRequests[index])
	}
	return result
}

// LastRequest returns an isolated copy of the most recent invocation.
func (predictable *Predictable) LastRequest() (damodel.Request, bool) {
	predictable.mu.Lock()
	defer predictable.mu.Unlock()
	if len(predictable.recentRequests) == 0 {
		return damodel.Request{}, false
	}
	return cloneModelRequest(predictable.recentRequests[len(predictable.recentRequests)-1]), true
}

// ClearRequests clears captured request history without changing the response
// sequence or configuration.
func (predictable *Predictable) ClearRequests() {
	predictable.mu.Lock()
	predictable.recentRequests = nil
	predictable.mu.Unlock()
}

// SetResponseDelay changes the cancellable delay applied to every invocation.
func (predictable *Predictable) SetResponseDelay(delay time.Duration) {
	predictable.mu.Lock()
	predictable.responseDelay = delay
	predictable.mu.Unlock()
}

func (predictable *Predictable) record(request damodel.Request) (uint64, time.Duration) {
	predictable.mu.Lock()
	defer predictable.mu.Unlock()
	predictable.nextID++
	predictable.recentRequests = append(predictable.recentRequests, cloneModelRequest(request))
	if predictable.historyLimit > 0 && len(predictable.recentRequests) > predictable.historyLimit {
		predictable.recentRequests = predictable.recentRequests[len(predictable.recentRequests)-predictable.historyLimit:]
	}
	return predictable.nextID, predictable.responseDelay
}

func (predictable *Predictable) textResponse(id uint64, text string, request damodel.Request) damodel.Response {
	result := damessage.Assistant(text)
	result.ID = fmt.Sprintf("predictable-%d", id)
	result.Usage = predictableUsage(request, result)
	return damodel.Response{Message: result}
}

func (predictable *Predictable) toolResponse(id uint64, name string, arguments json.RawMessage, request damodel.Request) damodel.Response {
	result := damessage.Assistant("I'll call " + name + ".")
	result.ID = fmt.Sprintf("predictable-%d", id)
	result.ToolCalls = []damessage.ToolCall{{
		ID:        fmt.Sprintf("predictable-tool-%d", id),
		Name:      name,
		Arguments: append(json.RawMessage(nil), arguments...),
	}}
	result.Usage = predictableUsage(request, result)
	return damodel.Response{Message: result}
}

func predictableUsage(request damodel.Request, response damessage.Message) *damessage.Usage {
	inputCharacters := 0
	for _, item := range request.Messages {
		inputCharacters += messageCharacters(item)
	}
	for _, definition := range request.Tools {
		inputCharacters += len(definition.Name) + len(definition.Description) + len(definition.InputSchema)
	}
	outputCharacters := messageCharacters(response)
	inputTokens := estimatedTokens(inputCharacters)
	outputTokens := estimatedTokens(outputCharacters)
	return &damessage.Usage{
		InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens,
	}
}

func messageCharacters(item damessage.Message) int {
	characters := 0
	for _, block := range item.Content {
		characters += len(block.Text) + len(block.Reasoning) + len(block.URL) + len(block.Data) + len(block.NonStandard)
	}
	for _, call := range item.ToolCalls {
		characters += len(call.Name) + len(call.Arguments)
	}
	return characters
}

func estimatedTokens(characters int) int {
	if characters <= 0 {
		return 1
	}
	return (characters + 3) / 4
}

func parsePredictableTool(value string) (string, json.RawMessage, error) {
	name, arguments, found := strings.Cut(strings.TrimSpace(value), " ")
	if !found || strings.TrimSpace(name) == "" || strings.TrimSpace(arguments) == "" {
		return "", nil, fmt.Errorf("predictable model: tool input must be NAME JSON")
	}
	raw := json.RawMessage(strings.TrimSpace(arguments))
	if !json.Valid(raw) {
		return "", nil, fmt.Errorf("predictable model: tool arguments are not valid JSON")
	}
	return name, append(json.RawMessage(nil), raw...), nil
}

func parsePredictableDuration(value string) (time.Duration, error) {
	if duration, err := time.ParseDuration(value); err == nil {
		if duration < 0 {
			return 0, fmt.Errorf("predictable model: delay cannot be negative")
		}
		return duration, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds < 0 {
		return 0, fmt.Errorf("predictable model: delay %q is invalid", value)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func waitPredictable(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func lastConversationMessage(messages []damessage.Message) (damessage.Message, bool) {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != damessage.RoleSystem && messages[index].Role != damessage.RoleRemove {
			return messages[index], true
		}
	}
	return damessage.Message{}, false
}

func cloneModelRequest(request damodel.Request) damodel.Request {
	copy := request
	if request.SystemMessage != nil {
		value := request.SystemMessage.Clone()
		copy.SystemMessage = &value
	}
	copy.Messages = make([]damessage.Message, len(request.Messages))
	for index := range request.Messages {
		copy.Messages[index] = request.Messages[index].Clone()
	}
	copy.Tools = append([]datool.Definition(nil), request.Tools...)
	for index := range copy.Tools {
		copy.Tools[index].InputSchema = append(json.RawMessage(nil), request.Tools[index].InputSchema...)
		copy.Tools[index].Extra = cloneRawMap(request.Tools[index].Extra)
	}
	if request.ToolChoice != nil {
		value := *request.ToolChoice
		copy.ToolChoice = &value
	}
	if request.ResponseFormat != nil {
		value := *request.ResponseFormat
		value.Schema = append(json.RawMessage(nil), request.ResponseFormat.Schema...)
		copy.ResponseFormat = &value
	}
	if request.PromptCache != nil {
		value := *request.PromptCache
		copy.PromptCache = &value
	}
	copy.Skills = append([]damodel.Skill(nil), request.Skills...)
	if request.Metadata != nil {
		copy.Metadata = make(map[string]json.RawMessage, len(request.Metadata))
		for key, value := range request.Metadata {
			copy.Metadata[key] = append(json.RawMessage(nil), value...)
		}
	}
	copy.Tags = append([]string(nil), request.Tags...)
	copy.Stop = append([]string(nil), request.Stop...)
	return copy
}

func cloneRawMap(value map[string]json.RawMessage) map[string]json.RawMessage {
	if value == nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(value))
	for key, item := range value {
		result[key] = append(json.RawMessage(nil), item...)
	}
	return result
}

type predictableStream struct {
	mu     sync.Mutex
	ctx    context.Context
	chunk  damodel.Chunk
	sent   bool
	closed bool
}

func (stream *predictableStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return damodel.Chunks(stream.ctx, stream)
}

func (stream *predictableStream) Next(ctx context.Context) (damodel.Chunk, error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.closed || stream.sent {
		return damodel.Chunk{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return damodel.Chunk{}, err
	}
	stream.sent = true
	chunk := stream.chunk
	chunk.MessageDelta = stream.chunk.MessageDelta.Clone()
	chunk.Structured = append(json.RawMessage(nil), stream.chunk.Structured...)
	return chunk, nil
}

func (stream *predictableStream) Close() error {
	stream.mu.Lock()
	stream.closed = true
	stream.mu.Unlock()
	return nil
}

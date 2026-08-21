package dacost

import (
	"context"
	"io"
	"iter"
	"math"
	"reflect"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
)

// NormalizeUsage decorates a model so response usage always carries configured
// provider/model fallbacks and streamed usage is cumulative. This keeps the
// final checkpoint accurate when providers report signed corrections or name a
// model only on a terminal chunk. Static nil input panics.
func NormalizeUsage(model damodel.Chat) damodel.Chat {
	if nilChat(model) {
		panic("dacost: model is nil")
	}
	base := &usageChat{chat: model}
	_, binder := model.(damodel.Binder)
	_, counter := model.(damodel.TokenCounter)
	switch {
	case binder && counter:
		return usageFullChat{usageChat: base}
	case binder:
		return usageBinderChat{usageChat: base}
	case counter:
		return usageCounterChat{usageChat: base}
	default:
		return base
	}
}

func nilChat(model damodel.Chat) bool {
	if model == nil {
		return true
	}
	value := reflect.ValueOf(model)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

type usageChat struct{ chat damodel.Chat }

func (model *usageChat) Profile() damodel.Profile { return model.chat.Profile() }

func (model *usageChat) Close() error {
	if closer, ok := model.chat.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (model *usageChat) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	response, err := model.chat.Invoke(ctx, request)
	if err != nil {
		return response, err
	}
	applyUsageFallback(&response.Message, model.Profile())
	return response, nil
}

func (model *usageChat) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	stream, err := model.chat.Stream(ctx, request)
	if err != nil {
		return nil, err
	}
	return &usageStream{stream: stream, profile: model.Profile(), ctx: ctx}, nil
}

type usageBinderChat struct{ *usageChat }

func (model usageBinderChat) BindTools(tools []datool.Definition) (damodel.Chat, error) {
	bound, err := model.chat.(damodel.Binder).BindTools(tools)
	if err != nil {
		return nil, err
	}
	return NormalizeUsage(bound), nil
}

type usageCounterChat struct{ *usageChat }

func (model usageCounterChat) CountTokens(ctx context.Context, messages []damessage.Message) (int, error) {
	return model.chat.(damodel.TokenCounter).CountTokens(ctx, messages)
}

type usageFullChat struct{ *usageChat }

func (model usageFullChat) BindTools(tools []datool.Definition) (damodel.Chat, error) {
	bound, err := model.chat.(damodel.Binder).BindTools(tools)
	if err != nil {
		return nil, err
	}
	return NormalizeUsage(bound), nil
}

func (model usageFullChat) CountTokens(ctx context.Context, messages []damessage.Message) (int, error) {
	return model.chat.(damodel.TokenCounter).CountTokens(ctx, messages)
}

type usageStream struct {
	stream  damodel.Stream
	profile damodel.Profile
	ctx     context.Context
	usage   *damessage.Usage
}

func (stream *usageStream) Next(ctx context.Context) (damodel.Chunk, error) {
	chunk, err := stream.stream.Next(ctx)
	if err != nil {
		return chunk, err
	}
	if chunk.MessageDelta.Usage == nil {
		return chunk, nil
	}
	merged, ok := mergeStreamUsage(stream.usage, chunk.MessageDelta.Usage)
	if !ok {
		// Accounting metadata must never fail an otherwise valid model stream.
		chunk.MessageDelta.Usage = nil
		return chunk, nil
	}
	applyUsageFallbackValue(&merged, stream.profile)
	stream.usage = &merged
	copy := cloneUsage(merged)
	chunk.MessageDelta.Usage = &copy
	return chunk, nil
}

func (stream *usageStream) Close() error { return stream.stream.Close() }

func (stream *usageStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return damodel.Chunks(stream.ctx, stream)
}

func applyUsageFallback(message *damessage.Message, profile damodel.Profile) {
	if message == nil || message.Usage == nil {
		return
	}
	usage := cloneUsage(*message.Usage)
	applyUsageFallbackValue(&usage, profile)
	message.Usage = &usage
}

func applyUsageFallbackValue(usage *damessage.Usage, profile damodel.Profile) {
	if usage.Provider == "" {
		usage.Provider = profile.Provider
	}
	if usage.Model == "" {
		usage.Model = profile.Model
	}
}

func mergeStreamUsage(current, next *damessage.Usage) (damessage.Usage, bool) {
	if next == nil {
		if current == nil {
			return damessage.Usage{}, true
		}
		return cloneUsage(*current), true
	}
	if current == nil {
		return cloneUsage(*next), true
	}
	result := cloneUsage(*current)
	var ok bool
	if result.InputTokens, ok = addUsageInt(result.InputTokens, next.InputTokens); !ok {
		return damessage.Usage{}, false
	}
	if result.OutputTokens, ok = addUsageInt(result.OutputTokens, next.OutputTokens); !ok {
		return damessage.Usage{}, false
	}
	if result.TotalTokens, ok = addUsageInt(result.TotalTokens, next.TotalTokens); !ok {
		return damessage.Usage{}, false
	}
	if result.InputDetails, ok = mergeUsageDetails(result.InputDetails, next.InputDetails); !ok {
		return damessage.Usage{}, false
	}
	if result.OutputDetails, ok = mergeUsageDetails(result.OutputDetails, next.OutputDetails); !ok {
		return damessage.Usage{}, false
	}
	if next.Provider != "" {
		result.Provider = next.Provider
	}
	if next.Model != "" {
		result.Model = next.Model
	}
	if next.URL != "" {
		result.URL = next.URL
	}
	if next.CostUSD != 0 {
		result.CostUSD += next.CostUSD
		if math.IsNaN(result.CostUSD) || math.IsInf(result.CostUSD, 0) {
			return damessage.Usage{}, false
		}
	}
	if result.StartedAt.IsZero() || (!next.StartedAt.IsZero() && next.StartedAt.Before(result.StartedAt)) {
		result.StartedAt = next.StartedAt
	}
	if next.FinishedAt.After(result.FinishedAt) {
		result.FinishedAt = next.FinishedAt
	}
	return result, true
}

func addUsageInt(left, right int) (int, bool) {
	if right > 0 && left > math.MaxInt-right || right < 0 && left < math.MinInt-right {
		return 0, false
	}
	return left + right, true
}

func mergeUsageDetails(left, right map[string]int) (map[string]int, bool) {
	if len(left)+len(right) > 512 {
		return nil, false
	}
	result := make(map[string]int, len(left)+len(right))
	for key, value := range left {
		result[key] = value
	}
	for key, value := range right {
		merged, ok := addUsageInt(result[key], value)
		if !ok {
			return nil, false
		}
		result[key] = merged
	}
	return result, true
}

var _ damodel.Chat = (*usageChat)(nil)
var _ damodel.Binder = usageBinderChat{}
var _ damodel.TokenCounter = usageCounterChat{}
var _ damodel.Stream = (*usageStream)(nil)

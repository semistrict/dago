package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"unicode/utf8"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
)

const retainedCompactionTokenBudget = 64_000

func (client *Client) shouldCompact(payload responsesRequest) bool {
	if client.options.ServerCompaction == nil || !*client.options.ServerCompaction {
		return false
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	return (len(encoded)+3)/4 >= client.options.CompactionThreshold
}

func isCompactionType(value string) bool {
	return value == "compaction" || value == "compaction_summary" || value == "context_compaction"
}

func (client *Client) compactInput(ctx context.Context, payload responsesRequest) ([]any, json.RawMessage, error) {
	compactionPayload := payload
	compactionPayload.Input = append(append([]any(nil), payload.Input...), map[string]any{"type": "compaction_trigger"})
	compactionPayload.Text = nil
	compactionPayload.ToolChoice = "auto"
	stream, err := client.streamPayload(ctx, compactionPayload)
	if err != nil {
		return nil, nil, err
	}
	defer stream.Close()
	var compacted json.RawMessage
	count := 0
	done := false
	for chunk, nextErr := range stream.Chunks() {
		if nextErr != nil {
			return nil, nil, nextErr
		}
		done = done || chunk.Done
		for _, block := range chunk.MessageDelta.Content {
			if block.Type != damessage.BlockNonStandard {
				continue
			}
			var item struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(block.NonStandard, &item) == nil && isCompactionType(item.Type) {
				count++
				if len(compacted) == 0 {
					compacted = append(json.RawMessage(nil), block.NonStandard...)
				}
			}
		}
	}
	if !done {
		return nil, nil, ErrIncompleteStream
	}
	if count != 1 {
		return nil, nil, fmt.Errorf("openai: compaction expected exactly one compaction item, got %d", count)
	}
	retained := retainedCompactionInput(payload.Input)
	retained = append(retained, append(json.RawMessage(nil), compacted...))
	return retained, compacted, nil
}

func retainedCompactionInput(input []any) []any {
	return retainedCompactionInputWithBudget(input, retainedCompactionTokenBudget)
}

func retainedCompactionInputWithBudget(input []any, tokenBudget int) []any {
	candidates := make([]any, 0, len(input))
	for _, item := range input {
		encoded, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var value struct {
			Role string `json:"role"`
		}
		// System instructions live on the request rather than in input, and this
		// message model has no separate assistant-commentary phase. Retain real
		// user messages and discard final answers, tool state, and older compactions.
		if json.Unmarshal(encoded, &value) == nil && value.Role == "user" {
			candidates = append(candidates, item)
		}
	}
	remaining := tokenBudget
	reversed := make([]any, 0, len(candidates))
	for index := len(candidates) - 1; index >= 0 && remaining > 0; index-- {
		item := candidates[index]
		tokens := compactionMessageTextTokens(item)
		if tokens < 1 {
			tokens = 1
		}
		if tokens <= remaining {
			reversed = append(reversed, item)
			remaining -= tokens
			continue
		}
		if truncated := truncateCompactionMessage(item, remaining); truncated != nil {
			reversed = append(reversed, truncated)
		}
		remaining = 0
	}
	retained := make([]any, len(reversed))
	for index := range reversed {
		retained[len(reversed)-1-index] = reversed[index]
	}
	return retained
}

func compactionMessageTextTokens(item any) int {
	encoded, err := json.Marshal(item)
	if err != nil {
		return 0
	}
	var message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(encoded, &message) != nil {
		return 0
	}
	tokens := 0
	for _, content := range message.Content {
		if content.Type == "input_text" || content.Type == "output_text" {
			tokens += (utf8.RuneCountInString(content.Text) + 3) / 4
		}
	}
	return tokens
}

func truncateCompactionMessage(item any, tokenBudget int) any {
	encoded, err := json.Marshal(item)
	if err != nil {
		return nil
	}
	var message map[string]any
	if json.Unmarshal(encoded, &message) != nil {
		return nil
	}
	content, ok := message["content"].([]any)
	if !ok {
		return nil
	}
	remainingRunes := tokenBudget * 4
	truncated := make([]any, 0, len(content))
	for _, raw := range content {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typeName, _ := part["type"].(string)
		if typeName != "input_text" && typeName != "output_text" {
			truncated = append(truncated, part)
			continue
		}
		if remainingRunes == 0 {
			continue
		}
		value, _ := part["text"].(string)
		runes := []rune(value)
		if len(runes) > remainingRunes {
			runes = runes[:remainingRunes]
		}
		part["text"] = string(runes)
		remainingRunes -= len(runes)
		if len(runes) > 0 {
			truncated = append(truncated, part)
		}
	}
	if len(truncated) == 0 {
		return nil
	}
	message["content"] = truncated
	return message
}

type compactionStateStream struct {
	ctx        context.Context
	inner      damodel.Stream
	compaction json.RawMessage
	emitted    bool
}

func newCompactionStateStream(ctx context.Context, inner damodel.Stream, compaction json.RawMessage) damodel.Stream {
	return &compactionStateStream{ctx: ctx, inner: inner, compaction: append(json.RawMessage(nil), compaction...)}
}

func (stream *compactionStateStream) Next(ctx context.Context) (damodel.Chunk, error) {
	if !stream.emitted {
		stream.emitted = true
		var item struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(stream.compaction, &item)
		return damodel.Chunk{MessageDelta: damessage.Message{Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{{
			Type: damessage.BlockNonStandard, ID: item.ID, NonStandard: append(json.RawMessage(nil), stream.compaction...),
		}}}}, nil
	}
	return stream.inner.Next(ctx)
}

func (stream *compactionStateStream) Close() error { return stream.inner.Close() }

func (stream *compactionStateStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return damodel.Chunks(stream.ctx, stream)
}

var _ damodel.Stream = (*compactionStateStream)(nil)

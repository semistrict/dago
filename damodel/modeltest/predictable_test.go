package modeltest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
)

func TestPredictableTextAndToolPatterns(t *testing.T) {
	predictable := NewPredictable(PredictableOptions{})

	response, err := predictable.Invoke(context.Background(), damodel.Request{
		Messages: []damessage.Message{damessage.Human("echo: hello world")},
	})
	if err != nil || response.Message.TextContent() != "hello world" {
		t.Fatalf("echo Invoke() = %#v, %v", response, err)
	}
	if response.Message.ID != "predictable-1" || response.Message.Usage == nil {
		t.Fatalf("echo metadata = %#v", response.Message)
	}

	response, err = predictable.Invoke(context.Background(), damodel.Request{
		Messages: []damessage.Message{damessage.Human(`tool: lookup {"query":"dago"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Name != "lookup" {
		t.Fatalf("tool response = %#v", response.Message)
	}
	var arguments map[string]string
	if err := json.Unmarshal(response.Message.ToolCalls[0].Arguments, &arguments); err != nil {
		t.Fatal(err)
	}
	if arguments["query"] != "dago" || response.Message.ToolCalls[0].ID != "predictable-tool-2" {
		t.Fatalf("tool call = %#v", response.Message.ToolCalls[0])
	}

	response, err = predictable.Invoke(context.Background(), damodel.Request{
		Messages: []damessage.Message{damessage.Tool("predictable-tool-2", "result")},
	})
	if err != nil || response.Message.TextContent() != "Done." {
		t.Fatalf("tool result Invoke() = %#v, %v", response, err)
	}
}

func TestNewPredictableRejectsNegativeLimits(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("negative history limit did not panic")
		}
	}()
	NewPredictable(PredictableOptions{HistoryLimit: -1})
}

func TestPredictableReasoningStructuredAndDefault(t *testing.T) {
	predictable := NewPredictable(PredictableOptions{DefaultResponse: "fallback"})

	response, err := predictable.Invoke(context.Background(), damodel.Request{
		Messages: []damessage.Message{damessage.Human("think: inspect first")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Message.Content) != 2 || response.Message.Content[0].Reasoning != "inspect first" {
		t.Fatalf("thinking response = %#v", response.Message.Content)
	}

	response, err = predictable.Invoke(context.Background(), damodel.Request{
		Messages: []damessage.Message{damessage.Human(`structured: {"answer":42}`)},
	})
	if err != nil || string(response.Structured) != `{"answer":42}` {
		t.Fatalf("structured response = %#v, %v", response, err)
	}

	response, err = predictable.Invoke(context.Background(), damodel.Request{
		Messages: []damessage.Message{damessage.Human("unknown")},
	})
	if err != nil || response.Message.TextContent() != "fallback" {
		t.Fatalf("default response = %#v, %v", response, err)
	}
}

func TestPredictableErrorsAndCancellation(t *testing.T) {
	predictable := NewPredictable(PredictableOptions{})
	_, err := predictable.Invoke(context.Background(), damodel.Request{
		Messages: []damessage.Message{damessage.Human("error: expected")},
	})
	if err == nil || err.Error() != "predictable error: expected" {
		t.Fatalf("error Invoke() = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	predictable.SetResponseDelay(time.Hour)
	_, err = predictable.Invoke(ctx, damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Invoke() = %v", err)
	}
}

func TestPredictableResponseDelayWithSynctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		predictable := NewPredictable(PredictableOptions{ResponseDelay: 2 * time.Second})
		started := time.Now()
		response, err := predictable.Invoke(t.Context(), damodel.Request{
			Messages: []damessage.Message{damessage.Human("hello")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.Message.TextContent() != "Well, hi there!" {
			t.Fatalf("response = %#v", response.Message)
		}
		if elapsed := time.Since(started); elapsed != 2*time.Second {
			t.Fatalf("elapsed = %s, want 2s", elapsed)
		}
	})
}

func TestPredictableHistoryIsBoundedAndIsolated(t *testing.T) {
	predictable := NewPredictable(PredictableOptions{HistoryLimit: 2})
	request := damodel.Request{Messages: []damessage.Message{damessage.Human("echo: one")}}
	for _, text := range []string{"echo: one", "echo: two", "echo: three"} {
		request.Messages[0] = damessage.Human(text)
		if _, err := predictable.Invoke(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}

	history := predictable.RecentRequests()
	if len(history) != 2 || history[0].Messages[0].TextContent() != "echo: two" {
		t.Fatalf("RecentRequests() = %#v", history)
	}
	history[0].Messages[0].Content[0].Text = "mutated"
	last, ok := predictable.LastRequest()
	if !ok || last.Messages[0].TextContent() != "echo: three" {
		t.Fatalf("LastRequest() = %#v, %v", last, ok)
	}
	if predictable.RecentRequests()[0].Messages[0].TextContent() != "echo: two" {
		t.Fatal("returned history mutated captured request")
	}
	predictable.ClearRequests()
	if _, ok := predictable.LastRequest(); ok {
		t.Fatal("LastRequest() remained populated after ClearRequests")
	}
}

func TestPredictableStreamAndTokenCount(t *testing.T) {
	predictable := NewPredictable(PredictableOptions{})
	stream, err := predictable.Stream(context.Background(), damodel.Request{
		Messages: []damessage.Message{damessage.Human("echo: streamed")},
	})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := stream.Next(context.Background())
	if err != nil || !chunk.Done || chunk.MessageDelta.TextContent() != "streamed" {
		t.Fatalf("Next() = %#v, %v", chunk, err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("final Next() = %v", err)
	}

	tokens, err := predictable.CountTokens(context.Background(), []damessage.Message{damessage.Human("12345678")})
	if err != nil || tokens != 2 {
		t.Fatalf("CountTokens() = %d, %v", tokens, err)
	}
}

func TestPredictableRejectsMalformedCommands(t *testing.T) {
	predictable := NewPredictable(PredictableOptions{})
	for _, input := range []string{"tool: missing-json", "tool: lookup {", "structured: {", "delay: later", "delay: -1s"} {
		t.Run(input, func(t *testing.T) {
			_, err := predictable.Invoke(context.Background(), damodel.Request{
				Messages: []damessage.Message{damessage.Human(input)},
			})
			if err == nil {
				t.Fatalf("Invoke(%q) succeeded", input)
			}
		})
	}
}

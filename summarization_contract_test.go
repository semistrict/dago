package dago

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	"github.com/semistrict/dago/checkpoint"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
	"github.com/semistrict/dago/state"
	"github.com/semistrict/dago/tool"
)

type failingHistoryBackend struct{ backend.Backend }

func (failingHistoryBackend) Write(context.Context, string, string) (backend.WriteResult, error) {
	return backend.WriteResult{}, errors.New("history unavailable")
}

type failingMediaBackend struct{ backend.Backend }

func (failingMediaBackend) Upload(_ context.Context, values []backend.Upload) []backend.UploadResult {
	result := make([]backend.UploadResult, len(values))
	for index, value := range values {
		result[index] = backend.UploadResult{Path: value.Path, Error: "upload unavailable"}
	}
	return result
}

func TestHistoryOffloadAppendsPerThreadAndFiltersSyntheticSummaries(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	options := SummarizationOptions{Backend: memory, HistoryRoot: "/conversation_history"}
	previous := message.Human("old synthetic summary")
	previous.Metadata = map[string]json.RawMessage{"lc_source": json.RawMessage(`"summarization"`)}
	first := offloadConversationHistory(context.Background(), options, agent.Runtime{
		Config: checkpoint.Config{ThreadID: "thread", CheckpointID: "first"},
	}, state.Values{}, []message.Message{previous, message.Human("new facts")})
	if first.Err != nil || first.Path != "/conversation_history/thread.md" {
		t.Fatalf("first offload = %#v", first)
	}
	second := offloadConversationHistory(context.Background(), options, agent.Runtime{
		Config: checkpoint.Config{ThreadID: "thread", CheckpointID: "second"},
	}, state.Values{}, []message.Message{message.Human("later facts")})
	if second.Err != nil || second.Path != first.Path {
		t.Fatalf("second offload = %#v", second)
	}
	read, err := memory.Read(context.Background(), first.Path, 0, 1_000)
	if err != nil || read.Data == nil {
		t.Fatalf("history read = %#v, %v", read, err)
	}
	content := read.Data.Content
	if strings.Contains(content, "old synthetic summary") || !strings.Contains(content, "new facts") || !strings.Contains(content, "later facts") || !strings.Contains(content, "Summarized at first") || !strings.Contains(content, "Summarized at second") {
		t.Fatalf("history = %q", content)
	}
}

func TestSummarizationContinuesWhenHistoryOffloadFails(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	summaryModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("durable facts")}})
	mainModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if !strings.Contains(request.Messages[0].TextContent(), "durable facts") || strings.Contains(request.Messages[0].TextContent(), "has been saved") {
			return errors.New("invalid degraded summary")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	middleware, err := SummarizationMiddleware(SummarizationOptions{
		Model: summaryModel, Backend: failingHistoryBackend{Backend: memory}, TriggerTokens: 1, KeepMessages: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := middleware.WrapModelCall(context.Background(), agent.ModelRequest{
		Model: mainModel, Messages: []message.Message{message.Human("old"), message.Assistant("recent")}, State: state.Values{},
	}, func(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
		return agent.ModelResponse{Messages: []message.Message{message.Assistant("done")}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, summary, _, ok := decodeSummarizationEvent(response.Update[summarizationEventKey])
	if !ok {
		t.Fatalf("summarization event = %#v", response.Update[summarizationEventKey])
	}
	if len(summary.Metadata["history_offload_error"]) == 0 || string(summary.Metadata["lc_source"]) != `"summarization"` {
		t.Fatalf("summary metadata = %#v", summary.Metadata)
	}
}

func TestMediaOffloadFailureUsesBoundedPlaceholder(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	summaryModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "failed_to_offload") {
			return errors.New("media placeholder missing")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("summary")}})
	mainModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("done")}})
	middleware, err := SummarizationMiddleware(SummarizationOptions{
		Model: summaryModel, Backend: failingMediaBackend{Backend: memory}, TriggerTokens: 1, KeepMessages: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.New(agent.Options{Model: mainModel, Middleware: []agent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	old := message.Message{Role: message.RoleHuman, Content: []message.ContentBlock{{Type: message.BlockImage, MIMEType: "image/png", Data: []byte("secret-inline-data")}}}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{old, message.Human("recent")}}); err != nil {
		t.Fatal(err)
	}
}

func TestArgumentTruncationPreservesSchemasAndUnrelatedTools(t *testing.T) {
	write := message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{
		{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/x","content":"unchanged payload"}`)},
		{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/x","content":"abcdefghijklmnop"}`)},
		{ID: "edit", Name: "edit_file", Arguments: json.RawMessage(`{"file_path":"/x","old_string":"abcdefghijklmnop","new_string":"qrstuvwxyzabcdef"}`)},
	}}
	messages := []message.Message{write, message.Human("middle"), message.Assistant("recent")}
	update := truncateOldToolArguments(messages, &ArgumentTruncationOptions{
		TriggerMessages: 3, KeepMessages: 1, MaxLength: 10, PreviewLength: 3, TruncationText: "[cut]",
	})
	overwrite, ok := update[agent.MessagesKey].(state.Overwrite)
	if !ok {
		t.Fatalf("truncation update = %#v", update)
	}
	cleaned, err := featureMessages(overwrite.Value)
	if err != nil {
		t.Fatal(err)
	}
	calls := cleaned[0].ToolCalls
	if string(calls[0].Arguments) != string(write.ToolCalls[0].Arguments) {
		t.Fatalf("read_file arguments changed: %s", calls[0].Arguments)
	}
	for index, fields := range map[int][]string{1: {"content"}, 2: {"old_string", "new_string"}} {
		var arguments map[string]any
		if err := json.Unmarshal(calls[index].Arguments, &arguments); err != nil {
			t.Fatalf("call %d arguments are no longer an object: %v", index, err)
		}
		if arguments["file_path"] != "/x" {
			t.Fatalf("call %d lost file_path: %#v", index, arguments)
		}
		for _, field := range fields {
			if arguments[field] != "abc[cut]" && arguments[field] != "qrs[cut]" {
				t.Fatalf("call %d field %s = %#v", index, field, arguments[field])
			}
		}
	}
	if update := truncateOldToolArguments(messages[:2], &ArgumentTruncationOptions{TriggerMessages: 3, KeepMessages: 1, MaxLength: 1, PreviewLength: 1, TruncationText: "..."}); update != nil {
		t.Fatalf("truncated below trigger: %#v", update)
	}
}

func TestSummarizationSupportsMessageTriggersAndTokenKeepWindows(t *testing.T) {
	messages := []message.Message{
		message.Human(strings.Repeat("a", 80)), message.Assistant(strings.Repeat("b", 80)),
		message.Human(strings.Repeat("c", 80)), message.Assistant(strings.Repeat("d", 80)),
	}
	cutoff := summaryCutoff(messages, SummarizationOptions{KeepTokens: 25})
	if cutoff != 3 {
		t.Fatalf("token keep cutoff = %d", cutoff)
	}
	memory, _ := backend.NewMemory(nil)
	summaryModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("summary")}})
	mainModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if !strings.Contains(request.Messages[0].TextContent(), "summary") {
			return errors.New("message trigger did not summarize")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	middleware, err := SummarizationMiddleware(SummarizationOptions{
		Model: summaryModel, Backend: memory, TriggerMessages: 3, KeepMessages: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.New(agent.Options{Model: mainModel, Middleware: []agent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: messages[:3]}); err != nil {
		t.Fatal(err)
	}
}

func TestSummarizationTriggerCountsSystemPrompt(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	summaryModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("system-aware summary")}})
	mainModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if !strings.Contains(request.Messages[1].TextContent(), "system-aware summary") {
			return errors.New("system prompt tokens did not trigger summarization")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	middleware, err := SummarizationMiddleware(SummarizationOptions{
		Model: summaryModel, Backend: memory, TriggerTokens: 40, KeepMessages: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.New(agent.Options{
		Model: mainModel, SystemPrompt: strings.Repeat("system context ", 40), Middleware: []agent.Middleware{middleware},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("old"), message.Assistant("recent")}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestManualSummarizationToolIsOptInAndSharesEventState(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	summaryModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("manual summary")}})
	automatic, err := SummarizationMiddleware(SummarizationOptions{Model: summaryModel, Backend: memory, TriggerMessages: 4, KeepMessages: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(automatic.Tools) != 0 {
		t.Fatalf("automatic summarization exposed tools: %#v", automatic.Tools)
	}
	manual, err := SummarizationToolMiddleware(SummarizationToolOptions{Summarization: SummarizationOptions{
		Model: summaryModel, Backend: memory, TriggerMessages: 4, KeepMessages: 2,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(manual.Tools) != 1 || manual.Tools[0].Definition().Name != "compact_conversation" {
		t.Fatalf("manual tools = %#v", manual.Tools)
	}
	messages := []message.Message{message.Human("old"), message.Assistant("old answer"), message.Human("recent"), message.Assistant("recent answer")}
	result, err := manual.Tools[0].Execute(context.Background(), json.RawMessage(`{}`), tool.Runtime{
		CallID: "compact", ThreadID: "thread", State: state.Values{agent.MessagesKey: messages},
	})
	if err != nil {
		t.Fatal(err)
	}
	cutoff, summary, _, ok := decodeSummarizationEvent(result.Update[summarizationEventKey])
	if !ok || cutoff != 2 || !strings.Contains(summary.TextContent(), "manual summary") {
		t.Fatalf("manual event = %#v", result.Update[summarizationEventKey])
	}
	if got := applySummarizationEvent(messages, result.Update[summarizationEventKey]); len(got) != 3 || got[0].ID != summary.ID || got[1].TextContent() != "recent" {
		t.Fatalf("effective messages = %#v", got)
	}
}

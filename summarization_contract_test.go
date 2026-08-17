package dago

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

func mustSummarization(model damodel.Chat, backend dabackend.Backend, options Summarization) dagent.Middleware {
	middleware, err := compileSummarization(options.modelFor(model), backend, options)
	if err != nil {
		panic(err)
	}
	return middleware
}

type failingHistoryBackend struct{ dabackend.Backend }

func (failingHistoryBackend) Write(context.Context, string, string) (dabackend.WriteResult, error) {
	return dabackend.WriteResult{}, errors.New("history unavailable")
}

type failingMediaBackend struct{ dabackend.Backend }

func (failingMediaBackend) Upload(_ context.Context, values []dabackend.Upload) []dabackend.UploadResult {
	result := make([]dabackend.UploadResult, len(values))
	for index, value := range values {
		result[index] = dabackend.UploadResult{Path: value.Path, Error: "upload unavailable"}
	}
	return result
}

func TestHistoryOffloadAppendsPerThreadAndFiltersSyntheticSummaries(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	options := summarizationRuntime{Summarization: Summarization{HistoryRoot: "/conversation_history"}, backend: memory}
	previous := damessage.Human("old synthetic summary")
	previous.Metadata = map[string]json.RawMessage{"lc_source": json.RawMessage(`"summarization"`)}
	first := offloadConversationHistory(context.Background(), options, dagent.Runtime{
		Config: dacheckpoint.Config{ThreadID: "thread", CheckpointID: "first"},
	}, dastate.Values{}, []damessage.Message{previous, damessage.Human("new facts")})
	if first.Err != nil || first.Path != "/conversation_history/thread.md" {
		t.Fatalf("first offload = %#v", first)
	}
	second := offloadConversationHistory(context.Background(), options, dagent.Runtime{
		Config: dacheckpoint.Config{ThreadID: "thread", CheckpointID: "second"},
	}, dastate.Values{}, []damessage.Message{damessage.Human("later facts")})
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
	memory := dabackend.NewMemory(nil)
	summaryModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("durable facts")}})
	mainModel := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if !strings.Contains(request.Messages[0].TextContent(), "durable facts") || strings.Contains(request.Messages[0].TextContent(), "has been saved") {
			return errors.New("invalid degraded summary")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	middleware := mustSummarization(
		summaryModel, failingHistoryBackend{Backend: memory}, Summarization{
			TriggerClauses: []SummarizationTriggerClause{{Tokens: 1}}, KeepMessages: 1,
		})

	system := damessage.System("You are a helpful assistant.")
	response, err := middleware.WrapModelCall(context.Background(), dagent.ModelRequest{
		Model: mainModel, SystemMessage: &system, Messages: []damessage.Message{damessage.Human("old"), damessage.Assistant("recent")}, State: dastate.Values{},
	}, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
		return dagent.ModelResponse{Messages: []damessage.Message{damessage.Assistant("done")}}, nil
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
	memory := dabackend.NewMemory(nil)
	summaryModel := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "failed_to_offload") {
			return errors.New("media placeholder missing")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("summary")}})
	mainModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}})
	middleware := mustSummarization(
		summaryModel, failingMediaBackend{Backend: memory}, Summarization{
			TriggerClauses: []SummarizationTriggerClause{{Tokens: 1}}, KeepMessages: 1,
		})

	compiled := dagent.New(mainModel, dagent.Options{Middleware: []dagent.Middleware{middleware}})

	old := damessage.Message{Role: damessage.RoleHuman, Content: []damessage.ContentBlock{{Type: damessage.BlockImage, MIMEType: "image/png", Data: []byte("secret-inline-data")}}}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{old, damessage.Human("recent")}}); err != nil {
		t.Fatal(err)
	}
}

func TestSummaryMediaOffloadsDataURLsAndPreservesRemoteReferences(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	options := summarizationRuntime{Summarization: Summarization{MediaRoot: "/conversation_history/media"}, backend: memory}
	messages := []damessage.Message{{Role: damessage.RoleHuman, Content: []damessage.ContentBlock{
		{Type: damessage.BlockImage, URL: "data:image/png,diagram%20bytes", Name: "diagram.png"},
		{Type: damessage.BlockAudio, URL: "https://media.example/audio.mp3", MIMEType: "audio/mpeg"},
		{Type: damessage.BlockVideo, URL: "https://media.example/video.mp4", MIMEType: "video/mp4"},
		{Type: damessage.BlockFile, URL: "https://media.example/report.pdf", MIMEType: "application/pdf", Name: "report.pdf"},
	}}}

	got := offloadSummaryMedia(context.Background(), messages, options)
	if got[0].Content[0].Type != damessage.BlockText || !strings.Contains(got[0].Content[0].Text, "/conversation_history/media/") {
		t.Fatalf("data URL block = %#v", got[0].Content[0])
	}
	for index := 1; index < len(got[0].Content); index++ {
		if got[0].Content[index].URL != messages[0].Content[index].URL {
			t.Fatalf("remote block %d = %#v", index, got[0].Content[index])
		}
	}
	files, err := memory.Glob(context.Background(), "**/*.png", "/conversation_history/media")
	if err != nil || len(files.Matches) != 1 {
		t.Fatalf("offloaded data URL = %#v, %v", files, err)
	}
	read, err := memory.Read(context.Background(), files.Matches[0].Path, 0, 10)
	if err != nil || read.Data == nil || read.Data.Encoding != dabackend.EncodingBase64 || read.Data.Content != base64.StdEncoding.EncodeToString([]byte("diagram bytes")) {
		t.Fatalf("decoded data URL = %#v, %v", read, err)
	}
	history := renderHistory(got)
	for _, reference := range []string{
		"https://media.example/audio.mp3",
		"https://media.example/video.mp4",
		"https://media.example/report.pdf",
		"/conversation_history/media/",
	} {
		if !strings.Contains(history, reference) {
			t.Fatalf("history lost %q: %s", reference, history)
		}
	}
}

func TestSummaryMediaUsesBoundedPlaceholderForMalformedDataURL(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	got := offloadSummaryMedia(context.Background(), []damessage.Message{{
		Role:    damessage.RoleHuman,
		Content: []damessage.ContentBlock{{Type: damessage.BlockImage, URL: "data:image/png;base64,%%%"}},
	}}, summarizationRuntime{Summarization: Summarization{MediaRoot: "/media"}, backend: memory})
	if block := got[0].Content[0]; block.Type != damessage.BlockText || block.Text != "<image error=\"failed_to_offload\" />" || strings.Contains(block.Text, "%%") {
		t.Fatalf("malformed data URL placeholder = %#v", block)
	}
}

func TestArgumentTruncationPreservesSchemasAndUnrelatedTools(t *testing.T) {
	write := damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{
		{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/x","content":"unchanged payload"}`)},
		{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/x","content":"abcdefghijklmnop"}`)},
		{ID: "edit", Name: "edit_file", Arguments: json.RawMessage(`{"file_path":"/x","old_string":"abcdefghijklmnop","new_string":"qrstuvwxyzabcdef"}`)},
	}}
	messages := []damessage.Message{write, damessage.Human("middle"), damessage.Assistant("recent")}
	update := truncateOldToolArguments(messages, &ArgumentTruncationOptions{
		TriggerMessages: 3, KeepMessages: 1, MaxLength: 10, PreviewLength: 3, TruncationText: "[cut]",
	})
	overwrite, ok := update[dagent.MessagesKey].(dastate.Overwrite)
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
	messages := []damessage.Message{
		damessage.Human(strings.Repeat("a", 80)), damessage.Assistant(strings.Repeat("b", 80)),
		damessage.Human(strings.Repeat("c", 80)), damessage.Assistant(strings.Repeat("d", 80)),
	}
	cutoff := summaryCutoff(messages, Summarization{KeepTokens: 25})
	if cutoff != 3 {
		t.Fatalf("token keep cutoff = %d", cutoff)
	}
	memory := dabackend.NewMemory(nil)
	summaryModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("summary")}})
	mainModel := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		messages := messagesWithoutSystem(request)
		if len(messages) == 0 || !strings.Contains(messages[0].TextContent(), "summary") {
			return errors.New("message trigger did not summarize")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	middleware := mustSummarization(
		summaryModel, memory, Summarization{
			TriggerClauses: []SummarizationTriggerClause{{Messages: 3}}, KeepMessages: 1,
		})

	compiled := dagent.New(mainModel, dagent.Options{Middleware: []dagent.Middleware{middleware}})

	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: messages[:3]}); err != nil {
		t.Fatal(err)
	}
}

func TestSummarizationTriggerClausesUseAndWithinOrAcross(t *testing.T) {
	chat := modeltest.New(damodel.Profile{ContextWindow: 1_000})
	memory := dabackend.NewMemory(nil)
	options, err := normalizeSummarization(chat, memory, Summarization{
		TriggerClauses: []SummarizationTriggerClause{
			{Messages: 10, Tokens: 400},
			{Fraction: 0.8},
		},
		KeepFraction:       0.1,
		ArgumentTruncation: &ArgumentTruncationOptions{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.KeepTokens != 100 {
		t.Fatalf("keep tokens = %d", options.KeepTokens)
	}
	if summarizationTriggered(options.triggerClauses, 10, 399, 1) {
		t.Fatal("AND trigger matched without its token threshold")
	}
	if !summarizationTriggered(options.triggerClauses, 10, 400, 1) {
		t.Fatal("AND trigger did not match both thresholds")
	}
	if !summarizationTriggered(options.triggerClauses, 1, 800, 1) {
		t.Fatal("fraction trigger did not form an independent OR clause")
	}
	if !summarizationTriggered(options.triggerClauses, 5, 200, 2) {
		t.Fatal("manual compaction did not use half of every clause threshold")
	}
}

func TestSummarizationFractionsRequireKnownContextWindow(t *testing.T) {
	chat := modeltest.New(damodel.Profile{})
	memory := dabackend.NewMemory(nil)
	_, err := normalizeSummarization(chat, memory, Summarization{
		TriggerClauses: []SummarizationTriggerClause{{Fraction: 0.8}}, KeepMessages: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "context window") {
		t.Fatalf("fraction error = %v", err)
	}
}

func TestSummarizationTriggerCountsSystemPrompt(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	summaryModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("system-aware summary")}})
	mainModel := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if !strings.Contains(request.Messages[1].TextContent(), "system-aware summary") {
			return errors.New("system prompt tokens did not trigger summarization")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	middleware := mustSummarization(
		summaryModel, memory, Summarization{
			TriggerClauses: []SummarizationTriggerClause{{Tokens: 40}}, KeepMessages: 1,
		})

	compiled := dagent.New(
		mainModel, dagent.Options{SystemMessage: damessage.System(strings.Repeat("system context ", 40)), Middleware: []dagent.Middleware{middleware}})

	_, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("old"), damessage.Assistant("recent")}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestManualSummarizationToolIsOptInAndSharesEventState(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	summaryModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("manual summary")}})
	automatic := mustSummarization(summaryModel, memory, Summarization{TriggerClauses: []SummarizationTriggerClause{{Messages: 4}}, KeepMessages: 2})

	if len(automatic.Tools) != 0 {
		t.Fatalf("automatic summarization exposed tools: %#v", automatic.Tools)
	}
	manual := SummarizationTool(summaryModel, memory, SummarizationToolOptions{Summarization: Summarization{
		TriggerClauses: []SummarizationTriggerClause{{Messages: 4}}, KeepMessages: 2,
	}})
	if len(manual.Tools) != 1 || manual.Tools[0].Definition().Name != "compact_conversation" {
		t.Fatalf("manual tools = %#v", manual.Tools)
	}
	messages := []damessage.Message{damessage.Human("old"), damessage.Assistant("old answer"), damessage.Human("recent"), damessage.Assistant("recent answer")}
	result, err := manual.Tools[0].Execute(context.Background(), json.RawMessage(`{}`), datool.Runtime{
		CallID: "compact", ThreadID: "thread", State: dastate.Values{dagent.MessagesKey: messages},
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

package dago

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint/serde"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
)

func mustMemory(backend dabackend.Backend, options Memory, addCacheControl ...bool) dagent.Middleware {
	cacheControl := len(addCacheControl) > 0 && addCacheControl[0]
	middleware, err := newMemory(backend, options, cacheControl)
	if err != nil {
		panic(err)
	}
	return middleware
}

type recordingDownloadBackend struct {
	dabackend.Backend
	calls   int
	paths   [][]string
	results []dabackend.DownloadResult
}

func (value *recordingDownloadBackend) Download(ctx context.Context, paths []string) []dabackend.DownloadResult {
	value.calls++
	value.paths = append(value.paths, append([]string(nil), paths...))
	if value.results != nil {
		return append([]dabackend.DownloadResult(nil), value.results...)
	}
	return value.Backend.Download(ctx, paths)
}

func TestMemoryLoadsOnceInOneBatchAndFormatsSourcesInOrder(t *testing.T) {
	memory := dabackend.NewMemory(map[string]dabackend.FileData{
		"/base/AGENTS.md":    {Content: "base\n<!-- private author note -->\n", Encoding: dabackend.EncodingUTF8},
		"/project/AGENTS.md": {Content: "project", Encoding: dabackend.EncodingUTF8},
	})
	recording := &recordingDownloadBackend{Backend: memory}
	middleware := mustMemory(
		recording, Memory{

			Sources: []string{"/base/AGENTS.md", "/missing/AGENTS.md", "/project/AGENTS.md"},
		})

	update, err := middleware.BeforeAgent(context.Background(), dastate.Values{}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if recording.calls != 1 || len(recording.paths) != 1 || len(recording.paths[0]) != 3 {
		t.Fatalf("downloads = %d %#v", recording.calls, recording.paths)
	}
	contents := update["memory_contents"].(map[string]string)
	if !strings.Contains(contents["/base/AGENTS.md"], "private author note") {
		t.Fatalf("raw memory was not retained: %#v", contents)
	}

	script := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		prompt := request.Messages[0].TextContent()
		if strings.Contains(prompt, "private author note") {
			return &memoryTestError{"HTML comment reached the model"}
		}
		base := strings.Index(prompt, "/base/AGENTS.md\n\nbase")
		project := strings.Index(prompt, "/project/AGENTS.md\n\nproject")
		if base < 0 || project <= base {
			return &memoryTestError{"memory source order was not preserved"}
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled := dagent.New(script, dagent.Options{Middleware: []dagent.Middleware{middleware}})

	if _, err := compiled.Invoke(context.Background(), dagent.Input{
		Messages: []damessage.Message{damessage.Human("go")},
		State:    dastate.Values{"memory_contents": contents},
	}); err != nil {
		t.Fatal(err)
	}
	if recording.calls != 1 {
		t.Fatalf("checkpointed memory reloaded; download calls = %d", recording.calls)
	}
}

func TestMemoryPreloadedContentsArePortableAndSurviveCheckpointDecode(t *testing.T) {
	memory := dabackend.NewMemory(map[string]dabackend.FileData{
		"/disk/AGENTS.md": {Content: "disk", Encoding: dabackend.EncodingUTF8},
	})
	recording := &recordingDownloadBackend{Backend: memory}
	middleware := mustMemory(
		recording, Memory{
			Sources:  []string{"/embedded/AGENTS.md", "/disk/AGENTS.md"},
			Contents: map[string]string{"/embedded/AGENTS.md": "embedded"},
		})

	update, err := middleware.BeforeAgent(context.Background(), dastate.Values{}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if recording.calls != 1 || len(recording.paths[0]) != 1 || recording.paths[0][0] != "/disk/AGENTS.md" {
		t.Fatalf("downloaded paths = %#v", recording.paths)
	}
	codec := serde.New(serde.Limits{})
	encoded, err := codec.Encode(update["memory_contents"])
	if err != nil {
		t.Fatal(err)
	}
	restored, err := codec.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	script := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		prompt := request.Messages[0].TextContent()
		if !strings.Contains(prompt, "/embedded/AGENTS.md\n\nembedded") || !strings.Contains(prompt, "/disk/AGENTS.md\n\ndisk") {
			return &memoryTestError{"restored memory contents were not injected"}
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled := dagent.New(script, dagent.Options{Middleware: []dagent.Middleware{middleware}})

	if _, err := compiled.Invoke(context.Background(), dagent.Input{
		Messages: []damessage.Message{damessage.Human("go")}, State: dastate.Values{"memory_contents": restored},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryMissingIsEmptyAndOtherDownloadErrorsFail(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	middleware := mustMemory(memory, Memory{Sources: []string{"/missing"}})

	update, err := middleware.BeforeAgent(context.Background(), dastate.Values{}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if len(update["memory_contents"].(map[string]string)) != 0 {
		t.Fatalf("update = %#v", update)
	}

	denied := &recordingDownloadBackend{Backend: memory, results: []dabackend.DownloadResult{{Path: "/locked", Error: "permission_denied"}}}
	middleware = mustMemory(denied, Memory{Sources: []string{"/locked"}})

	if _, err := middleware.BeforeAgent(context.Background(), dastate.Values{}, dagent.Runtime{}); err == nil || !strings.Contains(err.Error(), "permission_denied") {
		t.Fatalf("error = %v", err)
	}
}

func TestMemoryPromptCanBeCustomizedOrDisabled(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	invalid := "memory without a slot"
	requirePanicContaining(t, "{agent_memory}", func() {
		mustMemory(memory, Memory{SystemPrompt: PromptTemplate{Mode: PromptCustom, Text: invalid}})
	})

	middleware := mustMemory(memory, Memory{SystemPrompt: PromptTemplate{Mode: PromptDisabled}})

	system := damessage.System("You are a helpful assistant.")
	_, err := middleware.WrapModelCall(context.Background(), dagent.ModelRequest{
		SystemMessage: &system, Messages: []damessage.Message{damessage.Human("go")}, State: dastate.Values{"memory_contents": map[string]string{}},
	}, func(_ context.Context, request dagent.ModelRequest) (dagent.ModelResponse, error) {
		if request.SystemMessage == nil || request.SystemMessage.TextContent() != system.TextContent() {
			return dagent.ModelResponse{}, &memoryTestError{"disabled memory prompt changed the system message"}
		}
		return dagent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMemoryReadOnlyUsesUsefulDefaultUnlessPromptIsExplicit(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	middleware := mustMemory(memory, Memory{ReadOnly: true})

	_, err := middleware.WrapModelCall(context.Background(), dagent.ModelRequest{
		Messages: []damessage.Message{damessage.Human("go")}, State: dastate.Values{"memory_contents": map[string]string{}},
	}, func(_ context.Context, request dagent.ModelRequest) (dagent.ModelResponse, error) {
		prompt := request.SystemMessage.TextContent()
		if !strings.Contains(prompt, "Memory is read-only") || strings.Contains(prompt, "Persist durable") {
			return dagent.ModelResponse{}, &memoryTestError{"read-only memory prompt was not selected"}
		}
		return dagent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	custom := mustMemory(memory, Memory{ReadOnly: true, SystemPrompt: PromptTemplate{Mode: PromptCustom, Text: "custom {agent_memory}"}})
	_, err = custom.WrapModelCall(context.Background(), dagent.ModelRequest{State: dastate.Values{"memory_contents": map[string]string{}}}, func(_ context.Context, request dagent.ModelRequest) (dagent.ModelResponse, error) {
		if got := request.SystemMessage.TextContent(); !strings.Contains(got, "custom") || strings.Contains(got, "read-only") {
			return dagent.ModelResponse{}, &memoryTestError{"explicit memory prompt did not take precedence"}
		}
		return dagent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMemoryAddsProviderCacheHintToLastSystemBlock(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	middleware := mustMemory(memory, Memory{SystemPrompt: PromptTemplate{Mode: PromptDisabled}}, true)

	provider := modeltest.New(damodel.Profile{Provider: "anthropic"})
	system := damessage.System("static")
	_, err := middleware.WrapModelCall(context.Background(), dagent.ModelRequest{Model: provider, SystemMessage: &system}, func(_ context.Context, request dagent.ModelRequest) (dagent.ModelResponse, error) {
		raw := request.SystemMessage.Content[0].Extra["cache_control"]
		var hint map[string]string
		if json.Unmarshal(raw, &hint) != nil || hint["type"] != "ephemeral" {
			return dagent.ModelResponse{}, &memoryTestError{"cache hint missing"}
		}
		return dagent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type memoryTestError struct{ text string }

func (value *memoryTestError) Error() string { return value.text }

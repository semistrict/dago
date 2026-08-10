package dago

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	"github.com/semistrict/dago/checkpoint/serde"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
	"github.com/semistrict/dago/state"
)

type recordingDownloadBackend struct {
	backend.Backend
	calls   int
	paths   [][]string
	results []backend.DownloadResult
}

func (value *recordingDownloadBackend) Download(ctx context.Context, paths []string) []backend.DownloadResult {
	value.calls++
	value.paths = append(value.paths, append([]string(nil), paths...))
	if value.results != nil {
		return append([]backend.DownloadResult(nil), value.results...)
	}
	return value.Backend.Download(ctx, paths)
}

func TestMemoryLoadsOnceInOneBatchAndFormatsSourcesInOrder(t *testing.T) {
	memory, err := backend.NewMemory(map[string]backend.FileData{
		"/base/AGENTS.md":    {Content: "base\n<!-- private author note -->\n", Encoding: backend.EncodingUTF8},
		"/project/AGENTS.md": {Content: "project", Encoding: backend.EncodingUTF8},
	})
	if err != nil {
		t.Fatal(err)
	}
	recording := &recordingDownloadBackend{Backend: memory}
	middleware, err := MemoryMiddleware(MemoryOptions{
		Backend: recording,
		Sources: []string{"/base/AGENTS.md", "/missing/AGENTS.md", "/project/AGENTS.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	update, err := middleware.BeforeAgent(context.Background(), state.Values{}, agent.Runtime{})
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

	script := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
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
	}, Response: model.Response{Message: message.Assistant("done")}})
	compiled, err := agent.New(agent.Options{Model: script, Middleware: []agent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{
		Messages: []message.Message{message.Human("go")},
		State:    state.Values{"memory_contents": contents},
	}); err != nil {
		t.Fatal(err)
	}
	if recording.calls != 1 {
		t.Fatalf("checkpointed memory reloaded; download calls = %d", recording.calls)
	}
}

func TestMemoryPreloadedContentsArePortableAndSurviveCheckpointDecode(t *testing.T) {
	memory, err := backend.NewMemory(map[string]backend.FileData{
		"/disk/AGENTS.md": {Content: "disk", Encoding: backend.EncodingUTF8},
	})
	if err != nil {
		t.Fatal(err)
	}
	recording := &recordingDownloadBackend{Backend: memory}
	middleware, err := MemoryMiddleware(MemoryOptions{
		Backend: recording, Sources: []string{"/embedded/AGENTS.md", "/disk/AGENTS.md"},
		Contents: map[string]string{"/embedded/AGENTS.md": "embedded"},
	})
	if err != nil {
		t.Fatal(err)
	}
	update, err := middleware.BeforeAgent(context.Background(), state.Values{}, agent.Runtime{})
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
	script := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		prompt := request.Messages[0].TextContent()
		if !strings.Contains(prompt, "/embedded/AGENTS.md\n\nembedded") || !strings.Contains(prompt, "/disk/AGENTS.md\n\ndisk") {
			return &memoryTestError{"restored memory contents were not injected"}
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	compiled, err := agent.New(agent.Options{Model: script, Middleware: []agent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{
		Messages: []message.Message{message.Human("go")}, State: state.Values{"memory_contents": restored},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryMissingIsEmptyAndOtherDownloadErrorsFail(t *testing.T) {
	memory, err := backend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := MemoryMiddleware(MemoryOptions{Backend: memory, Sources: []string{"/missing"}})
	if err != nil {
		t.Fatal(err)
	}
	update, err := middleware.BeforeAgent(context.Background(), state.Values{}, agent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if len(update["memory_contents"].(map[string]string)) != 0 {
		t.Fatalf("update = %#v", update)
	}

	denied := &recordingDownloadBackend{Backend: memory, results: []backend.DownloadResult{{Path: "/locked", Error: "permission_denied"}}}
	middleware, err = MemoryMiddleware(MemoryOptions{Backend: denied, Sources: []string{"/locked"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := middleware.BeforeAgent(context.Background(), state.Values{}, agent.Runtime{}); err == nil || !strings.Contains(err.Error(), "permission_denied") {
		t.Fatalf("error = %v", err)
	}
}

func TestMemoryPromptCanBeCustomizedOrDisabled(t *testing.T) {
	memory, err := backend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	invalid := "memory without a slot"
	if _, err := MemoryMiddleware(MemoryOptions{Backend: memory, SystemPrompt: &invalid}); err == nil {
		t.Fatal("expected missing agent_memory slot to fail")
	}

	disabled := ""
	middleware, err := MemoryMiddleware(MemoryOptions{Backend: memory, SystemPrompt: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	_, err = middleware.WrapModelCall(context.Background(), agent.ModelRequest{
		Messages: []message.Message{message.Human("go")}, State: state.Values{"memory_contents": map[string]string{}},
	}, func(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
		if request.SystemMessage != nil {
			return agent.ModelResponse{}, &memoryTestError{"disabled memory prompt created a system message"}
		}
		return agent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMemoryAddsProviderCacheHintToLastSystemBlock(t *testing.T) {
	memory, err := backend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	disabled := ""
	middleware, err := MemoryMiddleware(MemoryOptions{Backend: memory, SystemPrompt: &disabled, AddCacheControl: true})
	if err != nil {
		t.Fatal(err)
	}
	provider := modeltest.New(model.Profile{Provider: "anthropic"})
	system := message.System("static")
	_, err = middleware.WrapModelCall(context.Background(), agent.ModelRequest{Model: provider, SystemMessage: &system}, func(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
		raw := request.SystemMessage.Content[0].Extra["cache_control"]
		var hint map[string]string
		if json.Unmarshal(raw, &hint) != nil || hint["type"] != "ephemeral" {
			return agent.ModelResponse{}, &memoryTestError{"cache hint missing"}
		}
		return agent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type memoryTestError struct{ text string }

func (value *memoryTestError) Error() string { return value.text }

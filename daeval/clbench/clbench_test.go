package clbench

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingFactory struct {
	configs []AgentConfig
	agent   Agent
	err     error
	panic   bool
}

func (factory *recordingFactory) Build(_ context.Context, config AgentConfig) (Agent, error) {
	if factory.panic {
		panic("secret factory panic")
	}
	factory.configs = append(factory.configs, config)
	return factory.agent, factory.err
}

func schema(name string) Schema {
	return NewSchema(name, json.RawMessage(`{"type":"object","properties":{"move":{"type":"string"}},"required":["move"]}`))
}

func TestSystemThreadsMemoryFeedbackUsageAndArtifacts(t *testing.T) {
	var inputs []TurnInput
	turn := 0
	agent := AgentFunc(func(_ context.Context, input TurnInput) (TurnOutput, error) {
		inputs = append(inputs, input)
		turn++
		if turn == 1 {
			return TurnOutput{
				Action: json.RawMessage(`{"move":"raise"}`),
				Files: map[string]File{
					MemoryPath:      {Content: "# Strategy notes\n\nraise often\n", Encoding: "utf-8"},
					"/scratch.json": {Content: `{"seen":true}`, Encoding: "utf-8"},
				},
				Usage: []Usage{{InputTokens: 10, OutputTokens: 3}, {InputTokens: 2, OutputTokens: 1}},
			}, nil
		}
		return TurnOutput{Action: json.RawMessage(`{"move":"call"}`)}, nil
	})
	factory := &recordingFactory{agent: agent}
	system := New(factory, Options{})
	response, err := system.Respond(t.Context(), NewQuery("", schema("poker_move")))
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Action) != `{"move":"raise"}` || response.Metadata.System != DefaultName || response.Metadata.Model != DefaultModel || response.Metadata.Interaction != 1 || response.Metadata.MemoryFiles[MemoryPath] != "# Strategy notes\n\nraise often\n" {
		t.Fatalf("first response = %#v", response)
	}
	if len(inputs) != 1 || inputs[0].Prompt != "(no content)" || inputs[0].Files[MemoryPath].Content != SeedMemory {
		t.Fatalf("first input = %#v", inputs)
	}
	if len(factory.configs) != 1 || factory.configs[0].Model != DefaultModel || factory.configs[0].SystemPrompt != SystemPrompt || !reflect.DeepEqual(factory.configs[0].MemorySources, []string{MemoryPath}) {
		t.Fatalf("factory configs = %#v", factory.configs)
	}

	if err := system.Observe(Observation{Content: " opponent folded "}); err != nil {
		t.Fatal(err)
	}
	response, err = system.Respond(t.Context(), NewQuery("Choose again", schema("poker_move")))
	if err != nil {
		t.Fatal(err)
	}
	wantPrompt := "Feedback on your previous action:\nopponent folded\n\nChoose again"
	if len(inputs) != 2 || inputs[1].Prompt != wantPrompt || inputs[1].Files[MemoryPath].Content != "# Strategy notes\n\nraise often\n" || inputs[1].Files["/scratch.json"].Content == "" {
		t.Fatalf("second input = %#v", inputs[1])
	}
	if response.Metadata.Interaction != 2 || response.Metadata.MemoryFiles[MemoryPath] != "# Strategy notes\n\nraise often\n" || len(factory.configs) != 1 {
		t.Fatalf("second response = %#v; builds %d", response, len(factory.configs))
	}
	usage := system.UsageEvents()
	if !reflect.DeepEqual(usage, []UsageEvent{{CallType: "completion", Model: DefaultModel, InputTokens: 12, OutputTokens: 4, TotalTokens: 16}}) {
		t.Fatalf("usage = %#v", usage)
	}
	artifacts := system.GetRunArtifacts()
	if artifacts.ArtifactType != DefaultName || artifacts.Model != DefaultModel || artifacts.InteractionCount != 2 || artifacts.MemoryFiles[MemoryPath] != "# Strategy notes\n\nraise often\n" {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	if system.Name() != DefaultName || !system.ParallelSafe() || !system.SupportsBaseline() {
		t.Fatalf("system properties = %q, %v, %v", system.Name(), system.ParallelSafe(), system.SupportsBaseline())
	}
}

func TestQueryFeedbackPrecedesPendingAndResetIsStateless(t *testing.T) {
	var prompts []string
	var memories []string
	agent := AgentFunc(func(_ context.Context, input TurnInput) (TurnOutput, error) {
		prompts = append(prompts, input.Prompt)
		memories = append(memories, input.Files[MemoryPath].Content)
		files := cloneFiles(input.Files)
		files[MemoryPath] = File{Content: "learned", Encoding: "utf-8"}
		return TurnOutput{Action: json.RawMessage(`{"move":"ok"}`), Files: files}, nil
	})
	factory := &recordingFactory{agent: agent}
	system := New(factory, Options{})
	if err := system.Observe(Observation{Content: "pending"}); err != nil {
		t.Fatal(err)
	}
	query := NewQuery("act", schema("move"))
	query.Feedback = "direct"
	if _, err := system.Respond(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	if prompts[0] != "Feedback on your previous action:\ndirect\n\nact" {
		t.Fatalf("prompt = %q", prompts[0])
	}
	if _, err := system.Respond(t.Context(), NewQuery("again", schema("move"))); err != nil {
		t.Fatal(err)
	}
	if prompts[1] != "again" {
		t.Fatalf("consumed pending feedback leaked: %q", prompts[1])
	}
	system.Reset()
	if _, err := system.Respond(t.Context(), NewQuery("fresh", schema("move"))); err != nil {
		t.Fatal(err)
	}
	if memories[2] != SeedMemory || len(factory.configs) != 1 {
		t.Fatalf("reset memory = %q; factory builds %d", memories[2], len(factory.configs))
	}
	if artifacts := system.GetRunArtifacts(); artifacts.InteractionCount != 1 {
		t.Fatalf("reset artifacts = %#v", artifacts)
	}
}

func TestSchemaCacheCanonicalizesJSONAndBoundsDistinctSchemas(t *testing.T) {
	factory := &recordingFactory{agent: AgentFunc(func(context.Context, TurnInput) (TurnOutput, error) {
		return TurnOutput{Action: json.RawMessage(`{"move":"ok"}`)}, nil
	})}
	system := New(factory, Options{MaxSchemas: 1})
	first := NewSchema("move", json.RawMessage(`{ "type": "object" }`))
	second := NewSchema("move", json.RawMessage(`{"type":"object"}`))
	if _, err := system.Respond(t.Context(), NewQuery("one", first)); err != nil {
		t.Fatal(err)
	}
	if _, err := system.Respond(t.Context(), NewQuery("two", second)); err != nil {
		t.Fatal(err)
	}
	if len(factory.configs) != 1 || string(factory.configs[0].ResponseSchema.Document) != `{"type":"object"}` {
		t.Fatalf("factory configs = %#v", factory.configs)
	}
	if _, err := system.Respond(t.Context(), NewQuery("three", NewSchema("other", json.RawMessage(`{"type":"object"}`)))); !errors.Is(err, ErrSchemaLimit) {
		t.Fatalf("schema limit = %v", err)
	}
}

func TestCancellationWinsWhenFactoryOrAgentReturnsAnyway(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	factory := FactoryFunc(func(buildCtx context.Context, _ AgentConfig) (Agent, error) {
		if _, ok := buildCtx.Deadline(); !ok {
			t.Fatal("factory context has no deadline")
		}
		cancel()
		return AgentFunc(func(context.Context, TurnInput) (TurnOutput, error) {
			return TurnOutput{Action: json.RawMessage(`{"move":"hidden"}`)}, nil
		}), nil
	})
	if _, err := New(factory, Options{}).Respond(ctx, NewQuery("act", schema("move"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("factory cancellation = %v", err)
	}

	ctx, cancel = context.WithCancel(t.Context())
	agent := AgentFunc(func(invokeCtx context.Context, _ TurnInput) (TurnOutput, error) {
		if _, ok := invokeCtx.Deadline(); !ok {
			t.Fatal("agent context has no deadline")
		}
		cancel()
		return TurnOutput{Action: json.RawMessage(`{"move":"hidden"}`)}, nil
	})
	if _, err := New(&recordingFactory{agent: agent}, Options{}).Respond(ctx, NewQuery("act", schema("move"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("agent cancellation = %v", err)
	}
}

func TestPanicsBecomeGenericErrorsAndTransportErrorsRemainClassifiable(t *testing.T) {
	factoryPanic := &recordingFactory{panic: true}
	if _, err := New(factoryPanic, Options{}).Respond(t.Context(), NewQuery("act", schema("move"))); err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "factory panicked") {
		t.Fatalf("factory panic = %v", err)
	}
	agentPanic := &recordingFactory{agent: AgentFunc(func(context.Context, TurnInput) (TurnOutput, error) {
		panic("secret agent panic")
	})}
	if _, err := New(agentPanic, Options{}).Respond(t.Context(), NewQuery("act", schema("move"))); err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "agent panicked") {
		t.Fatalf("agent panic = %v", err)
	}

	classified := errors.New("classified")
	long := errors.Join(classified, errors.New(strings.Repeat("x", 10_000)))
	failure := &recordingFactory{agent: AgentFunc(func(context.Context, TurnInput) (TurnOutput, error) {
		return TurnOutput{}, long
	})}
	_, err := New(failure, Options{MaxErrorBytes: 100}).Respond(t.Context(), NewQuery("act", schema("move")))
	if !errors.Is(err, classified) || len(err.Error()) > len("clbench agent turn: ")+103 {
		t.Fatalf("bounded classified error = %v", err)
	}
}

func TestResponseValidationIsAtomicAndBounded(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		output  TurnOutput
		wantIs  error
		want    string
	}{
		{name: "missing action", output: TurnOutput{}, want: "structured object"},
		{name: "non-object action", output: TurnOutput{Action: json.RawMessage(`[]`)}, want: "structured object"},
		{name: "oversized action", options: Options{MaxActionBytes: 2}, output: TurnOutput{Action: json.RawMessage(`{"a":1}`)}, wantIs: ErrPayloadTooLarge},
		{name: "too many files", options: Options{MaxFiles: 1}, output: TurnOutput{Action: json.RawMessage(`{}`), Files: map[string]File{"/a": {}, "/b": {}}}, wantIs: ErrPayloadTooLarge},
		{name: "invalid path", output: TurnOutput{Action: json.RawMessage(`{}`), Files: map[string]File{"../secret": {}}}, want: "invalid in-state file path"},
		{name: "invalid encoding", output: TurnOutput{Action: json.RawMessage(`{}`), Files: map[string]File{"/a": {Encoding: "rot13"}}}, want: "unsupported file encoding"},
		{name: "invalid base64", output: TurnOutput{Action: json.RawMessage(`{}`), Files: map[string]File{"/a": {Content: "%%%", Encoding: "base64"}}}, want: "invalid base64"},
		{name: "oversized file", options: Options{MaxFileBytes: 1}, output: TurnOutput{Action: json.RawMessage(`{}`), Files: map[string]File{"/a": {Content: "xx"}}}, wantIs: ErrPayloadTooLarge},
		{name: "oversized total", options: Options{MaxFilesTotalBytes: 3}, output: TurnOutput{Action: json.RawMessage(`{}`), Files: map[string]File{"/aa": {Content: "x"}}}, wantIs: ErrPayloadTooLarge},
		{name: "too many usage records", options: Options{MaxUsageRecords: 1}, output: TurnOutput{Action: json.RawMessage(`{}`), Usage: []Usage{{}, {}}}, wantIs: ErrPayloadTooLarge},
		{name: "negative usage", output: TurnOutput{Action: json.RawMessage(`{}`), Usage: []Usage{{InputTokens: -1}}}, wantIs: ErrPayloadTooLarge},
		{name: "token overflow", options: Options{MaxTokens: 2}, output: TurnOutput{Action: json.RawMessage(`{}`), Usage: []Usage{{InputTokens: 2, OutputTokens: 1}}}, wantIs: ErrPayloadTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			system := New(&recordingFactory{agent: AgentFunc(func(context.Context, TurnInput) (TurnOutput, error) {
				return test.output, nil
			})}, test.options)
			_, err := system.Respond(t.Context(), NewQuery("act", schema("move")))
			if test.wantIs != nil && !errors.Is(err, test.wantIs) {
				t.Fatalf("error = %v, want errors.Is %v", err, test.wantIs)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if snapshot := system.GetRunArtifacts().MemoryFiles[MemoryPath]; snapshot != SeedMemory {
				t.Fatalf("invalid output changed memory to %q", snapshot)
			}
		})
	}
}

func TestPromptObservationTurnAndSchemaLimits(t *testing.T) {
	agent := AgentFunc(func(context.Context, TurnInput) (TurnOutput, error) {
		return TurnOutput{Action: json.RawMessage(`{}`)}, nil
	})
	system := New(&recordingFactory{agent: agent}, Options{MaxPromptBytes: 5, MaxTurns: 1, MaxSchemaBytes: 40})
	if err := system.Observe(Observation{Content: "123456"}); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("observation limit = %v", err)
	}
	if err := system.Observe(Observation{Content: string([]byte{0xff})}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("observation UTF-8 = %v", err)
	}
	if _, err := system.Respond(t.Context(), NewQuery("123456", NewSchema("a", json.RawMessage(`{}`)))); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("prompt limit = %v", err)
	}
	invalidPrompt := NewQuery(string([]byte{0xff}), NewSchema("a", json.RawMessage(`{}`)))
	if _, err := system.Respond(t.Context(), invalidPrompt); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("prompt UTF-8 = %v", err)
	}
	if _, err := system.Respond(t.Context(), NewQuery("ok", schema("move"))); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("schema limit = %v", err)
	}
	if _, err := system.Respond(t.Context(), NewQuery("ok", NewSchema("a", json.RawMessage(`{}`)))); err != nil {
		t.Fatal(err)
	}
	if _, err := system.Respond(t.Context(), NewQuery("ok", NewSchema("a", json.RawMessage(`{}`)))); !errors.Is(err, ErrTurnLimit) {
		t.Fatalf("turn limit = %v", err)
	}
}

func TestInputAndReturnedSnapshotsCannotMutateSystem(t *testing.T) {
	var retained map[string]File
	action := json.RawMessage(`{"move":"safe"}`)
	agent := AgentFunc(func(_ context.Context, input TurnInput) (TurnOutput, error) {
		input.Files[MemoryPath] = File{Content: "mutated input"}
		retained = map[string]File{MemoryPath: {Content: "learned", Encoding: "utf-8"}}
		return TurnOutput{Action: action, Files: retained}, nil
	})
	system := New(&recordingFactory{agent: agent}, Options{})
	response, err := system.Respond(t.Context(), NewQuery("act", schema("move")))
	if err != nil {
		t.Fatal(err)
	}
	retained[MemoryPath] = File{Content: "changed later"}
	action[2] = 'X'
	response.Metadata.MemoryFiles[MemoryPath] = "response mutation"
	if got := system.GetRunArtifacts().MemoryFiles[MemoryPath]; got != "learned" {
		t.Fatalf("system memory = %q", got)
	}
	if string(response.Action) != `{"move":"safe"}` {
		t.Fatalf("response action mutated = %s", response.Action)
	}
}

func TestConcurrentTurnsAreSerializedAndRaceSafe(t *testing.T) {
	var calls int
	agent := AgentFunc(func(_ context.Context, input TurnInput) (TurnOutput, error) {
		calls++
		return TurnOutput{Action: json.RawMessage(`{"move":"ok"}`), Files: input.Files}, nil
	})
	system := New(&recordingFactory{agent: agent}, Options{})
	const count = 20
	var wait sync.WaitGroup
	errorsSeen := make(chan error, count)
	for range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := system.Respond(t.Context(), NewQuery("act", schema("move")))
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != count || system.GetRunArtifacts().InteractionCount != count {
		t.Fatalf("calls = %d, artifacts = %#v", calls, system.GetRunArtifacts())
	}
}

func TestConstructorsRejectInvalidStaticInputs(t *testing.T) {
	tests := []func(){
		func() { New(nil, Options{}) },
		func() {
			var factory *recordingFactory
			New(factory, Options{})
		},
		func() { New(&recordingFactory{}, Options{TurnTimeout: -time.Second}) },
		func() { New(&recordingFactory{}, Options{MaxTurns: -1}) },
		func() { New(&recordingFactory{}, Options{Name: " "}) },
		func() { NewSchema("", json.RawMessage(`{}`)) },
		func() { NewSchema("x", json.RawMessage(`no`)) },
		func() { NewSchema(string([]byte{0xff}), json.RawMessage(`{}`)) },
		func() { NewQuery("x", Schema{}) },
	}
	for index, call := range tests {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("constructor did not panic")
				}
			}()
			call()
		})
	}
}

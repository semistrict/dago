//go:build !tinygo

package dago

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

type interpreterProgressRecorder struct {
	mu     sync.Mutex
	events []datool.Progress
}

func (recorder *interpreterProgressRecorder) Write(_ context.Context, value json.RawMessage) error {
	var envelope struct {
		Progress datool.Progress `json:"tool_progress"`
	}
	if err := json.Unmarshal(value, &envelope); err != nil {
		return err
	}
	recorder.mu.Lock()
	recorder.events = append(recorder.events, envelope.Progress)
	recorder.mu.Unlock()
	return nil
}

func (recorder *interpreterProgressRecorder) snapshot() []datool.Progress {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]datool.Progress(nil), recorder.events...)
}

func TestInterpreterPersistsBindingsAndCallsPTCTools(t *testing.T) {
	echo := datool.MustNew("echo_value", "Return the supplied value.", func(_ context.Context, input struct {
		Value string `json:"value"`
	}) (string, error) {
		return "echo:" + input.Value, nil
	})
	middleware, err := compileInterpreter(Interpreter{PTC: []string{"echo_value"}})
	if err != nil {
		t.Fatal(err)
	}
	request := dagent.ModelRequest{
		Tools:   append([]datool.Tool{echo}, middleware.Tools...),
		Runtime: dagent.Runtime{Config: dacheckpoint.Config{ThreadID: "thread"}},
	}
	system := damessage.System("You are a helpful assistant.")
	request.SystemMessage = &system
	_, err = middleware.WrapModelCall(context.Background(), request, func(_ context.Context, got dagent.ModelRequest) (dagent.ModelResponse, error) {
		if !strings.Contains(got.SystemMessage.TextContent(), "tools.echoValue") {
			t.Fatalf("interpreter prompt = %#v", got.SystemMessage)
		}
		return dagent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	eval := middleware.Tools[0]
	first, err := eval.Execute(context.Background(), json.RawMessage(`{"code":"const base = 40; await tools.echoValue({value: String(base + 2)})"}`), datool.Runtime{
		ThreadID: "thread", TaskID: "task", CallID: "call", State: dastate.Values{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text := first.Content[0].Text; !strings.Contains(text, "echo:42") {
		t.Fatalf("first result = %s", text)
	}
	field := middleware.Fields[interpreterSnapshotKey]
	state, err := field.Reduce(field.Initial(), []any{first.Update[interpreterSnapshotKey]})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := materializedInterpreterSnapshot(state)
	if len(snapshot) == 0 {
		t.Fatalf("snapshot = %T (%d)", first.Update[interpreterSnapshotKey], len(snapshot))
	}
	second, err := eval.Execute(context.Background(), json.RawMessage(`{"code":"base + 3"}`), datool.Runtime{
		ThreadID: "thread", TaskID: "task", CallID: "call-2",
		State: dastate.Values{interpreterSnapshotKey: state},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text := second.Content[0].Text; !strings.Contains(text, ">43</result>") {
		t.Fatalf("restored result = %s", text)
	}
	record := second.Update[interpreterSnapshotKey].(map[string]any)
	if record["kind"] != "pages" || len(record["data"].([]byte)) >= len(snapshot) {
		t.Fatalf("second snapshot record = kind %v, %d bytes; full = %d", record["kind"], len(record["data"].([]byte)), len(snapshot))
	}
}

func TestInterpreterPTCTransparencyEmitsOrdinaryToolLifecycle(t *testing.T) {
	echo := datool.MustNew("echo_value", "Return the supplied value.", func(ctx context.Context, input struct {
		Value string `json:"value"`
	}) (string, error) {
		if err := datool.EmitProgress(ctx, datool.Progress{Output: "working"}); err != nil {
			return "", err
		}
		return "echo:" + input.Value, nil
	})
	_, eval, _ := prepareInterpreterTest(t, Interpreter{
		PTC: []string{"echo_value"}, PTCTransparency: true,
	}, "transparent", echo)
	recorder := &interpreterProgressRecorder{}
	raw := json.RawMessage(`{"code":"await tools.echoValue({value: 'visible'})"}`)
	result, err := eval.Execute(t.Context(), raw, datool.Runtime{
		ThreadID: "transparent", TaskID: "task", CallID: "outer-call", State: dastate.Values{}, Stream: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == damessage.ToolStatusError {
		t.Fatalf("interpreter result = %#v", result)
	}
	artifact, ok := datool.ParsePTCTranscriptArtifact(result.Artifact)
	if !ok || len(artifact.Calls) != 1 {
		t.Fatalf("transparent transcript artifact = %#v, %v", artifact, ok)
	}
	call := artifact.Calls[0]
	if call.CallID == "" || call.Name != "echo_value" || call.Output != "echo:visible" || call.Status != damessage.ToolStatusSuccess || string(call.Arguments) != `{"value":"visible"}` {
		t.Fatalf("transparent transcript call = %#v", call)
	}
	events := recorder.snapshot()
	if len(events) != 3 {
		t.Fatalf("progress events = %#v", events)
	}
	start, intermediate, completed := events[0], events[1], events[2]
	if start.CallID == "" || start.CallID != completed.CallID || start.Name != "echo_value" || start.ParentCallID != "outer-call" || start.Status != "" || start.Output != "" {
		t.Fatalf("start = %#v, completed = %#v", start, completed)
	}
	if intermediate.CallID != start.CallID || intermediate.Output != "working" || intermediate.Status != "" {
		t.Fatalf("intermediate = %#v", intermediate)
	}
	var arguments map[string]any
	if err := json.Unmarshal(start.Arguments, &arguments); err != nil || arguments["value"] != "visible" {
		t.Fatalf("arguments = %s, %v", start.Arguments, err)
	}
	if completed.Status != damessage.ToolStatusSuccess || completed.Output != "echo:visible" || len(completed.Arguments) != 0 || completed.ParentCallID != "outer-call" {
		t.Fatalf("completed = %#v", completed)
	}
}

func TestInterpreterPTCTransparencyDefaultsOff(t *testing.T) {
	echo := datool.MustNew("echo_value", "Return a value.", func(ctx context.Context, _ struct{}) (string, error) {
		if err := datool.EmitProgress(ctx, datool.Progress{Output: "internal progress"}); err != nil {
			return "", err
		}
		return "hidden", nil
	})
	_, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{"echo_value"}}, "opaque", echo)
	recorder := &interpreterProgressRecorder{}
	result, err := eval.Execute(t.Context(), json.RawMessage(`{"code":"await tools.echoValue({})"}`), datool.Runtime{
		ThreadID: "opaque", TaskID: "task", CallID: "outer-call", State: dastate.Values{}, Stream: recorder,
	})
	if err != nil || result.Status == damessage.ToolStatusError {
		t.Fatalf("result = %#v, %v", result, err)
	}
	if len(result.Artifact) != 0 {
		t.Fatalf("opaque interpreter persisted transparent artifact %s", result.Artifact)
	}
	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("default transparency events = %#v", events)
	}
}

func TestInterpreterPTCTransparencyMarksParentModelCalls(t *testing.T) {
	middleware, err := compileInterpreter(Interpreter{PTCTransparency: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := middleware.WrapModelCall(t.Context(), dagent.ModelRequest{}, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
		return dagent.ModelResponse{Messages: []damessage.Message{{
			Role: damessage.RoleAssistant,
			ToolCalls: []damessage.ToolCall{
				{ID: "eval-call", Name: "js_eval"},
				{ID: "read-call", Name: "read_file"},
			},
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, ok := damessage.MetadataAs[datool.PTCTransparencyMetadata](response.Messages[0].Metadata, datool.PTCTransparencyMetadataKey)
	if !ok || len(metadata.ParentCallIDs) != 1 || metadata.ParentCallIDs[0] != "eval-call" {
		t.Fatalf("transparency metadata = %#v, %v", metadata, ok)
	}
}

func TestInterpreterPTCTransparencyEmitsFailedToolCompletion(t *testing.T) {
	boom := errors.New("lookup failed")
	failing := datool.MustNew("lookup", "Fail a lookup.", func(context.Context, struct{}) (string, error) {
		return "", boom
	})
	_, eval, _ := prepareInterpreterTest(t, Interpreter{
		PTC: []string{"lookup"}, PTCTransparency: true,
	}, "transparent-failure", failing)
	recorder := &interpreterProgressRecorder{}
	_, err := eval.Execute(t.Context(), json.RawMessage(`{"code":"await tools.lookup({})"}`), datool.Runtime{
		ThreadID: "transparent-failure", TaskID: "task", CallID: "outer-call", State: dastate.Values{}, Stream: recorder,
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	events := recorder.snapshot()
	if len(events) != 2 || events[1].CallID != events[0].CallID || events[1].Status != damessage.ToolStatusError || !strings.Contains(events[1].Output, boom.Error()) {
		t.Fatalf("progress events = %#v", events)
	}
}

func TestInterpreterRejectsPTCNameCollisions(t *testing.T) {
	first := datool.MustNew("foo_bar", "first", func(context.Context, struct{}) (string, error) { return "", nil })
	second := datool.MustNew("foo-bar", "second", func(context.Context, struct{}) (string, error) { return "", nil })
	middleware, err := compileInterpreter(Interpreter{PTC: []string{"foo_bar", "foo-bar"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = middleware.WrapModelCall(context.Background(), dagent.ModelRequest{Tools: []datool.Tool{first, second}}, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
		return dagent.ModelResponse{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "same JavaScript name") {
		t.Fatalf("collision error = %v", err)
	}
}

func TestInterpreterSnapshotReducerRejectsMalformedDirtyPages(t *testing.T) {
	field := interpreterSnapshotField(10)
	_, err := field.Reduce(field.Initial(), []any{map[string]any{"kind": "pages", "data": []byte("bad")}})
	if err == nil {
		t.Fatal("malformed dirty-page error = nil")
	}
}

func TestMoveInterpreterAfterToolFilters(t *testing.T) {
	values := []dagent.Middleware{{Name: "code_interpreter"}, {Name: "tool_exclusion"}}
	values = moveMiddlewareLast(values, "code_interpreter")
	if values[0].Name != "tool_exclusion" || values[1].Name != "code_interpreter" {
		t.Fatalf("middleware order = %#v", values)
	}
}

package dago

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

func TestInterpreterPersistsBindingsAndCallsPTCTools(t *testing.T) {
	echo := datool.MustNew("echo_value", "Return the supplied value.", func(_ context.Context, input struct {
		Value string `json:"value"`
	}) (string, error) {
		return "echo:" + input.Value, nil
	})
	middleware, err := newInterpreter(Interpreter{Enabled: true, PTC: []string{"echo_value"}})
	if err != nil {
		t.Fatal(err)
	}
	request := dagent.ModelRequest{
		Tools:   append([]datool.Tool{echo}, middleware.Tools...),
		Runtime: dagent.Runtime{Config: dacheckpoint.Config{ThreadID: "thread"}},
	}
	_, err = middleware.WrapModelCall(context.Background(), request, func(_ context.Context, got dagent.ModelRequest) (dagent.ModelResponse, error) {
		if got.SystemMessage == nil || !strings.Contains(got.SystemMessage.TextContent(), "tools.echoValue") {
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

func TestInterpreterRejectsPTCNameCollisions(t *testing.T) {
	first := datool.MustNew("foo_bar", "first", func(context.Context, struct{}) (string, error) { return "", nil })
	second := datool.MustNew("foo-bar", "second", func(context.Context, struct{}) (string, error) { return "", nil })
	middleware, err := newInterpreter(Interpreter{Enabled: true, PTC: []string{"foo_bar", "foo-bar"}})
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

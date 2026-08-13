package dago

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

// These tests port the applicable contracts from the pinned
// libs/partners/quickjs test_end_to_end*, test_ptc, and test_repl_middleware
// suites. Go has one context-aware execution path, so upstream sync/async
// duplicates share one assertion here.

func prepareInterpreterTest(t *testing.T, options Interpreter, thread string, tools ...datool.Tool) (dagent.Middleware, datool.Tool, string) {
	t.Helper()
	options.Enabled = true
	middleware, err := newInterpreter(options)
	if err != nil {
		t.Fatal(err)
	}
	request := dagent.ModelRequest{
		Tools:   append(append([]datool.Tool(nil), tools...), middleware.Tools[0]),
		Runtime: dagent.Runtime{Config: dacheckpoint.Config{ThreadID: thread}},
	}
	prompt := ""
	_, err = middleware.WrapModelCall(context.Background(), request, func(_ context.Context, got dagent.ModelRequest) (dagent.ModelResponse, error) {
		if got.SystemMessage != nil {
			prompt = got.SystemMessage.TextContent()
		}
		return dagent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return middleware, middleware.Tools[0], prompt
}

func executeInterpreterTest(t *testing.T, tool datool.Tool, thread, code string, state dastate.Values) (datool.Result, error) {
	t.Helper()
	raw, err := json.Marshal(interpreterInput{Code: code})
	if err != nil {
		t.Fatal(err)
	}
	return tool.Execute(context.Background(), raw, datool.Runtime{
		ThreadID: thread, TaskID: "task", CallID: "outer", State: state,
	})
}

func reduceInterpreterTest(t *testing.T, middleware dagent.Middleware, current any, result datool.Result) any {
	t.Helper()
	field := middleware.Fields[interpreterSnapshotKey]
	if current == nil {
		current = field.Initial()
	}
	next, err := field.Reduce(current, []any{result.Update[interpreterSnapshotKey]})
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestInterpreterRegistersCustomToolAndRendersPTCPrompt(t *testing.T) {
	type greetInput struct {
		Name  string `json:"name" description:"Who to greet"`
		Times int    `json:"times,omitempty" description:"Number of greetings"`
	}
	greet := datool.MustNew("greet_user", "Greet a user.", func(context.Context, greetInput) (string, error) { return "hi", nil })
	middleware, _, prompt := prepareInterpreterTest(t, Interpreter{ToolName: "javascript", PTC: []string{"greet_user", "javascript"}}, "prompt", greet)
	if middleware.Tools[0].Definition().Name != "javascript" || !strings.Contains(strings.ToLower(middleware.Tools[0].Definition().Description), "persistent") {
		t.Fatalf("tool definition = %#v", middleware.Tools[0].Definition())
	}
	for _, want := range []string{
		"Use `javascript`", "globalThis.tools", "tools.greetUser(input: { name: string; times?: number })", "Greet a user.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "tools.javascript") {
		t.Fatalf("interpreter exposed itself through PTC:\n%s", prompt)
	}
}

func TestInterpreterRejectsInvalidLimits(t *testing.T) {
	tests := []Interpreter{
		{Enabled: true, Timeout: -time.Second},
		{Enabled: true, MaxStdoutChars: -1},
		{Enabled: true, MaxResultChars: -1},
		{Enabled: true, MaxSnapshotBytes: -1},
		{Enabled: true, MaxPTCCalls: -1},
	}
	for _, options := range tests {
		if _, err := newInterpreter(options); err == nil {
			t.Fatalf("options %#v accepted", options)
		}
	}
}

func TestInterpreterPTCAllowlistExcludesOtherToolsAndSelf(t *testing.T) {
	allowed := datool.MustNew("allowed_tool", "Allowed.", func(context.Context, struct{}) (string, error) { return "yes", nil })
	denied := datool.MustNew("denied_tool", "Denied.", func(context.Context, struct{}) (string, error) { return "no", nil })
	_, eval, prompt := prepareInterpreterTest(t, Interpreter{PTC: []string{"allowed_tool", "js_eval", "missing_tool"}}, "allowlist", allowed, denied)
	if !strings.Contains(prompt, "tools.allowedTool") || strings.Contains(prompt, "tools.deniedTool") || strings.Contains(prompt, "tools.jsEval") {
		t.Fatalf("allowlist prompt = %s", prompt)
	}
	result, err := executeInterpreterTest(t, eval, "allowlist", `[await tools.allowedTool({}), typeof tools.deniedTool, typeof tools.jsEval].join(":")`, dastate.Values{})
	if err != nil || !strings.Contains(firstInterpreterText(result), "yes:undefined:undefined") {
		t.Fatalf("allowlist content = %q, error = %v", firstInterpreterText(result), err)
	}
}

func TestInterpreterPromptRendersNestedInputSchemaTypes(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"mode":{"enum":["fast","safe"]},
			"items":{"type":"array","items":{"anyOf":[{"type":"integer"},{"type":"null"}]}},
			"config":{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}
		},
		"required":["mode","items"]
	}`)
	typed := datool.Func{Spec: datool.Definition{Name: "typed_input", Description: "Typed.", InputSchema: schema}}
	_, _, prompt := prepareInterpreterTest(t, Interpreter{PTC: []string{"typed_input"}}, "schema", typed)
	for _, want := range []string{`mode: "fast" | "safe"`, `items: (number | null)[]`, `config?: { enabled: boolean }`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
}

func TestInterpreterPTCMarshalsTypedReturns(t *testing.T) {
	type profile struct {
		ID   int      `json:"id"`
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	list := datool.MustNew("list_ids", "List IDs.", func(context.Context, struct{}) ([]int, error) { return []int{1, 21, 35}, nil })
	count := datool.MustNew("get_count", "Get count.", func(context.Context, struct{}) (int, error) { return 7, nil })
	object := datool.MustNew("get_profile", "Get profile.", func(context.Context, struct{}) (profile, error) {
		return profile{ID: 21, Name: "Bob", Tags: []string{"admin", "ops"}}, nil
	})
	nothing := datool.MustNew("get_none", "Return null.", func(context.Context, struct{}) (any, error) { return nil, nil })
	middleware, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{"list_ids", "get_count", "get_profile", "get_none"}}, "native", list, count, object, nothing)
	_ = middleware
	code := "const ids = await tools.listIds({});\n" +
		"const n = await tools.getCount({});\n" +
		"const p = await tools.getProfile({});\n" +
		"const empty = await tools.getNone({});\n" +
		"[Array.isArray(ids), ids.join(','), typeof n, n + 1, typeof p, p.id, p.tags.join(','), empty === null].join(':')"
	result, err := executeInterpreterTest(t, eval, "native", code, dastate.Values{})
	if err != nil {
		t.Fatal(err)
	}
	want := "true:1,21,35:number:8:object:21:admin,ops:true"
	if !strings.Contains(result.Content[0].Text, want) {
		t.Fatalf("result = %s", result.Content[0].Text)
	}
}

func TestInterpreterPTCMarshalsNestedJSONValues(t *testing.T) {
	type event struct {
		SeenAt time.Time `json:"seen_at"`
	}
	type profile struct {
		ID        int       `json:"id"`
		CreatedAt time.Time `json:"created_at"`
		Events    []event   `json:"events"`
	}
	created := time.Date(2024, 1, 1, 12, 30, 0, 0, time.UTC)
	seen := created.Add(27 * time.Hour)
	nested := datool.MustNew("nested_profile", "Nested JSON.", func(context.Context, struct{}) (profile, error) {
		return profile{ID: 21, CreatedAt: created, Events: []event{{SeenAt: seen}}}, nil
	})
	_, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{"nested_profile"}}, "nested", nested)
	result, err := executeInterpreterTest(t, eval, "nested", `const p=await tools.nestedProfile({}); [p.id,p.created_at,p.events[0].seen_at].join(":")`, dastate.Values{})
	if err != nil || !strings.Contains(firstInterpreterText(result), "21:2024-01-01T12:30:00Z:2024-01-02T15:30:00Z") {
		t.Fatalf("nested content = %q, error = %v", firstInterpreterText(result), err)
	}
}

func TestInterpreterPTCOmitsUndefinedArgumentsAndUsesSchemaDefaults(t *testing.T) {
	type input struct {
		Name string `json:"name,omitempty"`
	}
	echo := datool.MustNew("echo_default", "Echo optional name.", func(_ context.Context, value input) (string, error) {
		return "ok:" + value.Name, nil
	})
	_, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{"echo_default"}}, "defaults", echo)
	result, err := executeInterpreterTest(t, eval, "defaults", "const a = await tools.echoDefault({}); const b = await tools.echoDefault({name: undefined}); [a,b].join('|')", dastate.Values{})
	if err != nil || !strings.Contains(result.Content[0].Text, "ok:|ok:") {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestInterpreterPTCUsesLastTextBlock(t *testing.T) {
	messages := datool.Func{Spec: datool.Definition{
		Name: "messages", Description: "Return messages.", InputSchema: json.RawMessage(`{"type":"object"}`),
	}, Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		return datool.Result{Content: []damessage.ContentBlock{
			{Type: damessage.BlockText, Text: "first"}, {Type: damessage.BlockText, Text: "second"},
		}}, nil
	}}
	_, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{"messages"}}, "messages", messages)
	result, err := executeInterpreterTest(t, eval, "messages", `await tools.messages({})`, dastate.Values{})
	if err != nil || !strings.Contains(result.Content[0].Text, "<result>second</result>") {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestInterpreterPTCPropagatesRuntimeAndUniqueCallIDs(t *testing.T) {
	seen := []string{}
	inspect := datool.Func{Spec: datool.Definition{
		Name: "inspect_runtime", Description: "Inspect runtime.", InputSchema: json.RawMessage(`{"type":"object"}`),
	}, Run: func(_ context.Context, _ json.RawMessage, runtime datool.Runtime) (datool.Result, error) {
		value, _ := runtime.Configurable.Get("tenant")
		seen = append(seen, runtime.CallID)
		return datool.TextResult(fmt.Sprintf("%v|%s", value, runtime.CallID)), nil
	}}
	_, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{"inspect_runtime"}}, "runtime", inspect)
	raw, marshalErr := json.Marshal(interpreterInput{Code: "const a = await tools.inspectRuntime({}); const b = await tools.inspectRuntime({}); String(a !== b)"})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	result, err := eval.Execute(context.Background(), raw, datool.Runtime{
		ThreadID: "runtime", TaskID: "task", CallID: "outer", State: dastate.Values{}, Configurable: datool.NewConfigurable(map[string]any{"tenant": "alpha"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] == seen[1] || !strings.HasPrefix(seen[0], "ptc_inspect_runtime_") || !strings.Contains(result.Content[0].Text, "true") {
		t.Fatalf("call IDs = %#v, result = %#v", seen, result)
	}
}

func TestInterpreterPTCRunsPromiseAllConcurrently(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	type input struct {
		Name string `json:"name"`
	}
	wait := datool.MustNew("wait_for_release", "Wait.", func(ctx context.Context, value input) (string, error) {
		started <- value.Name
		select {
		case <-release:
			return value.Name, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	_, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{"wait_for_release"}, Timeout: 2 * time.Second}, "parallel", wait)
	done := make(chan struct {
		result datool.Result
		err    error
	}, 1)
	go func() {
		result, err := executeInterpreterTest(t, eval, "parallel", `const values = await Promise.all([tools.waitForRelease({name:'a'}), tools.waitForRelease({name:'b'})]); values.join('|')`, dastate.Values{})
		done <- struct {
			result datool.Result
			err    error
		}{result, err}
	}()
	first, second := <-started, <-started
	close(release)
	got := <-done
	if got.err != nil || !strings.Contains(got.result.Content[0].Text, "a|b") || first == second {
		t.Fatalf("started = %q %q, result = %#v, %v", first, second, got.result, got.err)
	}
}

func TestInterpreterPTCCallBudgetErrorsAndResets(t *testing.T) {
	echo := datool.MustNew("echo", "Echo.", func(context.Context, struct{}) (string, error) { return "ok", nil })
	middleware, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{"echo"}, MaxPTCCalls: 1}, "budget", echo)
	result, err := executeInterpreterTest(t, eval, "budget", `await tools.echo({}); await tools.echo({}); 'done'`, dastate.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != damessage.ToolStatusError || !strings.Contains(result.Content[0].Text, `type="PTCCallBudgetExceeded"`) || !strings.Contains(result.Content[0].Text, "limit=1 attempted=2 function=tools.echo") {
		t.Fatalf("budget result = %#v", result)
	}
	caught, err := executeInterpreterTest(t, eval, "budget", "try { await tools.echo({}); await tools.echo({}) } catch (e) { [e.name,e.message].join(':') }", dastate.Values{})
	if err != nil || caught.Status == damessage.ToolStatusError || !strings.Contains(caught.Content[0].Text, "HostError:Host function failed") {
		t.Fatalf("caught result = %#v, %v", caught, err)
	}
	first, err := executeInterpreterTest(t, eval, "budget", `await tools.echo({})`, dastate.Values{})
	if err != nil {
		t.Fatal(err)
	}
	state := reduceInterpreterTest(t, middleware, nil, first)
	second, err := executeInterpreterTest(t, eval, "budget", `await tools.echo({})`, dastate.Values{interpreterSnapshotKey: state})
	if err != nil || second.Status == damessage.ToolStatusError {
		t.Fatalf("budget did not reset: %#v, %v", second, err)
	}
}

func TestInterpreterPTCToolFailureCanBeCaughtOrPropagates(t *testing.T) {
	boom := errors.New("boom")
	failing := datool.MustNew("always_fails", "Fail.", func(context.Context, struct{}) (string, error) { return "", boom })
	_, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{"always_fails"}}, "failure", failing)
	caught, err := executeInterpreterTest(t, eval, "failure", `try { await tools.alwaysFails({}) } catch (e) { e.message }`, dastate.Values{})
	if err != nil || !strings.Contains(caught.Content[0].Text, "Host function failed") {
		t.Fatalf("caught = %#v, %v", caught, err)
	}
	_, err = executeInterpreterTest(t, eval, "failure", `await tools.alwaysFails({})`, dastate.Values{})
	if !errors.Is(err, boom) {
		t.Fatalf("uncaught error = %v", err)
	}
}

func TestInterpreterPTCWrongArgumentsPropagate(t *testing.T) {
	type input struct {
		Value string `json:"value"`
	}
	echo := datool.MustNew("echo", "Echo.", func(_ context.Context, value input) (string, error) { return value.Value, nil })
	_, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{"echo"}}, "args", echo)
	_, err := executeInterpreterTest(t, eval, "args", `await tools.echo({not_value:'x'})`, dastate.Values{})
	if err == nil || !errors.Is(err, datool.ErrInvalidArguments) {
		t.Fatalf("argument error = %v", err)
	}
}

func TestInterpreterPTCNamespaceIsReplacedAndNamesAreSafe(t *testing.T) {
	first := datool.MustNew("first_tool", "First.", func(context.Context, struct{}) (string, error) { return "first", nil })
	second := datool.MustNew("!1second-tool", "Second.", func(context.Context, struct{}) (string, error) { return "second", nil })
	middleware, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{"first_tool", "!1second-tool"}}, "namespace", first, second)
	initial, err := executeInterpreterTest(t, eval, "namespace", "const saved = await tools.firstTool({}); [saved, typeof tools._1secondTool].join(':')", dastate.Values{})
	if err != nil || !strings.Contains(initial.Content[0].Text, "first:function") {
		t.Fatalf("initial = %#v, %v", initial, err)
	}
	state := reduceInterpreterTest(t, middleware, nil, initial)
	request := dagent.ModelRequest{Tools: []datool.Tool{second, eval}, Runtime: dagent.Runtime{Config: dacheckpoint.Config{ThreadID: "namespace"}}}
	if _, err := middleware.WrapModelCall(context.Background(), request, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
		return dagent.ModelResponse{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	next, err := executeInterpreterTest(t, eval, "namespace", "[typeof tools.firstTool, await tools._1secondTool({})].join(':')", dastate.Values{interpreterSnapshotKey: state})
	if err != nil || !strings.Contains(next.Content[0].Text, "undefined:second") {
		t.Fatalf("next = %#v, %v", next, err)
	}
}

func TestInterpreterRejectsConcurrentEvalForSameThread(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	block := datool.MustNew("block", "Block.", func(ctx context.Context, _ struct{}) (string, error) {
		started <- struct{}{}
		select {
		case <-release:
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	_, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{"block"}, Timeout: 2 * time.Second}, "busy", block)
	done := make(chan error, 1)
	go func() {
		_, err := executeInterpreterTest(t, eval, "busy", `await tools.block({})`, dastate.Values{})
		done <- err
	}()
	<-started
	_, err := executeInterpreterTest(t, eval, "busy", `1 + 1`, dastate.Values{})
	if err == nil || !strings.Contains(err.Error(), "already evaluating") {
		t.Fatalf("concurrent error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestInterpreterFormatsErrorsOutputAndOpaqueValues(t *testing.T) {
	_, eval, _ := prepareInterpreterTest(t, Interpreter{MaxStdoutChars: 10, MaxResultChars: 40, PTC: []string{}}, "format")
	tests := []struct {
		code string
		want string
	}{
		{code: `throw new TypeError('<bad>')`, want: `<error type="TypeError">`},
		{code: `1 +`, want: `<error type="SyntaxError">`},
		{code: `"x".repeat(5000)`, want: `truncated`},
		{code: `(a, b) => a + b`, want: `<result kind="handle">`},
		{code: `console.log('abcdef'); console.log('ghij'); 2`, want: `<stdout>`},
	}
	for _, test := range tests {
		result, err := executeInterpreterTest(t, eval, "format", test.code, dastate.Values{})
		if err != nil || !strings.Contains(result.Content[0].Text, test.want) {
			t.Fatalf("code %q result = %#v, %v; want %q", test.code, result, err, test.want)
		}
		if strings.Contains(result.Content[0].Text, "<bad>") {
			t.Fatalf("error was not escaped: %s", result.Content[0].Text)
		}
	}
}

func TestInterpreterFormatsTimeoutAndDeadlock(t *testing.T) {
	for _, test := range []struct {
		name    string
		timeout time.Duration
		code    string
		want    string
	}{
		{name: "timeout", timeout: 20 * time.Millisecond, code: `while(true){}`, want: `<error type="Timeout">`},
		{name: "deadlock", timeout: 5 * time.Second, code: `new Promise(() => {})`, want: `<error type="Deadlock">`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, eval, _ := prepareInterpreterTest(t, Interpreter{Timeout: test.timeout, PTC: []string{}}, "runtime-"+test.name)
			result, err := executeInterpreterTest(t, eval, "runtime-"+test.name, test.code, dastate.Values{})
			if err != nil || result.Status != damessage.ToolStatusError || !strings.Contains(firstInterpreterText(result), test.want) {
				t.Fatalf("code %q content = %q, status = %q, error = %v", test.code, firstInterpreterText(result), result.Status, err)
			}
		})
	}
}

func TestInterpreterCancellationPropagates(t *testing.T) {
	_, eval, _ := prepareInterpreterTest(t, Interpreter{Timeout: time.Minute, PTC: []string{}}, "cancel")
	raw, err := json.Marshal(interpreterInput{Code: `while(true){}`})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err = eval.Execute(ctx, raw, datool.Runtime{ThreadID: "cancel", TaskID: "task", CallID: "call", State: dastate.Values{}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

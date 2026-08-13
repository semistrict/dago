package quickjs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEvalMarshalsValuesAndUnwrapsPromises(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)
	tests := []struct {
		name, code, kind string
		want             any
	}{
		{name: "undefined", code: "undefined", kind: "undefined", want: Undefined{}},
		{name: "null", code: "null", kind: "null", want: nil},
		{name: "boolean", code: "true", kind: "boolean", want: true},
		{name: "integer", code: "42", kind: "number", want: int64(42)},
		{name: "float", code: "4.25", kind: "number", want: 4.25},
		{name: "string", code: "'hello'", kind: "string", want: "hello"},
		{name: "bigint", code: "9007199254740993n", kind: "bigint", want: "9007199254740993n"},
		{name: "array", code: "[1, 'two', null]", kind: "array", want: []any{int64(1), "two", nil}},
		{name: "object", code: "({id: 21, tags: ['a', 'b']})", kind: "object", want: map[string]any{"id": int64(21), "tags": []any{"a", "b"}}},
		{name: "promise", code: "Promise.resolve(7)", kind: "number", want: int64(7)},
		{name: "async_iife", code: "(async () => { return await Promise.resolve(456) })()", kind: "number", want: int64(456)},
		{name: "top_level_await", code: "const promised = await Promise.resolve(123); promised", kind: "number", want: int64(123)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome, err := engine.Eval(ctx, test.code, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.ValueKind != test.kind || !reflect.DeepEqual(outcome.Value, test.want) {
				t.Fatalf("outcome = %#v, want kind %q value %#v", outcome, test.kind, test.want)
			}
		})
	}
}

func TestNewEnablesMemoryTrackingByDefault(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)
	if engine.bitmapEnabled.Get() != 1 || engine.bitmapPtr == 0 || engine.bitmapBytes == 0 {
		t.Fatalf("tracking globals = enabled:%d ptr:%d bytes:%d", engine.bitmapEnabled.Get(), engine.bitmapPtr, engine.bitmapBytes)
	}
}

func TestEvalPromiseErrorsAndSideEffects(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)
	if _, err := engine.Eval(ctx, "throw new TypeError('bad')", time.Second); err == nil || !strings.Contains(err.Error(), "TypeError: bad") {
		t.Fatalf("runtime error = %v", err)
	}
	if _, err := engine.Eval(ctx, "1 +", time.Second); err == nil || !strings.Contains(err.Error(), "SyntaxError") {
		t.Fatalf("syntax error = %v", err)
	}
	if _, err := engine.Eval(ctx, "Promise.reject(new Error('rejected'))", time.Second); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("promise error = %v", err)
	}
	outcome, err := engine.Eval(ctx, "(async () => { console.log('hit'); return 1 })()", time.Second)
	if err != nil || outcome.Value != int64(1) || outcome.Stdout != "hit" {
		t.Fatalf("side effect outcome = %#v, %v", outcome, err)
	}
}

func TestEvalConsoleCaptureIsBoundedAndResets(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{MaxStdout: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)
	first, err := engine.Eval(ctx, "console.log('abcdef'); console.log('ghij'); 1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stdout != "abcdef\nghi" || first.StdoutDropped != 1 {
		t.Fatalf("first stdout = %q dropped=%d", first.Stdout, first.StdoutDropped)
	}
	second, err := engine.Eval(ctx, "console.log('ok'); 2", time.Second)
	if err != nil || second.Stdout != "ok" || second.StdoutDropped != 0 || second.Value != int64(2) {
		t.Fatalf("second outcome = %#v, %v", second, err)
	}
}

func TestHostFunctionsMarshalNativeValuesAndOmitUndefinedProperties(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{HostFunctions: map[string]HostFunction{
		"list": func(context.Context, []any) (any, error) { return []any{1, "two", nil}, nil },
		"object": func(context.Context, []any) (any, error) {
			return map[string]any{"id": 21, "tags": []any{"admin", "ops"}}, nil
		},
		"echo": func(_ context.Context, arguments []any) (any, error) { return arguments[0], nil },
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)
	tests := []struct{ code, want string }{
		{"const ids = await list({}); `${Array.isArray(ids)}:${ids.join(',')}`", "true:1,two,"},
		{"const value = await object({}); `${typeof value}:${value.id}:${value.tags.join(',')}`", "object:21:admin,ops"},
		{"const value = await echo({missing: undefined, present: 1}); `${Object.hasOwn(value, 'missing')}:${value.present}`", "false:1"},
	}
	for _, test := range tests {
		outcome, err := engine.Eval(ctx, test.code, time.Second)
		if err != nil || outcome.Value != test.want {
			t.Fatalf("%s = %#v, %v; want %q", test.code, outcome, err, test.want)
		}
	}
}

func TestHostFunctionFailureCanBeCaughtOrPropagated(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("tool exploded")
	engine, err := New(ctx, Options{HostFunctions: map[string]HostFunction{
		"fail": func(context.Context, []any) (any, error) { return nil, boom },
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)
	caught, err := engine.Eval(ctx, "try { await fail({}) } catch (e) { `${e.name}:${e.message}` }", time.Second)
	if err != nil || caught.Value != "HostError:Host function failed" {
		t.Fatalf("caught = %#v, %v", caught, err)
	}
	_, err = engine.Eval(ctx, "await fail({})", time.Second)
	var hostError *HostError
	if !errors.As(err, &hostError) || !errors.Is(err, boom) {
		t.Fatalf("uncaught error = %T %v", err, err)
	}
}

func TestEvalDetectsPromiseDeadlock(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)
	_, err = engine.Eval(ctx, "new Promise(() => {})", time.Second)
	if err == nil || !strings.Contains(err.Error(), "pending with no host work") {
		t.Fatalf("deadlock error = %v", err)
	}
}

func TestEvalPersistsThroughWholeMemorySnapshot(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{MemoryLimit: 64 << 20, Timeout: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Eval(ctx, `const answer = 40; console.log("saved"); answer + 2`, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Value != int64(42) || result.Stdout != "saved" {
		t.Fatalf("first eval = %#v", result)
	}
	snapshot, err := engine.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(ctx); err != nil {
		t.Fatal(err)
	}

	restored, err := New(ctx, Options{Timeout: time.Second}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(ctx)
	result, err = restored.Eval(ctx, `answer`, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Value != int64(40) {
		t.Fatalf("restored eval = %#v", result)
	}
}

func TestEvalPersistsThroughDirtyPageSnapshot(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{MemoryLimit: 64 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Eval(ctx, `const retained = {answer: 40}`, time.Second); err != nil {
		t.Fatal(err)
	}
	base, err := engine.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	engine.Close(ctx)

	restored, err := New(ctx, Options{MemoryLimit: 64 << 20}, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restored.Eval(ctx, `retained.answer += 2`, time.Second); err != nil {
		t.Fatal(err)
	}
	delta, err := restored.DirtySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	restored.Close(ctx)
	if len(delta) >= len(base) {
		t.Fatalf("dirty snapshot = %d bytes, full snapshot = %d", len(delta), len(base))
	}
	updated, err := ApplyDirtySnapshot(base, delta, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	partial := append([]byte(nil), delta...)
	if binary.LittleEndian.Uint32(partial[56:]) == 0 {
		t.Fatal("dirty snapshot contains no pages")
	}
	binary.LittleEndian.PutUint32(partial[deltaHeaderSize+4:], 1)
	if _, err := ApplyDirtySnapshot(base, partial, 64<<20); err == nil {
		t.Fatal("partial dirty page was accepted")
	}
	invalidBase := append([]byte(nil), base...)
	binary.LittleEndian.PutUint32(invalidBase[4:], 2)
	if _, err := ApplyDirtySnapshot(invalidBase, delta, 64<<20); err == nil {
		t.Fatal("invalid base snapshot was accepted")
	}
	final, err := New(ctx, Options{MemoryLimit: 64 << 20}, updated)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close(ctx)
	result, err := final.Eval(ctx, `retained.answer`, time.Second)
	if err != nil || result.Value != int64(42) {
		t.Fatalf("restored dirty result = %#v, %v", result, err)
	}
}

func TestEvalDrivesConcurrentHostPromises(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{HostFunctions: map[string]HostFunction{
		"lookup": func(_ context.Context, args []any) (any, error) {
			input, ok := args[0].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("input = %T", args[0])
			}
			return "found:" + input["query"].(string), nil
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)
	result, err := engine.Eval(ctx, `await Promise.all([lookup({query:"a"}), lookup({query:"b"})])`, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{"found:a", "found:b"}
	if fmt.Sprint(result.Value) != fmt.Sprint(want) {
		t.Fatalf("result = %#v", result.Value)
	}
}

func TestHostWaitDoesNotConsumeJavaScriptTimeout(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{HostFunctions: map[string]HostFunction{
		"slow": func(context.Context, []any) (any, error) {
			time.Sleep(650 * time.Millisecond)
			return 42, nil
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)
	result, err := engine.Eval(ctx, `await slow()`, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if result.Value != int64(42) {
		t.Fatalf("result = %#v", result.Value)
	}
}

func TestSnapshotAfterExceptionPreservesPriorMutations(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Eval(ctx, `var beforeThrow = 7; throw new Error("boom")`, time.Second); err == nil {
		t.Fatal("eval error = nil")
	}
	snapshot, err := engine.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	engine.Close(ctx)
	restored, err := New(ctx, Options{}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(ctx)
	result, err := restored.Eval(ctx, `beforeThrow`, time.Second)
	if err != nil || result.Value != int64(7) {
		t.Fatalf("restored = %#v, %v", result, err)
	}
}

func TestEvalInterruptsLoop(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{Timeout: 20 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)
	_, err = engine.Eval(ctx, `while (true) {}`, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("loop error = %v", err)
	}
}

func TestEvalCancellationInterruptsLoop(t *testing.T) {
	engine, err := New(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	started := time.Now()
	_, err = engine.Eval(ctx, `while (true) {}`, time.Minute)
	if err != context.Canceled {
		t.Fatalf("loop error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("cancellation took %s", time.Since(started))
	}
}

func TestValidateSnapshotRejectsCorruptionAndBounds(t *testing.T) {
	ctx := context.Background()
	engine, err := New(ctx, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := engine.Snapshot()
	engine.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSnapshot(snapshot, len(snapshot)); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func([]byte) []byte
	}{
		{name: "short", edit: func(value []byte) []byte { return value[:10] }},
		{name: "size_cap", edit: func(value []byte) []byte { return value }},
		{name: "magic", edit: func(value []byte) []byte { value[0] = 'X'; return value }},
		{name: "version", edit: func(value []byte) []byte { binary.LittleEndian.PutUint32(value[4:], 2); return value }},
		{name: "build", edit: func(value []byte) []byte { value[8] ^= 0xff; return value }},
		{name: "memory_size", edit: func(value []byte) []byte { binary.LittleEndian.PutUint64(value[40:], 1); return value }},
		{name: "stack", edit: func(value []byte) []byte { binary.LittleEndian.PutUint32(value[48:], ^uint32(0)); return value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := append([]byte(nil), snapshot...)
			max := len(copy)
			if test.name == "size_cap" {
				max--
			}
			if err := ValidateSnapshot(test.edit(copy), max); err == nil {
				t.Fatal("validation error = nil")
			}
		})
	}
}

func BenchmarkEvalPTCAndConsole(b *testing.B) {
	ctx := context.Background()
	engine, err := New(ctx, Options{HostFunctions: map[string]HostFunction{
		"echo": func(_ context.Context, arguments []any) (any, error) { return arguments[0], nil },
	}}, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer engine.Close(ctx)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := engine.Eval(ctx, `const values=[]; for(let i=0;i<20;i++){const value=await echo({value:i}); values.push(value); console.log(value)} values.length`, time.Second); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSnapshotRestoreTurns(b *testing.B) {
	ctx := context.Background()
	engine, err := New(ctx, Options{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := engine.Eval(ctx, `let counter = 0; counter`, time.Second); err != nil {
		b.Fatal(err)
	}
	snapshot, err := engine.Snapshot()
	engine.Close(ctx)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ReportMetric(float64(len(snapshot)), "anchor-bytes")
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		engine, err = New(ctx, Options{}, snapshot)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := engine.Eval(ctx, `counter += 1; counter`, time.Second); err != nil {
			b.Fatal(err)
		}
		delta, err := engine.DirtySnapshot()
		engine.Close(ctx)
		if err != nil {
			b.Fatal(err)
		}
		snapshot, err = ApplyDirtySnapshot(snapshot, delta, 64<<20)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConcurrentEngineMemoryWorkload(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			engine, err := New(ctx, Options{MaxStdout: 4_000}, nil)
			if err != nil {
				b.Error(err)
				return
			}
			_, err = engine.Eval(ctx, `for(let i=0;i<200;i++){console.log("line-"+i)} "ok"`, time.Second)
			engine.Close(ctx)
			if err != nil {
				b.Error(err)
				return
			}
		}
	})
}

package dago

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

// BenchmarkCodeInterpreterCheckpointing measures the complete public tool
// path: Wazero/QuickJS startup, restore, evaluation, dirty-page capture, and
// state-field materialization. This intentionally includes costs that the
// lower-level quickjs benchmarks isolate individually.
func BenchmarkCodeInterpreterCheckpointing(b *testing.B) {
	ctx := context.Background()
	middleware, eval := newInterpreterBenchmark(b, Interpreter{PTC: []string{}}, "benchmark")

	b.Run("cold_full_snapshot", func(b *testing.B) {
		var checkpointBytes int64
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			result := executeInterpreterBenchmark(b, ctx, eval, "cold", `1 + 1`, dastate.Values{})
			checkpointBytes += int64(interpreterBenchmarkRecordBytes(result))
		}
		b.ReportMetric(float64(checkpointBytes)/float64(b.N), "checkpoint-B/op")
	})

	for _, mutationBytes := range []int{4 << 10, 256 << 10, 4 << 20} {
		b.Run(fmt.Sprintf("restored_dirty_snapshot/%s", benchmarkByteName(mutationBytes)), func(b *testing.B) {
			thread := "dirty-" + benchmarkByteName(mutationBytes)
			initial := executeInterpreterBenchmark(b, ctx, eval, thread,
				fmt.Sprintf(`globalThis.benchmarkBuffer = new Uint8Array(%d); benchmarkBuffer.length`, mutationBytes), dastate.Values{})
			state := reduceInterpreterBenchmark(b, middleware, nil, initial)
			code := fmt.Sprintf(`benchmarkBuffer.fill((benchmarkBuffer[0] + 1) & 255, 0, %d); benchmarkBuffer[0]`, mutationBytes)
			var checkpointBytes int64
			b.SetBytes(int64(mutationBytes))
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				result := executeInterpreterBenchmark(b, ctx, eval, thread, code, dastate.Values{interpreterSnapshotKey: state})
				checkpointBytes += int64(interpreterBenchmarkRecordBytes(result))
				state = reduceInterpreterBenchmark(b, middleware, state, result)
			}
			b.ReportMetric(float64(checkpointBytes)/float64(b.N), "checkpoint-B/op")
		})
	}
}

// BenchmarkCodeInterpreterToolOutput measures the PTC bridge and checkpoint
// path with payloads large enough to expose JSON normalization and Go-to-JS
// marshalling costs.
func BenchmarkCodeInterpreterToolOutput(b *testing.B) {
	for _, size := range []int{4 << 10, 256 << 10, 2 << 20} {
		payload := strings.Repeat("x", size)
		textTool := datool.MustNew("large_text", "Return a large text payload.", func(context.Context, struct{}) (string, error) {
			return payload, nil
		})
		b.Run("text/"+benchmarkByteName(size), func(b *testing.B) {
			benchmarkInterpreterToolOutput(b, textTool, size, `(await tools.largeText({})).length`)
		})
	}

	for _, items := range []int{256, 4 << 10, 32 << 10} {
		values := make([]string, items)
		for index := range values {
			values[index] = fmt.Sprintf("value-%07d", index)
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			b.Fatal(err)
		}
		structuredTool := datool.MustNew("large_array", "Return a large structured payload.", func(context.Context, struct{}) ([]string, error) {
			return values, nil
		})
		b.Run(fmt.Sprintf("structured_array/%d_items", items), func(b *testing.B) {
			benchmarkInterpreterToolOutput(b, structuredTool, len(encoded), `(await tools.largeArray({})).length`)
		})
	}
}

func benchmarkInterpreterToolOutput(b *testing.B, tool datool.Tool, payloadBytes int, code string) {
	b.Helper()
	ctx := context.Background()
	thread := "large-output"
	middleware, eval := newInterpreterBenchmark(b, Interpreter{PTC: []string{tool.Definition().Name}}, thread, tool)
	initial := executeInterpreterBenchmark(b, ctx, eval, thread, `0`, dastate.Values{})
	state := reduceInterpreterBenchmark(b, middleware, nil, initial)
	var checkpointBytes int64
	b.SetBytes(int64(payloadBytes))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result := executeInterpreterBenchmark(b, ctx, eval, thread, code, dastate.Values{interpreterSnapshotKey: state})
		checkpointBytes += int64(interpreterBenchmarkRecordBytes(result))
		state = reduceInterpreterBenchmark(b, middleware, state, result)
	}
	b.ReportMetric(float64(checkpointBytes)/float64(b.N), "checkpoint-B/op")
	b.ReportMetric(float64(payloadBytes), "tool-output-B/op")
}

func newInterpreterBenchmark(b *testing.B, options Interpreter, thread string, tools ...datool.Tool) (dagent.Middleware, datool.Tool) {
	b.Helper()
	options.Enabled = true
	middleware, err := newInterpreter(options)
	if err != nil {
		b.Fatal(err)
	}
	request := dagent.ModelRequest{
		Tools:   append(append([]datool.Tool(nil), tools...), middleware.Tools[0]),
		Runtime: dagent.Runtime{Config: dacheckpoint.Config{ThreadID: thread}},
	}
	_, err = middleware.WrapModelCall(context.Background(), request, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
		return dagent.ModelResponse{}, nil
	})
	if err != nil {
		b.Fatal(err)
	}
	return middleware, middleware.Tools[0]
}

func executeInterpreterBenchmark(b *testing.B, ctx context.Context, tool datool.Tool, thread, code string, state dastate.Values) datool.Result {
	b.Helper()
	raw, err := json.Marshal(interpreterInput{Code: code})
	if err != nil {
		b.Fatal(err)
	}
	result, err := tool.Execute(ctx, raw, datool.Runtime{ThreadID: thread, TaskID: "benchmark", CallID: "eval", State: state})
	if err != nil {
		b.Fatal(err)
	}
	return result
}

func reduceInterpreterBenchmark(b *testing.B, middleware dagent.Middleware, current any, result datool.Result) any {
	b.Helper()
	field := middleware.Fields[interpreterSnapshotKey]
	if current == nil {
		current = field.Initial()
	}
	next, err := field.Reduce(current, []any{result.Update[interpreterSnapshotKey]})
	if err != nil {
		b.Fatal(err)
	}
	return next
}

func interpreterBenchmarkRecordBytes(result datool.Result) int {
	record, _ := result.Update[interpreterSnapshotKey].(map[string]any)
	data, _ := record["data"].([]byte)
	return len(data)
}

func benchmarkByteName(size int) string {
	if size%(1<<20) == 0 {
		return fmt.Sprintf("%dMiB", size>>20)
	}
	return fmt.Sprintf("%dKiB", size>>10)
}

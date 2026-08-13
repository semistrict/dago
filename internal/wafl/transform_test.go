package wafl

import (
	"context"
	"fmt"
	"testing"

	"github.com/semistrict/dago/internal/quickjswasm"
	wasmbinary "github.com/tetratelabs/wabin/binary"
	"github.com/tetratelabs/wabin/leb128"
	"github.com/tetratelabs/wabin/wasm"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func TestTransformTracksCrossPageScalarStore(t *testing.T) {
	module := testStoreModule(false)
	tracked, err := Transform(wasmbinary.EncodeModule(module), Config{PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer runtime.Close(ctx)
	instance, err := runtime.Instantiate(ctx, tracked)
	if err != nil {
		t.Fatal(err)
	}
	base := instance.ExportedGlobal(BitmapBaseExport).(api.MutableGlobal)
	enabled := instance.ExportedGlobal(EnabledExport).(api.MutableGlobal)
	base.Set(8192)
	enabled.Set(1)
	if _, err := instance.ExportedFunction("store").Call(ctx); err != nil {
		t.Fatal(err)
	}
	memory := instance.Memory()
	first, _ := memory.ReadByte(8192)
	second, _ := memory.ReadByte(8193)
	if first != 1 || second != 1 {
		t.Fatalf("dirty bytes = [%d %d], want [1 1]", first, second)
	}
}

func TestTransformTracksBulkFill(t *testing.T) {
	module := testStoreModule(true)
	tracked, err := Transform(wasmbinary.EncodeModule(module), Config{PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer runtime.Close(ctx)
	instance, err := runtime.Instantiate(ctx, tracked)
	if err != nil {
		t.Fatal(err)
	}
	instance.ExportedGlobal(BitmapBaseExport).(api.MutableGlobal).Set(8192)
	instance.ExportedGlobal(EnabledExport).(api.MutableGlobal).Set(1)
	if _, err := instance.ExportedFunction("store").Call(ctx); err != nil {
		t.Fatal(err)
	}
	first, _ := instance.Memory().ReadByte(8192)
	second, _ := instance.Memory().ReadByte(8193)
	if first != 1 || second != 1 {
		t.Fatalf("dirty bytes = [%d %d], want [1 1]", first, second)
	}
}

func BenchmarkTransformQuickJSGuest(b *testing.B) {
	b.SetBytes(int64(len(quickjswasm.Guest)))
	b.ReportAllocs()
	var outputBytes int
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		tracked, err := Transform(quickjswasm.Guest, Config{PageSize: 4096})
		if err != nil {
			b.Fatal(err)
		}
		outputBytes = len(tracked)
	}
	b.ReportMetric(float64(len(quickjswasm.Guest)), "input-B/op")
	b.ReportMetric(float64(outputBytes), "output-B/op")
}

func BenchmarkTrackedStoreOverhead(b *testing.B) {
	for _, bulk := range []bool{false, true} {
		scenario := "scalar_cross_page"
		if bulk {
			scenario = "bulk_cross_page"
		}
		source := wasmbinary.EncodeModule(testStoreModule(bulk))
		tracked, err := Transform(source, Config{PageSize: 4096})
		if err != nil {
			b.Fatal(err)
		}
		for _, test := range []struct {
			name    string
			module  []byte
			tracked bool
		}{{name: "plain", module: source}, {name: "tracked", module: tracked, tracked: true}} {
			b.Run(fmt.Sprintf("%s/%s", scenario, test.name), func(b *testing.B) {
				ctx := context.Background()
				runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
				defer runtime.Close(ctx)
				instance, err := runtime.Instantiate(ctx, test.module)
				if err != nil {
					b.Fatal(err)
				}
				if test.tracked {
					instance.ExportedGlobal(BitmapBaseExport).(api.MutableGlobal).Set(8192)
					instance.ExportedGlobal(EnabledExport).(api.MutableGlobal).Set(1)
				}
				store := instance.ExportedFunction("store")
				b.ReportAllocs()
				b.ResetTimer()
				for index := 0; index < b.N; index++ {
					if _, err := store.Call(ctx); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func testStoreModule(bulk bool) *wasm.Module {
	body := []byte{wasm.OpcodeI32Const}
	body = append(body, leb128.EncodeInt32(4095)...)
	if bulk {
		body = append(body, wasm.OpcodeI32Const)
		body = append(body, leb128.EncodeInt32(7)...)
		body = append(body, wasm.OpcodeI32Const)
		body = append(body, leb128.EncodeInt32(8)...)
		body = append(body, wasm.OpcodeMiscPrefix)
		body = append(body, leb128.EncodeUint32(uint32(wasm.OpcodeMiscMemoryFill))...)
		body = append(body, leb128.EncodeUint32(0)...)
	} else {
		body = append(body, wasm.OpcodeI64Const)
		body = append(body, leb128.EncodeInt64(42)...)
		body = append(body, wasm.OpcodeI64Store)
		body = append(body, leb128.EncodeUint32(0)...)
		body = append(body, leb128.EncodeUint32(0)...)
	}
	body = append(body, wasm.OpcodeEnd)
	return &wasm.Module{
		TypeSection:     []*wasm.FunctionType{{}},
		FunctionSection: []wasm.Index{0},
		MemorySection:   &wasm.Memory{Min: 1},
		ExportSection: []*wasm.Export{
			{Name: "memory", Type: wasm.ExternTypeMemory, Index: 0},
			{Name: "store", Type: wasm.ExternTypeFunc, Index: 0},
		},
		CodeSection: []*wasm.Code{{Body: body}},
	}
}

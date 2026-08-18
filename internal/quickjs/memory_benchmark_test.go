package quickjs

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// BenchmarkMemoryTrackingOverhead isolates the guest-side WAFL store
// instrumentation. evalSync avoids source-transform and Promise scheduling
// costs that would otherwise dominate the memory-fill measurement.
func BenchmarkMemoryTrackingOverhead(b *testing.B) {
	for _, writeBytes := range []int{4 << 10, 256 << 10, 4 << 20} {
		for _, tracking := range []bool{false, true} {
			name := "synthetic_untracked_baseline"
			if tracking {
				name = "default_tracked"
			}
			b.Run(fmt.Sprintf("%s/%s", benchmarkSizeName(writeBytes), name), func(b *testing.B) {
				ctx := context.Background()
				engine, err := New(ctx, Options{}, nil)
				if err != nil {
					b.Fatal(err)
				}
				defer engine.Close(ctx)
				if _, err := engine.evalSync(ctx, fmt.Sprintf(`globalThis.trackedBytes = new Uint8Array(%d)`, writeBytes)); err != nil {
					b.Fatal(err)
				}
				engine.bitmapEnabled.Set(0)
				if tracking {
					engine.bitmapEnabled.Set(1)
				}
				code := fmt.Sprintf(`trackedBytes.fill((trackedBytes[0] + 1) & 255, 0, %d)`, writeBytes)
				b.SetBytes(int64(writeBytes))
				b.ReportAllocs()
				b.ResetTimer()
				for index := 0; index < b.N; index++ {
					if _, err := engine.evalSync(ctx, code); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkSnapshotCapture separates whole-memory copying from dirty-page
// capture for small and large mutations of the same retained heap.
func BenchmarkSnapshotCapture(b *testing.B) {
	const heapBytes = 8 << 20
	ctx := context.Background()
	engine, err := New(ctx, Options{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer engine.Close(ctx)
	if _, err := engine.Eval(ctx, fmt.Sprintf(`globalThis.snapshotBytes = new Uint8Array(%d); snapshotBytes.length`, heapBytes), time.Second); err != nil {
		b.Fatal(err)
	}
	anchor, err := engine.Snapshot()
	if err != nil {
		b.Fatal(err)
	}
	if _, err := engine.DirtySnapshot(); err != nil {
		b.Fatal(err)
	}

	b.Run("full", func(b *testing.B) {
		b.SetBytes(int64(len(anchor)))
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			if _, err := engine.Snapshot(); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(len(anchor)), "snapshot-B/op")
	})

	for _, writeBytes := range []int{4 << 10, 256 << 10, 4 << 20} {
		b.Run("dirty/"+benchmarkSizeName(writeBytes), func(b *testing.B) {
			code := fmt.Sprintf(`snapshotBytes.fill((snapshotBytes[0] + 1) & 255, 0, %d)`, writeBytes)
			var snapshotBytes int64
			b.SetBytes(int64(writeBytes))
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if _, err := engine.evalSync(ctx, code); err != nil {
					b.Fatal(err)
				}
				delta, err := engine.DirtySnapshot()
				if err != nil {
					b.Fatal(err)
				}
				snapshotBytes += int64(len(delta))
			}
			b.ReportMetric(float64(snapshotBytes)/float64(b.N), "snapshot-B/op")
		})
	}
}

// BenchmarkDirtyBitmapScan scales the retained heap while keeping the write
// set empty or sparse. It makes bitmap-scan cost visible independently from a
// large dirty payload.
func BenchmarkDirtyBitmapScan(b *testing.B) {
	for _, heapBytes := range []int{1 << 20, 8 << 20, 32 << 20} {
		b.Run(benchmarkSizeName(heapBytes), func(b *testing.B) {
			ctx := context.Background()
			engine, err := New(ctx, Options{}, nil)
			if err != nil {
				b.Fatal(err)
			}
			defer engine.Close(ctx)
			if _, err := engine.evalSync(ctx, fmt.Sprintf(`globalThis.sparseBytes = new Uint8Array(%d)`, heapBytes)); err != nil {
				b.Fatal(err)
			}
			anchor, err := engine.Snapshot()
			if err != nil {
				b.Fatal(err)
			}
			if _, err := engine.DirtySnapshot(); err != nil {
				b.Fatal(err)
			}
			linearMemoryBytes := len(anchor) - snapshotSize

			b.Run("clean", func(b *testing.B) {
				var snapshotBytes int64
				b.ReportAllocs()
				b.ResetTimer()
				for index := 0; index < b.N; index++ {
					delta, err := engine.DirtySnapshot()
					if err != nil {
						b.Fatal(err)
					}
					snapshotBytes += int64(len(delta))
				}
				b.ReportMetric(float64(linearMemoryBytes), "linear-memory-B/op")
				b.ReportMetric(float64(snapshotBytes)/float64(b.N), "snapshot-B/op")
			})

			b.Run("sparse_write", func(b *testing.B) {
				var snapshotBytes int64
				b.ReportAllocs()
				b.ResetTimer()
				for index := 0; index < b.N; index++ {
					if _, err := engine.evalSync(ctx, `sparseBytes[0] = (sparseBytes[0] + 1) & 255`); err != nil {
						b.Fatal(err)
					}
					delta, err := engine.DirtySnapshot()
					if err != nil {
						b.Fatal(err)
					}
					snapshotBytes += int64(len(delta))
				}
				b.ReportMetric(float64(linearMemoryBytes), "linear-memory-B/op")
				b.ReportMetric(float64(snapshotBytes)/float64(b.N), "snapshot-B/op")
			})
		})
	}
}

// BenchmarkApplyDirtySnapshot exposes the reducer-side cost of materializing a
// full checkpoint from a retained anchor and a varying number of dirty pages.
func BenchmarkApplyDirtySnapshot(b *testing.B) {
	const heapBytes = 8 << 20
	ctx := context.Background()
	seed, err := New(ctx, Options{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := seed.Eval(ctx, fmt.Sprintf(`globalThis.patchBytes = new Uint8Array(%d); patchBytes.length`, heapBytes), time.Second); err != nil {
		b.Fatal(err)
	}
	anchor, err := seed.Snapshot()
	seed.Close(ctx)
	if err != nil {
		b.Fatal(err)
	}

	for _, writeBytes := range []int{4 << 10, 256 << 10, 4 << 20} {
		engine, err := New(ctx, Options{}, anchor)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := engine.evalSync(ctx, fmt.Sprintf(`patchBytes.fill(1, 0, %d)`, writeBytes)); err != nil {
			b.Fatal(err)
		}
		delta, err := engine.DirtySnapshot()
		engine.Close(ctx)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(benchmarkSizeName(writeBytes), func(b *testing.B) {
			b.SetBytes(int64(len(delta)))
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if _, err := ApplyDirtySnapshot(anchor, delta, 64<<20); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(anchor)), "materialized-B/op")
			b.ReportMetric(float64(len(delta)), "patch-B/op")
		})
	}
}

func benchmarkSizeName(size int) string {
	if size%(1<<20) == 0 {
		return fmt.Sprintf("%dMiB", size>>20)
	}
	return fmt.Sprintf("%dKiB", size>>10)
}

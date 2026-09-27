package secret

import (
	"strconv"
	"testing"
	"time"
)

// These four exist so the cost tests read as cost tests rather than as string
// formatting. itoa and the clock are here rather than imported so the hot loop in
// the fixture builder does not allocate a slice per repetition.

func itoa(i int) string { return strconv.Itoa(i) }

func now() time.Time { return time.Now() }

func secondsSince(start time.Time) float64 { return time.Since(start).Seconds() }

type allocResult struct {
	bytesPerOp  float64
	allocsPerOp float64
}

// benchmarkDetect measures bytes and allocations per call, as the MINIMUM of
// several runs.
//
// The minimum, not the mean, because AllocsPerOp is an average over a
// GC-paced loop and a single collection landing inside the window inflates it by
// an order of magnitude. An earlier version of this took one run and failed
// intermittently at 0.9x against a 2.0x bar, which is a measurement artefact
// rather than a finding. The minimum is the standard idiom for exactly this and
// it is still well clear of the regression, which measures 2.7x-10.4x.
func benchmarkDetect(text string) allocResult {
	best := allocResult{bytesPerOp: -1}
	for i := 0; i < 5; i++ {
		r := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				Detect(text)
			}
		})
		got := float64(r.AllocedBytesPerOp())
		if best.bytesPerOp < 0 || got < best.bytesPerOp {
			best = allocResult{bytesPerOp: got, allocsPerOp: float64(r.AllocsPerOp())}
		}
	}
	return best
}

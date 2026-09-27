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

func benchmarkDetect(text string) allocResult {
	r := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			Detect(text)
		}
	})
	return allocResult{
		bytesPerOp:  float64(r.AllocedBytesPerOp()),
		allocsPerOp: float64(r.AllocsPerOp()),
	}
}

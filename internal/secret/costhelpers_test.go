package secret

import (
	"runtime"
	"runtime/debug"
	"strconv"
	"time"
)

// These exist so the cost tests read as cost tests rather than as string
// formatting. itoa and the clock are here rather than imported so the hot loop in
// the fixture builder does not allocate a slice per repetition.

func itoa(i int) string { return strconv.Itoa(i) }

func now() time.Time { return time.Now() }

func secondsSince(start time.Time) float64 { return time.Since(start).Seconds() }

type allocResult struct {
	bytesPerOp  float64
	allocsPerOp float64
}

// allocatedBytesPerCall measures the exact bytes one call of f allocates, as the
// minimum of a few runs.
//
// The instrument is MemStats rather than testing.Benchmark, and the reason is a
// flake that cost a whole round. AllocsPerOp is an average over a GC-paced loop,
// so it is a function of the whole process's allocation state: run it inside a
// package where other tests are also allocating and it measures them too. Taking
// the minimum of five Benchmark runs narrowed that and did not remove it — the
// byte ratio still failed intermittently at 0.8x against a 2.0x bar while
// passing every time it ran alone.
//
// MemStats.TotalAlloc is a running total of bytes actually allocated, and it is
// exact rather than sampled. With GC off for the measured window nothing is
// collected, so the delta across a fixed number of calls is precisely what those
// calls allocated — no averaging, no sampling, and nothing for a neighbouring
// test's collection to perturb.
//
// The remaining noise is not GC but the FLOOR: the process allocates a few KB for
// its own reasons inside any window, and eight calls of a fixture that allocates
// 500 bytes each is a 4 KB signal, so one background allocation doubled it. The
// 4 KB figure read 569, 8975, 3582, 569, 8975 across six runs while the 32 KB
// figure was steady to within a few percent — the same measurement, one order of
// magnitude less signal. The call count is therefore chosen per fixture to put
// the signal well above the floor, not to make the test fast: 100 calls of the
// small fixture is ~0.1 s, 8 of the large one ~0.06 s. A ratio of bytes PER CALL
// is unaffected by the two counts differing.
//
// The minimum over three runs is kept because a first run's own setup lands in
// the window.
//
// GC is disabled process-wide, so this must not run beside a parallel test. The
// package has none (nothing calls t.Parallel), and the window is short: a few
// hundred full-buffer Detects, not a benchmark loop.
func allocatedBytesPerCall(calls int, f func()) allocResult {
	best := allocResult{bytesPerOp: -1}
	for run := 0; run < 3; run++ {
		// GC, then a warm call, then the window. The order matters twice over.
		// runtime.GC clears every sync.Pool, and Go's regexp keeps its match
		// machine in one — so the first call after a collection rebuilds the
		// machines for whichever patterns the input touches. That is a ONE-TIME
		// cost, not a per-call one, and measured inside the window it made the
		// 32 KB figure read either 421 or 4,203 B/op depending on which run got
		// there first, which is a 10x swing in a ratio test and a coin flip on
		// the assertion. Warming up after the collection puts those allocations
		// outside the window, where they belong.
		runtime.GC()
		f()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)

		restore := debug.SetGCPercent(-1)
		for i := 0; i < calls; i++ {
			f()
		}
		debug.SetGCPercent(restore)

		runtime.ReadMemStats(&after)
		bytes := float64(after.TotalAlloc-before.TotalAlloc) / float64(calls)
		if best.bytesPerOp < 0 || bytes < best.bytesPerOp {
			best = allocResult{
				bytesPerOp:  bytes,
				allocsPerOp: float64(after.Mallocs-before.Mallocs) / float64(calls),
			}
		}
	}
	return best
}

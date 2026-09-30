package secret

// These exist so the cost tests read as cost tests rather than as string
// formatting. itoa and the clock are here rather than imported so the hot loop in
// the fixture builder does not allocate a slice per repetition.

import (
	"runtime"
	"runtime/debug"
	"strconv"
	"time"
)

func itoa(i int) string { return strconv.Itoa(i) }

func now() time.Time { return time.Now() }

func secondsSince(start time.Time) float64 { return time.Since(start).Seconds() }

// pinnedToOneThread runs f with the process pinned to a single OS thread and a
// single P, and restores both afterwards.
//
// This is what testing.AllocsPerRun does for the same reason, and the reason is
// not tidiness. Go's regexp keeps each pattern's match machine in a sync.Pool,
// and a sync.Pool has a per-P local with a PRIVATE slot that other goroutines
// cannot take. A goroutine that migrates between Ps therefore misses the machine
// it left behind and allocates a fresh one — a machine sized for the input, so
// tens of kilobytes for a long line. Measured on this laptop, an eight-call
// Detect window on a 32 KB body read 130 B/op on 30 windows in a row while
// pinned and 130..9,962 B/op across 30 windows unpinned, with 18 of those 30
// contaminated. An allocation measurement that reads the whole process's
// TotalAlloc therefore measures the scheduler as well as the function, and the
// scheduler is noisier by four orders of magnitude than the thing under test.
//
// It is restored rather than left pinned: GOMAXPROCS(1) for the length of one
// measurement is invisible to the rest of the package, and no test here runs in
// parallel (nothing calls t.Parallel), so nothing else can be starved by it.
func pinnedToOneThread(f func()) {
	prev := runtime.GOMAXPROCS(1)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer runtime.GOMAXPROCS(prev)
	f()
}

type allocResult struct {
	bytesPerOp float64
}

// allocatedBytesPerCall measures the bytes one call of f allocates, as the
// minimum over a few windows.
//
// MemStats.TotalAlloc is the instrument rather than testing.Benchmark, and the
// reason is a flake that cost a whole round. AllocsPerOp is an average over a
// GC-paced loop, so it is a function of the whole process's allocation state: run
// it inside a package where other tests are also allocating and it measures them
// too. Taking the minimum of five Benchmark runs narrowed that and did not remove
// it — the byte ratio still failed intermittently at 0.8x against a 2.0x bar
// while passing every time it ran alone.
//
// MemStats.TotalAlloc is a running total of bytes actually allocated, and it is
// exact rather than sampled. With GC off for the measured window nothing is
// collected, so the delta across a fixed number of calls is precisely what those
// calls allocated — no averaging, no sampling, and nothing for a neighbouring
// test's collection to perturb.
//
// The window has to be isolated, and pinnedToOneThread is what isolates it. The
// first version of this helper pinned nothing, and its floor was the runtime's
// own regexp machine pool: with GC on, a collection empties the pool, the warm
// call after it rebuilds the machine for the pattern the input touches, and the
// measurement is then a function of whether the goroutine moved between Ps while
// the pool was being rebuilt. That put a 7-80 KB one-off into roughly one window
// in five, and because the two fixtures were measured with different call
// counts (100 against 8) it landed almost entirely on the small-signal side, so
// the RATIO failed on a machine that had changed nothing. Issue #815 is that
// flake, measured: on the unpinned instrument the 32 KB figure read anything from
// 130 to 9,962 B/op between adjacent windows of the same run.
//
// So three things are load-bearing here and each is measured rather than hoped
// for. The measurement is pinned to one thread and one P, which empties the
// per-P pool effect entirely. GC is off for the whole measurement, not
// re-armed per window, and the only collection is an explicit runtime.GC()
// between windows — so no automatic cycle can fire inside one and be counted as
// the fixture's cost. And the minimum is over windows rather than an average,
// because everything that can go wrong here adds bytes.
//
// GC is disabled process-wide, so this must not run beside a parallel test. The
// package has none, and the window is short: a few dozen full-buffer Detects, not
// a benchmark loop.
func allocatedBytesPerCall(calls int, f func()) allocResult {
	best := allocResult{bytesPerOp: -1}
	pinnedToOneThread(func() {
		restore := debug.SetGCPercent(-1)
		defer debug.SetGCPercent(restore)
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
			for i := 0; i < calls; i++ {
				f()
			}
			runtime.ReadMemStats(&after)

			bytes := float64(after.TotalAlloc-before.TotalAlloc) / float64(calls)
			if best.bytesPerOp < 0 || bytes < best.bytesPerOp {
				best = allocResult{bytesPerOp: bytes}
			}
		}
	})
	return best
}

// fastestOf returns the cheapest CPU time one call of f took, in seconds, as the
// minimum over rounds of calls.
//
// The clock is CPU time rather than wall time — cpuClock has the measurement
// behind it, and this is the second half of why. Wall time counts the time this
// process spent not running, so on a loaded runner the 8x-input sample reads
// anywhere from 2x to 36x the 1x sample for a code path whose true ratio is
// 8.2x, and no bar placed between those numbers can be both sensitive and
// reliable. Measured on this laptop under 14 competing spinners, the same ratio
// read 8.02x to 8.21x on the CPU clock. That is the property being asserted —
// the shape of the cost curve, not how busy the runner is — so that is what it
// is measured on.
//
// The MINIMUM over rounds, not the mean or the median, because with the clock
// this accurate what remains is small enough that a mean would average away the
// signal along with the jitter, and because every remaining source of error can
// only make a call look more expensive: a page fault, a GC assist charged to the
// window, a cache miss the pinned thread took cold. The quadratic cost this
// bounds is in every round, because it is the work.
//
// callsPerRound repeats f inside one window and divides, so the clock's
// resolution is not the measurement's resolution. The smallest window the cost
// tests take is milliseconds of CPU, and on a platform whose clock is coarse —
// macOS's getrusage is 1 ms — a single call could land entirely inside one tick
// and read as zero. Repeating until the window is tens of milliseconds costs
// nothing here: the fixture is pure computation with nothing to amortise.
//
// Each round is preceded by an explicit collection and an untimed warm call, so
// a window never carries the sync.Pool rebuild that runtime.GC forces — the same
// effect that made the allocation helper's floor unpredictable — and the whole
// thing runs pinned so the rounds are comparable with each other.
func fastestOf(rounds, callsPerRound int, f func()) float64 {
	best := 0.0
	pinnedToOneThread(func() {
		for round := 0; round < rounds; round++ {
			runtime.GC()
			f()
			start := cpuClock()
			for i := 0; i < callsPerRound; i++ {
				f()
			}
			perCall := float64(cpuClock()-start) / float64(callsPerRound) / float64(time.Second)
			if round == 0 || perCall < best {
				best = perCall
			}
		}
	})
	return best
}

func gcNow() { runtime.GC() }

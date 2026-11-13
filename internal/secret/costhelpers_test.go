package secret

// The clock the cost tests measure with is cpuClock, in cpuclock_unix_test.go
// and cpuclock_other_test.go.

import (
	"runtime"
	"time"
)

// pinnedToOneThread runs f with the process pinned to a single OS thread and a
// single P, and restores both afterwards. Go's regexp keeps each pattern's match
// machine in a sync.Pool with a per-P private slot, so a goroutine that migrates
// between Ps misses the machine it left behind and allocates a fresh one; an
// unpinned measurement times the scheduler as well as the function. No test in
// this package runs in parallel, so the pin cannot starve anything else.
func pinnedToOneThread(f func()) {
	prev := runtime.GOMAXPROCS(1)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer runtime.GOMAXPROCS(prev)
	f()
}

// fastestOf returns the cheapest CPU time one call of f took, in seconds, as
// the minimum over rounds of callsPerRound calls.
//
// The clock is CPU time rather than wall time, which counts time this process
// was not running: on a loaded runner the 8x-input sample reads anywhere from
// 2x to 36x the 1x sample for code whose true ratio is 8.2x (#815). The minimum
// is over rounds because every remaining error can only make a call look more
// expensive, and callsPerRound keeps the window well above the clock's
// resolution (1 ms on macOS). Each round starts with a collection and an
// untimed warm call so a window never carries the sync.Pool rebuild that
// runtime.GC forces.
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

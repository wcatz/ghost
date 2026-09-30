//go:build unix

package secret

import (
	"syscall"
	"time"
)

// cpuClock is the CLOCK the cost tests measure with, and it is CPU time rather
// than wall time for a reason that is the whole difference between a gate and a
// coin flip. Wall time counts time this process was not running: on a loaded
// runner the 8x-input sample reads anywhere from 2x to 36x the 1x-input sample
// for code whose cost ratio is 8.2x, and a bar placed between those numbers has
// to fail sometimes. CPU time counts the work and only the work, so the same
// ratio measured under 14 competing spinners read 8.02x to 8.21x — the range an
// idle machine gives, because idle is not what the property is about.
//
// One process's own user+system time, read from getrusage, which is a counter
// rather than a derived estimate and so has no allocation and no error to
// accumulate. Measured on this laptop: 33 us of spinning shows up as a 33 us
// delta, so the clock is fine-grained enough for the smallest window the cost
// tests take.
//
// The unix build tag is the honest one: getrusage is not on Windows, which is
// why cpuclock_other_test.go exists rather than a runtime.GOOS branch. The
// fallback there is wall time and it is a real weakening — see that file.
func cpuClock() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		// Not reachable on a platform that compiles this file; and returning zero
		// would make every ratio read as 0/0, so it must be loud.
		panic("getrusage: " + err.Error())
	}
	total := func(tv syscall.Timeval) time.Duration {
		return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
	}
	return total(ru.Utime) + total(ru.Stime)
}

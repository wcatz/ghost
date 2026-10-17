//go:build unix

package memory

import (
	"syscall"
	"time"
)

// cpuClock is the CLOCK the cost test in this package measures with, and it is
// CPU time rather than wall time for the same reason internal/secret's cost
// tests measure with one (see internal/secret/cpuclock_unix_test.go, whose
// measurement this copies rather than re-derives): wall time counts the time
// this process was not running, so a single wall-clock sample of a
// millisecond-scale call on a loaded shared runner is a coin flip. On the
// runner that flapped (#1458), one loaded sample of a call that costs tens of
// milliseconds read 257ms against a 250ms bar and turned a busy runner into a
// red release build. CPU time counts the work and only the work, so the same
// call reads the same whether the rest of the machine is busy or idle — and the
// quadratic alignment the cost test bounds IS work, so it is fully visible on
// this clock (the regression reads seconds on it).
//
// One process's own user+system time, read from getrusage, which is a counter
// rather than a derived estimate and so has no allocation and no error to
// accumulate.
//
// The unix build tag is the honest one: getrusage is not on Windows, which is
// why cpuclock_other_test.go exists rather than a runtime.GOOS branch. The
// fallback there is wall time and it is a real weakening — see that file.
func cpuClock() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		// Not reachable on a platform that compiles this file; and returning zero
		// would make the cost test read a free call, so it must be loud.
		panic("getrusage: " + err.Error())
	}
	total := func(tv syscall.Timeval) time.Duration {
		return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
	}
	return total(ru.Utime) + total(ru.Stime)
}

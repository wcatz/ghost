//go:build unix

package secret

import (
	"syscall"
	"time"
)

// cpuClock is the clock the cost tests measure with: this process's own
// user+system time from getrusage, rather than wall time, which counts time the
// process was not running and made the same 8.2x ratio read 2x to 36x on a
// loaded runner. The unix build tag is the honest one: getrusage is not on
// Windows, where cpuclock_other_test.go falls back to wall time.
func cpuClock() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		// A zero would make every ratio read as 0/0, so it must be loud.
		panic("getrusage: " + err.Error())
	}
	total := func(tv syscall.Timeval) time.Duration {
		return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
	}
	return total(ru.Utime) + total(ru.Stime)
}

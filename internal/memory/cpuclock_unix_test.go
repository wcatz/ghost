//go:build unix

package memory

import (
	"syscall"
	"time"
)

func cpuClock() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		panic("getrusage: " + err.Error())
	}
	total := func(tv syscall.Timeval) time.Duration {
		return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
	}
	return total(ru.Utime) + total(ru.Stime)
}

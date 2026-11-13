//go:build !unix

package secret

import "time"

// cpuClock is the fallback for platforms without getrusage. It is WALL time,
// a real weakening of every ratio the cost tests measure: the tests still catch
// a quadratic scan (64x for 8x the input) and log their raw figures, and CI
// runs on Linux, where cpuclock_unix_test.go supplies the CPU clock.
func cpuClock() time.Duration { return time.Duration(time.Now().UnixNano()) }

//go:build !unix

package secret

import "time"

// cpuClock is the fallback for platforms without getrusage, and it is WALL time,
// which is a real weakening of every ratio the cost tests measure. It is written
// down here rather than left to be discovered as a flake on somebody's Windows
// laptop.
//
// The reason it is a wall clock rather than nothing is that the tests are still
// worth running: a quadratic scan is 64x for 8x the input, and no amount of
// scheduling noise closes a gap that wide. The reason it is not relied on is that
// it IS weaker — the small fixture's window is milliseconds long and a lost
// thread is tens of milliseconds — so the cost tests take the minimum over
// several rounds, and every cost test logs its raw figures so a marginal
// failure is diagnosable from the log line alone.
//
// CI runs this package on Linux, where cpuclock_unix_test.go supplies the CPU
// clock, so the gate that protects the repository is the one without this
// compromise.
func cpuClock() time.Duration { return time.Duration(time.Now().UnixNano()) }

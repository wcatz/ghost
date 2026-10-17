//go:build !unix

package memory

import "time"

// cpuClock is the fallback for platforms without getrusage, and it is WALL
// time, which is a real weakening of the cost test's bar. It is written down
// here rather than left to be discovered as a flake on somebody's Windows
// laptop.
//
// The reason it is a wall clock rather than nothing is that the test is still
// worth running: the quadratic alignment it bounds is 300x the honest cost on
// this bucket, and no amount of scheduling noise closes a gap that wide. The
// reason it is not relied on is that it IS weaker — the honest window is
// milliseconds long and a lost thread is tens of milliseconds — so the cost
// test takes the minimum over several rounds and logs its raw figure, so a
// marginal failure is diagnosable from the log line alone.
//
// CI runs this package on Linux, where cpuclock_unix_test.go supplies the CPU
// clock, so the gate that protects the repository is the one without this
// compromise.
func cpuClock() time.Duration { return time.Duration(time.Now().UnixNano()) }

package mcpinit

import "os"

// readPIDFile reads a PID file — the per-project lifecycle claim and the obsidian
// sync's own marker — and reports whether the path was one at all.
//
// A PID file is always a regular file Ghost itself wrote: the claim is
// published by write-temp-then-rename (atomicWritePID), so a reader never sees
// a half-written one. Anything else at the path is not a PID file, and the
// difference is not cosmetic: the path lives in the data directory, which every
// local process of the user's own can write, and opening a FIFO there for
// reading blocks until a writer appears — which hung the stop hook, the
// session-end hook and `ghost history compact` on a planted path — while a
// character device is read without a bound.
//
// The check is a stat BEFORE the read, which is the guard the other two readers
// of the lifecycle claim already carry (claimPidFileReclaim and
// unfinishedLifecycleRun), and the one openTranscript and readHostConfigFile
// carry for their own paths. A symlink is followed, so it is the target that
// has to be regular — which is what a stat, rather than a lstat, decides.
func readPIDFile(path string) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

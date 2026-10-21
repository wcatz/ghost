package mcpinit

import (
	"io"
	"os"
)

// maxPIDFileBytes bounds what readPIDFile reads from a PID file. A PID file
// holds a pid and at most a creation-time token, so it is a few bytes; the
// bound is what keeps a file that is not one — a regular file somebody
// planted, a device — from being read into memory whole.
const maxPIDFileBytes = 4096

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
// The file is OPENED and the DESCRIPTOR is judged, rather than the path being
// stat'ed and then read: a path swapped for a FIFO between the two calls
// would still hang the reader, so the check has to be made on what was
// opened. The flags are the ones openMarker opens a failure marker with
// (markerOpenFlags), which carry the split the marker code already has:
// O_NOFOLLOW makes the kernel refuse a symlink planted at the path, and
// O_NONBLOCK keeps a FIFO from blocking the open until a writer appears — such
// a FIFO is then refused by the fstat on the open descriptor. O_NONBLOCK
// changes nothing for a regular file.
//
// What is read is bounded, and a file over the bound is no PID file: the pid
// and its token are a few bytes, and nothing else is a claim.
func readPIDFile(path string) ([]byte, bool) {
	f, err := os.OpenFile(path, markerOpenFlags, 0) // #nosec G304 -- a PID file inside the Ghost data directory, not caller input
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	// One byte past the bound, so an over-cap file is told apart from one that
	// exactly fills it.
	data, err := io.ReadAll(io.LimitReader(f, maxPIDFileBytes+1))
	if err != nil || len(data) > maxPIDFileBytes {
		return nil, false
	}
	return data, true
}

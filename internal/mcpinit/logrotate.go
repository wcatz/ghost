package mcpinit

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

// logRotateCap is the size at which a data-dir log handed to openLogForAppend
// is rotated instead of merely appended to. It is a ceiling on a file nothing
// else bounds: the stop hook opens lifecycle.log after every turn, and a
// failing chain can put hundreds of identical lines a day into it (#542
// measured a 5 MB lifecycle.log on a laptop). Five megabytes is far past
// anything a human greps and far short of anything that matters as disk.
const logRotateCap = 5 << 20

// createLog opens a data-dir log for append; after a rotation it opens both
// candidates, the fresh file and the rotated copy. It is a package variable so
// a test can make an open fail — the real cause is ENOSPC or an inode
// exhaustion on the very filesystem the rotation exists to protect, which a
// test cannot stage.
var createLog = func(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// openLogForAppend opens path for append, creating it if it is missing, and
// hands an existing file that has reached logRotateCap to "<path>.1" —
// replacing the previous ".1", so the directory holds the current log and
// exactly one rotated copy rather than a chain of them.
//
// It is the one way Ghost opens the logs it appends to (lifecycle.log and
// obsidian-sync.log today, and any phase log a future build writes), so the cap
// applies wherever a log is written rather than to whichever call site
// remembered it.
//
// Rotation happens BEFORE the file is opened, never while a descriptor of this
// process's own is held across the rename: Windows refuses to move a file that
// is open without a delete-sharing grant, so the rename would fail there every
// time and the cap would be a Linux-only feature.
//
// Failures degrade to a plain open wherever they can. A rename that cannot
// happen (a directory at "<path>.1", a read-only mount, another process)
// leaves the log exactly where it was and the open proceeds as it did before
// rotation existed. The fallback to "<path>.1" is offered only when the name
// is GONE — Lstat finds nothing there, because the rename moved the content
// into the copy (or an earlier open already left the name missing). That is
// the condition that makes the copy worth appending to: with no file under
// the name it is either where the rename just put the recent lines or the
// only copy left, and opening an existing file for append allocates nothing,
// which is why it still opens on the disk that refused the fresh create.
// Because the name stays gone until the filesystem has room to create it,
// EVERY later open takes this route, not just the one that rotated. A name
// that exists but will not open — permissions, a directory wearing it, an
// inode shortage — reports itself instead: the recent lines are under that
// name, the ".1" beside it is an earlier generation, and splicing a new run
// into the earlier generation would join two runs with no boundary between
// them. The copy is never created here either; a fallback only ever uses one
// already there.
//
// The fallback is silent to the caller, so it is logged here: while it lasts
// "<path>" does not exist, so anything tailing the log by name sees nothing —
// the name returns as soon as the filesystem has room to create it again.
// One line per open, not per append, and only while the condition lasts. It
// also means the copy is what grows during the fallback and nothing rotates
// it again (rotation only looks at "<path>"), so the cap resumes with the
// name; refusing or truncating to hold it would cost a diagnostic log its
// lines, which is the wrong trade. Two failures come back as errors: no copy
// to fall back to (the name exists but would not open, or the copy is absent
// or not a regular file), which names the fresh path; and both opens failing,
// which names both paths and both causes.
//
// Rotation is decided by Lstat, so a symlink wearing the log's name is
// appended through and never renamed: renaming it would move the link out from
// under whoever arranged it, and untangling the target is theirs to do.
func openLogForAppend(path string) (*os.File, error) {
	rotatedPath := path + ".1"
	rotateLogIfOversized(path, rotatedPath, os.Lstat)

	// Try the log under its own name first — the open every caller did before
	// rotation existed.
	if f, err := createLog(path); err == nil {
		return f, nil
	} else if _, lerr := os.Lstat(path); lerr == nil {
		// The name is still there and would not open. The recent lines are
		// under it, and "<path>.1" beside it is an earlier generation, so
		// this is the one failure that is not a rotation fallback: report it.
		return nil, err
	} else if fi, rerr := os.Lstat(rotatedPath); rerr != nil || !fi.Mode().IsRegular() {
		// The name is gone and there is nothing rotated to fall back to:
		// report the open that failed.
		return nil, err
	} else if f2, err2 := createLog(rotatedPath); err2 == nil {
		slog.Warn("ghost: log could not be opened under its own name, appending through the rotated copy instead",
			"path", path, "rotated", rotatedPath, "error", err)
		return f2, nil
	} else {
		return nil, fmt.Errorf("open log %s after rotating it to %s: %w", path, rotatedPath, errors.Join(err, err2))
	}
}

// rotateLogIfOversized moves path to rotatedPath when path is a regular file
// that has reached logRotateCap, and reports whether it moved one.
//
// The size is read TWICE, and it is the second read that decides. Two Ghost
// processes can both see an oversized log — the stop hook runs after every
// turn, and two turns overlap — and one read lets the loser of that race
// destroy the winner's work: the winner renames the full log into ".1" and its
// own open creates a small fresh log at the name, and the loser's rename then
// replaces that ".1" with the small file. Five megabytes of diagnostics, gone,
// with nothing logged about it. Re-reading immediately before the rename is
// what catches the ordinary case, because by then the other process's rename
// has already landed and the name holds a file well under the cap.
//
// It does not make the rename atomic with the read: a process that interleaves
// inside that window still rotates something. Closing that needs a lock around
// the whole pass, which is a file in the data directory for the sake of a
// diagnostic log's tail — the loss it would prevent is bounded by one cap, and
// the cost is a lock every log write has to take.
//
// The probe is a parameter so a test can state the two reads a race produces
// rather than interleave with them, the same reason linkGooseConfigDirsWith
// takes its probes.
//
// The rename happens with nothing of this process's own held across it: Windows
// refuses to move a file that is open without a delete-sharing grant, so the
// open-then-rename order would make the cap a no-op there. A rename that fails
// leaves the log where it is, and the caller's open proceeds as it did before
// rotation existed.
func rotateLogIfOversized(path, rotatedPath string, probe func(string) (os.FileInfo, error)) bool {
	// Read once to decide whether the log is a rotation candidate at all.
	if !logAtOrOverCap(path, probe) {
		return false
	}
	// Read AGAIN, here, immediately before the rename — and this is the reading
	// that counts. Written as two separate calls rather than a repeated
	// expression so the intent is legible: a single read is the bug, and code
	// that looks like a typo is a poor place to be carrying the fix for one.
	if !logAtOrOverCap(path, probe) {
		return false
	}
	return os.Rename(path, rotatedPath) == nil
}

// logAtOrOverCap reports whether path is a regular file that has reached
// logRotateCap, which is the whole of the rotation test. It is called twice per
// rotation attempt; see rotateLogIfOversized for why the second call is the one
// that decides.
func logAtOrOverCap(path string, probe func(string) (os.FileInfo, error)) bool {
	info, err := probe(path)
	return err == nil && info.Mode().IsRegular() && info.Size() >= logRotateCap
}

// LogRotateCap is logRotateCap for a caller outside this package.
const LogRotateCap = logRotateCap

// OpenLogForAppend is openLogForAppend for a caller outside this package: the
// MCP server's GHOST_LOG_FILE redirect names an arbitrary path rather than a
// data-dir file, and takes the same cap, single ".1" copy and 0600 create.
func OpenLogForAppend(path string) (*os.File, error) {
	return openLogForAppend(path)
}

// RotatingLog is an append-only log writer that holds the cap while it is
// open. openLogForAppend checks the size once, at open, which is enough for a
// log opened per run but not for the MCP server's GHOST_LOG_FILE redirect: the
// server keeps one descriptor for its whole life, so under GHOST_DEBUG a
// session that stays open for days would grow the file without bound.
//
// Before a write, a log that has reached logRotateCap is closed, handed to
// openLogForAppend again — which renames it to "<path>.1" by the same rule and
// recreates it 0600 — and the write goes to the fresh file. The file is
// closed BEFORE the rename for the Windows reason given on
// rotateLogIfOversized. The live file therefore ends at most one write past
// the cap. Writes are serialised by a mutex, because slog handlers share one
// writer across goroutines.
//
// Write never returns an error: a log write that fails must not fail the code
// that logged. If the reopen after a rotation fails, the writer has no file
// and drops lines (the same degradation as the server falling back to stderr
// when the open at start fails) and tries to open again on the next write.
// A rotation whose rename fails leaves the file where it was, so the writer
// keeps appending to it and tries again after another megabyte rather than
// closing and reopening on every line.
type RotatingLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	// limit is the size at which the next write rotates first.
	limit int64
	// closed is set by Close; a write after it reports os.ErrClosed, as the
	// bare file the redirect used to be did.
	closed bool
}

// retryAfterFailedRotation is how much a log may grow past a rotation that
// could not rename it before the writer tries again.
const retryAfterFailedRotation = 1 << 20

// OpenRotatingLog opens path through openLogForAppend and returns a writer
// that keeps it within the cap for as long as it stays open.
func OpenRotatingLog(path string) (*RotatingLog, error) {
	l := &RotatingLog{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

// open opens the log and reads its size. The caller holds mu, or owns l.
func (l *RotatingLog) open() error {
	f, err := openLogForAppend(l.path)
	if err != nil {
		return err
	}
	var size int64
	if fi, serr := f.Stat(); serr == nil {
		size = fi.Size()
	}
	l.f, l.size, l.limit = f, size, logRotateCap
	if size >= logRotateCap {
		// The rename did not happen; do not retry on every line.
		l.limit = size + retryAfterFailedRotation
	}
	return nil
}

// Write appends p, rotating first when the log has reached its cap. Apart from
// a write after Close, it always reports len(p) and a nil error; see the type
// comment.
func (l *RotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, os.ErrClosed
	}
	if l.f != nil && l.size >= l.limit {
		_ = l.f.Close()
		l.f = nil
	}
	if l.f == nil {
		if err := l.open(); err != nil {
			return len(p), nil
		}
	}
	n, _ := l.f.Write(p)
	l.size += int64(n)
	return len(p), nil
}

// Close closes the current file and ends the writer.
func (l *RotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

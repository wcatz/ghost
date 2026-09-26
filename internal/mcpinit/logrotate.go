package mcpinit

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Size() >= logRotateCap {
		// Rename first, with nothing of this process's own held across it:
		// Windows refuses to move a file that is open without a delete-sharing
		// grant, so the open-then-rename order would make the cap a no-op
		// there. A rename that fails leaves the log where it is, and the open
		// below proceeds as it did before rotation existed.
		_ = os.Rename(path, rotatedPath)
	}

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

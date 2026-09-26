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
// rotation existed. When the name cannot be opened at all — the rename landed
// and the fresh file will not create (a disk that just filled up, the case
// rotation exists for), or the name was never openable — the rotated copy is
// used if it is already there: opening an existing file for append allocates
// nothing, which is why it still works on the disk that refused the fresh
// create, and it is where the recent lines are. That copy is never created
// here: a fallback only ever uses one already sitting beside the log, so a
// path with nothing to fall back to — a directory wearing the name — reports
// the original error rather than conjuring a stray file.
//
// The fallback is silent to the caller, so it is logged here: while it lasts
// "<path>" does not exist, so anything tailing the log by name sees nothing —
// the name returns as soon as the filesystem has room to create it again.
// One line per open, not per append, and only while the condition lasts. The
// one case that still returns an error is both files failing to open; that
// error names both paths and both causes.
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
	} else if fi, lerr := os.Lstat(rotatedPath); lerr != nil || !fi.Mode().IsRegular() {
		// Nothing rotated to fall back to: report the open that failed.
		return nil, err
	} else if f2, err2 := createLog(rotatedPath); err2 == nil {
		slog.Warn("ghost: log could not be opened under its own name, appending through the rotated copy instead",
			"path", path, "rotated", rotatedPath, "error", err)
		return f2, nil
	} else {
		return nil, fmt.Errorf("open log %s after rotating it to %s: %w", path, rotatedPath, errors.Join(err, err2))
	}
}

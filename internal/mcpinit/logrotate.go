package mcpinit

import "os"

// logRotateCap is the size at which a data-dir log handed to openLogForAppend
// is rotated instead of merely appended to. It is a ceiling on a file nothing
// else bounds: the stop hook opens lifecycle.log after every turn, and a
// failing chain can put hundreds of identical lines a day into it (#542
// measured a 5 MB lifecycle.log on a laptop). Five megabytes is far past
// anything a human greps and far short of anything that matters as disk.
const logRotateCap = 5 << 20

// createLog opens a data-dir log for append. It is a package variable so a test
// can make the post-rotation create fail — the real cause is ENOSPC or an inode
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
// Every failure degrades to a plain open rather than to an error. A rename that
// cannot happen (a directory at "<path>.1", a read-only mount, another
// process) leaves the log exactly where it was and the open proceeds as it did
// before rotation existed. A rename that lands but leaves the fresh file
// uncreatable — a disk that just filled up, which is the case rotation exists
// for — falls back to appending through "<path>.1", the file that now holds the
// content, so the caller's line still lands instead of being reported as a
// broken log and dropped: both spawn sites read an open error as "no log",
// and one of them then declines to spawn at all.
//
// Rotation is decided by Lstat, so a symlink wearing the log's name is
// appended through and never renamed: renaming it would move the link out from
// under whoever arranged it, and untangling the target is theirs to do.
func openLogForAppend(path string) (*os.File, error) {
	rotated := false
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Size() >= logRotateCap {
		if err := os.Rename(path, path+".1"); err == nil {
			rotated = true
		}
	}
	if rotated {
		// Prefer the fresh file; if it cannot be created, append through the
		// rotated copy, which is where the content now lives.
		if f, err := createLog(path); err == nil {
			return f, nil
		} else if f2, err2 := createLog(path + ".1"); err2 == nil {
			return f2, nil
		} else {
			return nil, err
		}
	}
	return createLog(path)
}

package mcpinit

import "os"

// logRotateCap is the size at which a data-dir log handed to openLogForAppend
// is rotated instead of merely appended to. It is a ceiling on a file nothing
// else bounds: the stop hook opens lifecycle.log after every turn, and a
// failing chain can put hundreds of identical lines a day into it (#542
// measured a 5 MB lifecycle.log on a laptop). Five megabytes is far past
// anything a human greps and far short of anything that matters as disk.
const logRotateCap = 5 << 20

// createRotatedLog opens the fresh file a rotation leaves behind. It is a
// package variable only so a test can make this one open fail: the real cause
// is ENOSPC or an inode exhaustion on the very filesystem the rotation exists
// to protect, which a test cannot stage. Failing here must stay non-fatal —
// openLogForAppend still hands back a descriptor that writes.
var createRotatedLog = func(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// openLogForAppend opens path for append, creating it if it is missing, and
// hands an existing file that has reached logRotateCap to "<path>.1" —
// replacing the previous ".1", so the directory holds the current log and
// exactly one rotated copy rather than a chain of them.
//
// It is the one way Ghost opens the logs it appends to (lifecycle.log and
// obsidian-sync.log today, and any phase log a future build writes), so the
// cap applies wherever a log is written rather than to whichever call site
// remembered it.
//
// Order matters: the file is opened first, exactly as it was before rotation
// existed, and only then rotated, while that descriptor is still in hand.
// Rotation therefore cannot make an open fail. The descriptor handed to the
// caller keeps pointing at the same inode after the rename, so every step
// that can go wrong — the rename itself, or creating the fresh file afterwards
// on a filesystem that just filled up — degrades to appending through the
// rotated copy, which is where the caller's line belongs anyway, rather than
// to an error the spawn sites would read as "the log is broken" and drop the
// spawned process's stdout for.
//
// Rotation is decided by Lstat, so a symlink wearing the log's name is
// appended through and never renamed: renaming it would move the link out from
// under whoever arranged it, and untangling the target is theirs to do.
func openLogForAppend(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	fi, lerr := os.Lstat(path)
	if lerr != nil || !fi.Mode().IsRegular() || fi.Size() < logRotateCap {
		// Missing (just created), under the cap, gone again, or a symlink:
		// nothing to rotate.
		return f, nil
	}
	if err := os.Rename(path, path+".1"); err != nil {
		// A directory at "<path>.1", a read-only mount, another process.
		// Keep appending where the log already is; the open still succeeded.
		return f, nil
	}
	nf, err := createRotatedLog(path)
	if err != nil {
		// The rename landed but the fresh file could not be created. f still
		// names the rotated copy, so the caller writes there instead of
		// losing its line — and the next open will try the rotation again.
		return f, nil
	}
	_ = f.Close()
	return nf, nil
}

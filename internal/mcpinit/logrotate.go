package mcpinit

import "os"

// logRotateCap is the size at which a data-dir log handed to openLogForAppend
// is rotated instead of appended to. It is a ceiling on a file nothing else
// bounds: the stop hook opens lifecycle.log after every turn and a failing
// chain can put hundreds of identical lines a day into it (#542 measured a
// 5 MB lifecycle.log on a laptop). Five megabytes is far past anything a human
// greps and far short of anything that matters as disk.
const logRotateCap = 5 << 20

// openLogForAppend opens path for append, creating it if it is missing, and
// renames an existing file that has reached logRotateCap to "<path>.1" first —
// replacing the previous ".1", so the directory holds the current log and
// exactly one rotated copy rather than a chain of them.
//
// It is the one way Ghost opens the logs it appends to (lifecycle.log,
// obsidian-sync.log, and the phase logs when a caller names them), so the cap
// applies wherever a log is written rather than to whichever call site
// remembered it. The append is append-only for the same reason the file exists
// at all: two processes opening it concurrently must both end up with their
// lines, and a truncate would drop one of them.
//
// Rotation is best effort. It is decided by Lstat, so a symlink wearing the
// log's name is appended through and never renamed (renaming it would move the
// link out from under whoever arranged it), and a rename that fails — a
// directory sitting at "<path>.1", a read-only mount — leaves the log growing
// where it is. Never failing the caller is the point: every caller treats this
// file as diagnostic, and the spawn sites lose their child's stdout entirely
// if the open does not happen.
func openLogForAppend(path string) (*os.File, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Size() >= logRotateCap {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

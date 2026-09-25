package obsidian

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const markerName = ".ghost-vault"

// readDirFn is swappable so tests can force ensureVault's ReadDir call to
// fail deterministically — os.Chmod-based fault injection is unreliable
// across CI privilege levels (e.g. a process with CAP_DAC_READ_SEARCH can
// bypass a directory's permission bits).
var readDirFn atomic.Value // func(string) ([]os.DirEntry, error)

func readDir(dir string) ([]os.DirEntry, error) {
	fn, _ := readDirFn.Load().(func(string) ([]os.DirEntry, error))
	return fn(dir)
}

// removeDirFn is swappable so tests can force pruneOrphanFolder's directory
// cleanup to fail deterministically — the same rationale as readDirFn.
var removeDirFn atomic.Value // func(string) error

// pruneBeforeRemoveFn is a test seam between Ghost-note classification and
// removal. Production leaves it as a no-op; tests use it to model a concurrent
// replacement without relying on scheduler timing.
var pruneBeforeRemoveFn atomic.Value // func(string)

// renameFn is swappable so tests can force the publish step to fail with a
// platform-specific transient error (see renameWithRetry).
var renameFn atomic.Value // func(string, string) error

// removeFileFn is swappable so tests can force the final deletion of a
// quarantined note to fail — the step whose failure has to roll back, and the
// one os.Chmod-based injection cannot reach reliably across CI privilege
// levels.
var removeFileFn atomic.Value // func(string) error

func removeFile(path string) error {
	fn, _ := removeFileFn.Load().(func(string) error)
	return fn(path)
}

// ErrNotRegularFile reports a note path that exists but is not a regular file.
// It is a distinct error so a caller can skip that note and keep going, rather
// than treating it as a write failure and abandoning the whole export.
var ErrNotRegularFile = errors.New("destination is not a regular file")

// crashTempMarker is the infix writeIfChanged gives os.CreateTemp, so a
// crashed write leaves a name like "My Note.md.ghost-tmp-4821".
const crashTempMarker = ".ghost-tmp-"

// pruneQuarantineMarker is the prefix removeGhostFile uses to move a note aside
// before deleting it. A crash between the rename and the delete leaves the note
// under that name, and nothing else ever looks for it again — the mirror is
// silently missing a note it still owns. The reclaim sweep therefore covers it
// on the same terms as a crashed publish: same grace period, so a note that is
// mid-delete right now is never taken out from under itself.
const pruneQuarantineMarker = ".ghost-prune-"

// crashTempGrace is how long a temp file must have existed before the reclaim
// sweep will remove it.
//
// writeIfChanged publishes through a temp file in the same directory and
// renames it into place. Between CreateTemp and that rename the file is a LIVE
// file: a concurrent sync that is mid-write owns it, and deleting it would
// destroy a note the other writer is about to publish. A crash artifact is
// indistinguishable from that window by name alone, so age is the discriminator —
// an hour is far longer than any publish takes, and a leftover is only a
// leftover for as long as it is not being written to.
const crashTempGrace = time.Hour

// isCrashTemp reports whether path is a leftover from an interrupted publish,
// and not a temp file a concurrent writer is still using.
//
// The pattern has to match what CreateTemp is actually given, and the trailing
// "-*" matters as much as the infix: an earlier test on the bare ".ghost-tmp"
// suffix matched no real artifact at all, while a test on the infix alone would
// delete a user's own file called "notes.ghost-tmp". Requiring both the infix
// and a non-empty random suffix keeps the sweep pointed at Ghost's files.
//
// The age check is what makes it safe to remove one at all. mtime is the only
// signal available, and a stat failure means "cannot prove it is old", which is
// the answer that must not delete.
func isCrashTemp(path string, now time.Time) bool {
	name := filepath.Base(path)
	idx := strings.LastIndex(name, crashTempMarker)
	if idx < 0 {
		// A note stranded under the prune quarantine for the same reason: the
		// process died between moving it aside and deleting it.
		if q := strings.LastIndex(name, pruneQuarantineMarker); q >= 0 && len(name) > q+len(pruneQuarantineMarker) {
			return olderThanGrace(path, now)
		}
		return false
	}
	if len(name) <= idx+len(crashTempMarker) {
		return false
	}
	return olderThanGrace(path, now)
}

// olderThanGrace reports whether path has not been touched for longer than the
// reclaim grace period. A stat failure is false: "cannot prove it is
// abandoned" is the answer that must not delete.
func olderThanGrace(path string, now time.Time) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return now.Sub(info.ModTime()) > crashTempGrace
}

func init() {
	readDirFn.Store(os.ReadDir)
	removeDirFn.Store(os.Remove)
	pruneBeforeRemoveFn.Store(func(string) {})
	renameFn.Store(os.Rename)
	removeFileFn.Store(os.Remove)
}

func removeDir(path string) error {
	fn, _ := removeDirFn.Load().(func(string) error)
	return fn(path)
}

func beforePruneRemove(path string) {
	fn, _ := pruneBeforeRemoveFn.Load().(func(string))
	fn(path)
}

// restoreQuarantined moves a quarantined object back to the path it came from,
// but only onto a free path.
//
// The no-replace rename is the whole point. A check-then-rename pair is a race:
// on POSIX the rename replaces whatever now occupies the destination, so a file
// created in that window is unlinked and its contents destroyed — silently, by
// the code written to make sure Ghost never deletes a file it did not
// inspect. Failing leaves the quarantined object where it is, which a later
// pass can find and this one can report.
//
// A source that no longer exists is not an error: a concurrent prune took it,
// and there is nothing to restore and nothing to report.
func restoreQuarantined(quarantinePath, destPath string) error {
	if err := renameNoReplace(quarantinePath, destPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		if os.IsExist(err) {
			return fmt.Errorf("cannot restore replaced prune candidate %s: %w", destPath, err)
		}
		return err
	}
	return nil
}

// removeGhostFile atomically moves path aside, verifies the moved object is
// still a Ghost note, and only then removes it. A concurrent replacement is
// restored instead of deleted.
func removeGhostFile(path string) (bool, error) {
	beforePruneRemove(path)

	temp, err := os.CreateTemp(filepath.Dir(path), ".ghost-prune-*")
	if err != nil {
		return false, err
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return false, err
	}
	if err := os.Remove(tempPath); err != nil {
		return false, err
	}
	if err := os.Rename(path, tempPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	// Put the quarantined note back only if nothing now occupies the original
	// path, and do it with a no-replace rename. An existence check followed by
	// os.Rename is a race, not a guard: on POSIX the rename replaces whatever
	// created the file in between, so the concurrent writer's data is lost
	// silently. renameNoReplace fails instead, which leaves the quarantined
	// object in place for the next pass to find rather than destroying
	// something Ghost never inspected.
	restore := func() error {
		return restoreQuarantined(tempPath, path)
	}
	info, err := os.Lstat(tempPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		// Same strand as the failed deletion below: the note has already left
		// its original name, so returning here leaves it under the
		// .ghost-prune-* prefix that no later pass searches. An unreadable
		// quarantine is exactly when restoring matters most, since the object
		// is the only copy.
		if restoreErr := restore(); restoreErr != nil {
			return false, fmt.Errorf("inspect quarantined note: %w (and it could not be restored: %v)", err, restoreErr)
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, restore()
	}
	if _, ok := hasGhostID(tempPath); !ok {
		return false, restore()
	}
	if err := removeFile(tempPath); err != nil && !os.IsNotExist(err) {
		// The note has already left its original name, so a bare return here
		// strands it under the .ghost-prune-* name — a name every later pass
		// ignores, which means the mirror silently loses a note it still owns
		// and nothing reports it. Put it back where it came from. If that also
		// fails, both errors are reported: the caller can no longer assume the
		// note is where it left it, and a single error would hide which of the
		// two problems actually needs attention.
		if restoreErr := restore(); restoreErr != nil {
			return false, fmt.Errorf("remove quarantined note: %w (and it could not be restored: %v)", err, restoreErr)
		}
		return false, err
	}
	return true, nil
}

// ensureVault prepares dir as a Ghost-managed mirror target. Fresh or empty
// dirs are initialized with the marker; a non-empty dir without the marker is
// refused — Ghost never adopts a folder it didn't create. An existing vault
// also gets one tighten pass: writeIfChanged skips unchanged content and
// MkdirAll never retightens a directory, so a vault created under the old
// 0755/0644 defaults would stay world-readable forever otherwise.
func ensureVault(dir string) error {
	entries, err := readDir(dir)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create vault dir: %w", err)
		}
		entries = nil
	} else if err != nil {
		return fmt.Errorf("read vault dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, markerName)); err != nil {
		if len(entries) > 0 {
			return fmt.Errorf("%s exists, is not empty, and has no %s marker — refusing to manage it (use a fresh directory)", dir, markerName)
		}
		if err := os.WriteFile(filepath.Join(dir, markerName), []byte(`{"schema_version":1}`+"\n"), 0o600); err != nil {
			return err
		}
	}
	// No retroactive permission pass. An earlier revision walked the whole
	// vault on every ensureVault and chmod'd every directory it found to 0700 —
	// including the user's own folders and the .obsidian/ directory Ghost never
	// created — and on Windows it rewrote a protected DACL on each of them, on
	// every pass. That is outside what a mirror is for: it changes permissions
	// on files Ghost does not own, for a vault it merely reads. Permissions are
	// now set only where Ghost writes, in writeIfChanged, via protectFile.
	return nil
}

// writeIfChanged writes content atomically (temp+rename), skipping the write
// when the file already has identical content — no mtime churn.
// writeIfChangedSkipSpecial is writeIfChanged for the export loop: a note path
// that is not a regular file is skipped with a warning rather than ending the
// export. A symlink or a named pipe at a note path belongs to the user or to
// another tool, and Ghost will not replace it — but refusing every note because
// one path is occupied is a worse failure than syncing the rest.
func writeIfChangedSkipSpecial(path, content string) (bool, error) {
	written, err := writeIfChanged(path, content)
	if errors.Is(err, ErrNotRegularFile) {
		fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", path, err)
		return false, nil
	}
	return written, err
}

func writeIfChanged(path, content string) (bool, error) {
	// Decide the destination's type before reading it, not after. os.ReadFile
	// on a FIFO blocks until a writer appears, so a FIFO sitting at a
	// canonical note path would hang the export indefinitely; and a symlink
	// would be read through, so a link pointing outside the vault has its
	// target compared against Ghost's content while the publish step below
	// replaces the link entry — the same special-file hazard the delete path
	// guards, on the write path.
	//
	// A destination that is not a regular file is refused rather than replaced.
	// Replacing it is what the special file actually is not: a named pipe or a
	// symlink is something a user or another tool put there, and silently
	// unlinking it is the data loss this package exists to avoid.
	switch info, err := os.Lstat(path); {
	case err != nil && !os.IsNotExist(err):
		return false, err
	case err == nil && !info.Mode().IsRegular():
		// Skip the note, do not fail the export. A named pipe or a symlink at a
		// note path is something a user or another tool put there, and Ghost
		// will not replace it — but one such path is not a reason to abandon
		// every other note in the export. The caller decides whether a skipped
		// note is a problem; the default is that a mirror that syncs 400 notes
		// and refuses to sync any of them is worse than one that syncs 399.
		return false, ErrNotRegularFile
	case err == nil:
		// Regular file: the unchanged-content check still applies, so an
		// already-correct note costs no write and no mtime churn.
		// A read failure here is not a write failure. On Windows a file another
		// process holds open cannot be read at all, and the original code
		// treated that as "contents unknown, go ahead and write" — returning
		// the error instead failed the whole export over a file Ghost was
		// about to replace anyway.
		if existing, readErr := os.ReadFile(path); readErr == nil && string(existing) == content {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	// A fixed temp name collides when two syncs write the same note at once
	// (a manual run plus the auto-sync worker), so the rename could publish a
	// partially interleaved file. Use a unique temp file in the same
	// directory — rename stays atomic and last-writer-wins is well defined.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".ghost-tmp-*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer os.Remove(tmp) //nolint:errcheck // no-op once the rename succeeds
	if err := protectFile(f); err != nil {
		f.Close() //nolint:errcheck
		return false, err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close() //nolint:errcheck
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// maxFrontmatterLine bounds a single frontmatter line for hasGhostID's
// scanner. Ghost's renderer puts every value on one short line, so this sits
// orders of magnitude above anything a real note produces — but Scanner's
// default 64 KiB cap could be reached by a hand-edited note with one long
// value, which would silently misclassify a Ghost note as the user's file.
const maxFrontmatterLine = 1 << 20

// hasGhostID reports whether a file's frontmatter carries a ghost_id key —
// the only files prune may touch. Only the frontmatter block (between the
// opening and closing --- lines) is scanned, never the note body, so the
// stream stops at the closing --- instead of reading the whole file: prune
// and the permission tighten both ask this of every .md in the vault, and
// whole-file reads made the orphan sweep read each note twice.
func hasGhostID(path string) (string, bool) {
	// openRegular, not os.Open: the caller's regular-file check came from a
	// directory entry stat'ed before this point, and re-opening the name leaves
	// a window in which the object behind it is no longer that entry. Deciding
	// the type on the handle that is read closes it — a symlink is refused, a
	// FIFO cannot block the open, and a directory is rejected before a read is
	// attempted.
	f, err := openRegular(path)
	if err != nil {
		return "", false
	}
	defer f.Close() //nolint:errcheck // read-only: close errors are meaningless here
	id, ok := frontmatterHasGhostID(f)
	return id, ok
}

// frontmatterHasGhostID scans an open regular file's frontmatter for a
// ghost_id key. Split from hasGhostID so a caller that must distinguish "not
// Ghost's" from "could not be read" can open the file itself and get the
// failure reason rather than a flattened false.
func frontmatterHasGhostID(f *os.File) (string, bool) {
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 4096), maxFrontmatterLine)
	if !s.Scan() || s.Text() != "---" {
		return "", false
	}
	// A candidate id counts only once the closing --- is seen. EOF or a scan
	// error before that delimiter means this was never frontmatter: a user
	// note that opens with a horizontal rule and mentions ghost_id further
	// down (a template, a pasted example) would otherwise be classified as
	// Ghost's, and prune deletes what it classifies.
	var id string
	closed := false
	for s.Scan() {
		line := s.Text()
		if line == "---" { // end of frontmatter — stop before the body
			closed = true
			break
		}
		if id == "" {
			if candidate, ok := strings.CutPrefix(line, "ghost_id: "); ok {
				id = strings.TrimSpace(candidate)
			}
		}
	}
	if !closed {
		// The loop also exits with closed=false on a scan error — a line past
		// maxFrontmatterLine or a read failure — so an unread frontmatter is
		// "not Ghost's file" rather than a silent miss.
		return "", false
	}
	return id, id != ""
}

// pruneOrphanFolder removes Ghost's own notes from a folder the current
// project set no longer claims, then removes directories left empty by
// those deletions.
//
// It never removes a file that is not Ghost's. The sweep used os.RemoveAll on
// the whole folder whenever it held at least one ghost_id note, so a user's
// note or attachment sitting beside that file went with it — with no undo, and
// against a design spec that states user-created files are never touched
// (2026-07-10 obsidian-vault-mirror-design.md). Triggers are ordinary: a
// project rename or merge changes the folder set, and any user folder holding
// a copied Ghost note looked exactly like an orphaned project.
//
// Deleting Ghost-managed files one at a time and offering directories for
// removal only when they held a deleted Ghost file leaves user content
// exactly where it was — including an empty folder the user created, which
// a sweep keyed on emptiness alone would vacuum up. os.Remove answering
// ENOTEMPTY is not an error here but the proof the guard worked: a
// directory that will not close is a directory that still holds something
// Ghost does not own.
//
// One walk does all three jobs — find Ghost notes, delete them, remember
// where — instead of the earlier probe walk, delete walk, and directory
// walk, each re-reading every note. A folder with no Ghost notes deletes
// nothing and therefore offers nothing upward, which is the same guarantee
// the old hasGhostContent probe gave, for free.
func pruneOrphanFolder(dir string) error {
	var deletedDirs []string
	// Identity of every directory the walk descended through, so the cleanup
	// climb can prove it is still removing the directory it emptied rather than
	// one that replaced it. See the SameFile check below.
	walkedDirs := make(map[string]os.FileInfo)
	found := false
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: leave it and everything under it alone
		}
		if d.IsDir() {
			// Remember what this directory is, by identity rather than by name.
			// The cleanup climb runs after the walk, and a directory that was
			// replaced in between is a different object that happens to have the
			// same path — os.Remove would delete the replacement, which is the
			// one thing this whole function is written to never do. lstat, so a
			// symlinked directory records the link and not its target.
			if info, infoErr := d.Info(); infoErr == nil {
				walkedDirs[path] = info
			}
			return nil
		}
		// Regular files only. A symlink is the user's own indirection: a
		// "shortcut.md" pointing at a Ghost note must not be classified
		// through the link (and unlinked), and opening a FIFO blocks
		// forever — either would hang or damage an export. The permission
		// pass already holds a symlink to the same rule.
		if !d.Type().IsRegular() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil // an attachment — not Ghost's to delete
		}
		if _, ok := hasGhostID(path); !ok {
			return nil // a note the user wrote — never touched
		}
		deleted, err := removeGhostFile(path)
		if err != nil {
			return err
		}
		if !deleted {
			return nil
		}
		found = true
		deletedDirs = append(deletedDirs, filepath.Dir(path))
		return nil
	}); err != nil {
		return fmt.Errorf("remove ghost-managed files: %w", err)
	}
	if !found {
		return nil // no Ghost content here: the folder is the user's, untouched
	}

	// Climb from each deletion site toward the orphan root, offering a
	// directory only while it is empty. Every ancestor of a deleted Ghost
	// note gets its chance (a later climb cleans up what an earlier one
	// found still populated), while a user's empty folder sits under no
	// deletion site and is never offered at all. The climb stops at dir —
	// the orphan folder itself may close, its parent may not.
	//
	// Only "still holds something" and "already gone" pass silently; any
	// other failure (permissions, a Windows sharing violation, I/O) is
	// returned, because swallowing it made prune report success while an
	// empty stale directory sat there for every later export to retry.
	for _, site := range deletedDirs {
	climb:
		for sub := site; ; sub = filepath.Dir(sub) {
			// Only remove the directory the walk emptied. A directory replaced
			// after the walk — by a sync, a sync client, or the user — shares
			// its path but is a different object, and os.Remove would take it,
			// along with anything inside it, on the strength of a name the walk
			// never saw. Leaving an ambiguous directory behind costs an empty
			// folder; removing the wrong one costs a user's files. A directory
			// with no recorded identity (created during the walk, so never
			// descended into) is treated the same way.
			if walked, ok := walkedDirs[sub]; ok {
				now, lstatErr := os.Lstat(sub)
				if lstatErr != nil || !os.SameFile(walked, now) {
					if lstatErr != nil && !os.IsNotExist(lstatErr) {
						return fmt.Errorf("verify orphan directory %s before removal: %w", sub, lstatErr)
					}
					break // gone, or replaced: nothing here is ours to remove
				}
			} else {
				break
			}
			err := removeDir(sub)
			if err != nil && !os.IsNotExist(err) {
				if !isDirNotEmpty(err) {
					return fmt.Errorf("remove orphan directory %s: %w", sub, err)
				}
				break // still holds something Ghost doesn't own: the guard
			}
			if sub == dir {
				// The orphan root itself may close; never offer its parent,
				// even when a previous climb already removed it and this
				// one only sees ENOENT.
				break climb
			}
		}
	}
	return nil
}

// prune deletes Ghost-managed .md files under the given vault subtrees whose
// ghost_id is not in keep, or whose basename is not the canonical one for
// that ghost_id (a content edit renamed the slug — the old-slug file is
// stale even though its ID is still live). All three guards from the spec
// are enforced. Orphaned *.ghost-tmp files (left by a crashed writeIfChanged)
// are also reclaimed — but only inside the managed subtrees, behind the
// marker guard.
func prune(root string, subtrees []string, keep map[string]string, knownFolders []string) error {
	if _, err := os.Stat(filepath.Join(root, markerName)); err != nil {
		return fmt.Errorf("refusing to prune: %s marker not found in %s", markerName, root)
	}
	for _, sub := range subtrees {
		if !filepath.IsLocal(sub) {
			return fmt.Errorf("refusing to prune: subtree %q escapes vault root", sub)
		}
		base := filepath.Join(root, sub)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil // missing subtree is fine
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			// Regular files only — same reasoning as the orphan walk: never
			// open a symlink or a FIFO on the delete path.
			if !d.Type().IsRegular() {
				return nil
			}
			if isCrashTemp(path, time.Now()) {
				// Orphan from a crashed writeIfChanged. The name must match
				// what writeIfChanged actually produces — CreateTemp's
				// "<base>.ghost-tmp-<random>" — because a bare ".ghost-tmp"
				// suffix test never matched a real leftover, so the sweep both
				// missed every crash artifact and deleted any user file that
				// happened to end in those five characters. The trailing
				// wildcard is what makes it a claim on Ghost's own files
				// rather than on the user's.
				//
				// The name is a claim, not proof. A user who copied a note
				// beside the vault can end up with a file whose name ends
				// exactly this way, and age is not ownership. The ghost_id
				// check is: a crashed publish leaves a half-written RENDERED
				// NOTE, which carries frontmatter, and a file that does not
				// is not Ghost's whatever it is called. A prune-quarantine
				// file is exempt, because that object is moved aside
				// precisely so it can be examined this way before the
				// delete.
				if !strings.HasPrefix(filepath.Base(path), pruneQuarantineMarker) {
					if _, ok := hasGhostID(path); !ok {
						return nil
					}
				}
				return removeFile(path)
			}
			if !strings.HasSuffix(path, ".md") {
				return nil
			}
			if id, ok := hasGhostID(path); ok {
				if canonical, kept := keep[id]; !kept || canonical != filepath.Base(path) {
					_, err := removeGhostFile(path)
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	// Orphan cleanup: remove vault top-level directories that are not in the
	// current project set but contain Ghost-managed content. This handles
	// projects deleted from the DB since the last export. Runs whenever
	// knownFolders is non-nil — including when it is empty, which is the
	// complete set after the last project was deleted and every folder is
	// an orphan. Only nil (filtered export — we can't know what's orphaned)
	// skips it.
	if knownFolders != nil {
		known := make(map[string]bool, len(knownFolders))
		for _, f := range knownFolders {
			known[f] = true
		}
		entries, err := readDir(root)
		if err != nil {
			return fmt.Errorf("read vault root for orphan scan: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name() == markerName {
				continue
			}
			if known[e.Name()] {
				continue
			}
			if !filepath.IsLocal(e.Name()) {
				return fmt.Errorf("refusing to prune: orphan dir %q escapes vault root", e.Name())
			}
			dir := filepath.Join(root, e.Name())
			// probe-as-it-goes: with nothing Ghost-owned to delete,
			// pruneOrphanFolder returns without touching a file, a
			// directory, or the folder itself.
			if err := pruneOrphanFolder(dir); err != nil {
				return fmt.Errorf("prune orphan folder %s: %w", e.Name(), err)
			}
		}
	}
	return nil
}

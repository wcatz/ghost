package obsidian

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

const markerName = ".ghost-vault"

// readDirFn is swappable so tests can force ensureVault's ReadDir call to
// fail deterministically — os.Chmod-based fault injection is unreliable
// across CI privilege levels (e.g. a process with CAP_DAC_READ_SEARCH can
// bypass a directory's permission bits).
var readDirFn atomic.Value // func(string) ([]os.DirEntry, error)

func init() { readDirFn.Store(os.ReadDir) }

func readDir(dir string) ([]os.DirEntry, error) {
	fn, _ := readDirFn.Load().(func(string) ([]os.DirEntry, error))
	return fn(dir)
}

// ensureVault prepares dir as a Ghost-managed mirror target. Fresh or empty
// dirs are initialized with the marker; a non-empty dir without the marker is
// refused — Ghost never adopts a folder it didn't create.
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
	if _, err := os.Stat(filepath.Join(dir, markerName)); err == nil {
		return nil
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s exists, is not empty, and has no %s marker — refusing to manage it (use a fresh directory)", dir, markerName)
	}
	return os.WriteFile(filepath.Join(dir, markerName), []byte(`{"schema_version":1}`+"\n"), 0o600)
}

// writeIfChanged writes content atomically (temp+rename), skipping the write
// when the file already has identical content — no mtime churn.
func writeIfChanged(path, content string) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return false, nil
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
	if err := f.Chmod(0o600); err != nil {
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

// hasGhostID reports whether a file's frontmatter carries a ghost_id key —
// the only files prune may touch. Only a closed frontmatter block (between
// the opening and closing --- lines) counts: a note that merely starts with a
// --- horizontal rule and mentions ghost_id later in its body is not Ghost's.
//
// The value it returns is the id AS THE NOTE RECORDS IT, which is not always the
// id in the store: yamlScalar flattens a tab, a newline and a carriage return to
// a space so that every key occupies one line, and that flattening is lossy — an
// id of "tab<TAB>id", one of "line<NL>id" and one of "tab id" all read back as
// the same bytes. Quoting is undone here for the same reason it had to be: a
// quoted value is not the value, and a reader that is not the inverse of the
// writer returns something no keep-set is keyed by. Nothing may DEPEND on
// recovering the stored id from a note, which is why prune keys on the canonical
// filename instead; this function answers "is this Ghost's note, and which id does
// it say it is", and only the first is a question with a reliable answer.
func hasGhostID(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return "", false
	}
	id, found := "", false
	for _, line := range lines[1:] {
		if line == "---" { // end of frontmatter — stop before the body
			return id, found
		}
		if v, ok := strings.CutPrefix(line, "ghost_id: "); ok && !found {
			id, found = unquoteYAMLScalar(strings.TrimSpace(v)), true
		}
	}
	return "", false // frontmatter never closed
}

// unquoteYAMLScalar reverses the quoting yamlScalar applies, so a value written
// through it reads back as the value that went in. It undoes exactly the two
// escapes yamlScalar emits and nothing else — a hand-written note is never
// reinterpreted through escapes this function does not recognise, which matters
// because it decides which files prune may DELETE. Only the double-quoted form
// is recognised, because that is the only one yamlScalar emits; a single-quoted
// value keeps reading as the text it says and therefore never matches a keep-set
// key. A quoted value that does not decode is returned unchanged, which leaves
// it unmatched and therefore untouchable.
func unquoteYAMLScalar(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	inner := s[1 : len(s)-1]
	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		if inner[i] != '\\' || i+1 >= len(inner) {
			b.WriteByte(inner[i])
			continue
		}
		switch inner[i+1] {
		case '\\', '"':
			b.WriteByte(inner[i+1])
			i++
		default:
			// Not an escape yamlScalar emits: keep both bytes so the value is
			// returned as it was written rather than as something else.
			b.WriteByte(inner[i])
		}
	}
	return b.String()
}

// hasGhostContent reports whether dir contains any .md file with a ghost_id
// in its frontmatter — the signature of a Ghost-managed note.
func hasGhostContent(dir string) bool {
	var found bool
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		if _, ok := hasGhostID(path); ok {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return false // can't scan — preserve the folder
	}
	return found
}

// keepSet is the set of note PATHS this pass wrote — the files that must
// survive, one per live memory, task and decision. A path is relative to the
// vault root and slash-separated, so it is the same key on every platform; see
// keepKey.
//
// It is a SET OF PATHS, not a map from ghost_id to path, and the reason is that
// the id cannot be read back out of a note. yamlScalar flattens a tab, a
// newline and a carriage return to a space so that every front-matter key stays
// on one line, so the id in a note is not always the id in the store: "tab\tid",
// "line\nid" and "tab id" all render to the same bytes and read back as one
// value. Keyed by that value, three distinct notes collided on one key and two of
// them lost — the note was written and then deleted by the pass that wrote it, on
// every export, for good.
//
// The path, and not the bare basename, is what the set is keyed on. A basename
// cannot say WHICH note this pass wrote: the same name occurs in two project
// folders and in two kinds of one (a memory and a task with the same title
// produce the same slug, and can share an id token), so a basename-keyed set
// keeps a stale copy alive on the strength of a live note elsewhere and the
// pass can never reclaim it. Keyed by path, the two questions separate: a path
// in the set is one of the notes this pass wrote at that exact location, and a
// path absent from it is stale (a deleted entity, a memory that moved projects,
// or a content edit that renamed the slug — the old-slug file is stale even
// though its id is still live).
type keepSet map[string]struct{}

// keepKey is the key a note is recorded under in a keepSet, and the key prune
// looks it up by: its path from the vault root, slash-separated so the key does
// not change with the platform's separator.
//
// A path with no relative form (one that is not under root at all) is recorded
// as its own absolute path, which is a key nothing will ever match. That is the
// safe direction: an entry that cannot be matched is a note prune will not
// delete, and a note that cannot be expressed is a note the pass wrote to the
// wrong place. prune refuses a subtree that escapes root, so the case does not
// arise for anything it walks.
func keepKey(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// prune deletes Ghost-managed .md files under the given vault subtrees whose
// path is not one of the canonical paths this pass wrote. All three guards
// from the spec are enforced. Orphaned *.ghost-tmp files (left by a crashed
// writeIfChanged) are also reclaimed — but only inside the managed subtrees,
// behind the marker guard.
func prune(root string, subtrees []string, keep keepSet, knownFolders []string) error {
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
			if strings.HasSuffix(path, ".ghost-tmp") {
				return os.Remove(path) // orphan from a crashed write
			}
			if !strings.HasSuffix(path, ".md") {
				return nil
			}
			// The front matter is what makes the file Ghost's to touch; the path
			// is what says whether it is the one this pass wrote.
			if _, ok := hasGhostID(path); ok {
				if _, kept := keep[keepKey(root, path)]; !kept {
					return os.Remove(path)
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
	// projects deleted from the DB since the last export. Skipped when
	// knownFolders is nil (filtered export — we can't know what's orphaned).
	if len(knownFolders) > 0 {
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
			if hasGhostContent(dir) {
				if err := pruneOrphanFolder(dir); err != nil {
					return fmt.Errorf("prune orphan folder %s: %w", e.Name(), err)
				}
			}
		}
	}
	return nil
}

// pruneOrphanFolder cleans a top-level folder whose project no longer exists.
// It removes only Ghost's own notes — regular .md files carrying a ghost_id in
// their frontmatter — and then any directory those removals left empty, deepest
// first, the orphan folder itself last. A user's file, attachment or subfolder
// keeps its parent directories. Symlinks are never followed (WalkDir does not
// descend into them) and never removed, and an entry that cannot be read is
// left alone rather than failing the export.
func pruneOrphanFolder(dir string) error {
	var dirs []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// A Windows junction or other reparse point can report as a
			// directory; never walk through one to files outside the folder.
			if fi, err := os.Lstat(path); err != nil || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
				return filepath.SkipDir
			}
			dirs = append(dirs, path)
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		if _, ok := hasGhostID(path); !ok {
			return nil
		}
		// A note Ghost cannot remove (a read-only user folder) is left in
		// place: failing here would fail every export and make sync retry
		// forever for a file that is merely stale.
		_ = os.Remove(path)
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		removeIfEmptyDir(dirs[i])
	}
	return nil
}

// removeIfEmptyDir removes path only while it is a real directory with no
// entries. os.Remove refuses a non-empty directory on every platform, so a
// file that appears between the check and the removal keeps the directory;
// any failure simply leaves it in place.
func removeIfEmptyDir(path string) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.IsDir() {
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(path)
}

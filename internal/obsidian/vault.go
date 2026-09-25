package obsidian

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wcatz/ghost/internal/maintenance"
)

const (
	markerName      = ".ghost-vault"
	ghostTempMinAge = time.Hour
)

var ghostTempNamePattern = regexp.MustCompile(`^.+\.md\.ghost-tmp-[A-Za-z0-9]+$`)

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
		if err := os.MkdirAll(dir, 0o755); err != nil {
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
	return os.WriteFile(filepath.Join(dir, markerName), []byte(`{"schema_version":1}`+"\n"), 0o644)
}

// writeIfChanged writes content atomically (temp+rename), skipping the write
// when the file already has identical content — no mtime churn.
func writeIfChanged(path, content string) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
	if err := f.Chmod(0o644); err != nil {
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

// isGhostTempFile recognizes the original fixed temp suffix and the exact
// generated form used by writeIfChanged's os.CreateTemp call. A broad
// substring match would delete user files such as note.ghost-tmp-draft.md.
func isGhostTempFile(path string) bool {
	name := filepath.Base(path)
	return strings.HasSuffix(name, ".ghost-tmp") || ghostTempNamePattern.MatchString(name)
}

func ghostTempIsOld(path string) bool {
	info, err := os.Stat(path)
	return err == nil && time.Since(info.ModTime()) >= ghostTempMinAge
}

// hasGhostID reports whether a file's frontmatter carries a ghost_id key —
// the only files prune may touch. Only the frontmatter block (between the
// opening and closing --- lines) is scanned, never the note body.
func hasGhostID(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return "", false
	}
	for _, line := range lines[1:] {
		if line == "---" { // end of frontmatter — stop before the body
			break
		}
		if id, ok := strings.CutPrefix(line, "ghost_id: "); ok {
			return strings.TrimSpace(id), true
		}
	}
	return "", false
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

// prune deletes Ghost-managed .md files under the given vault subtrees whose
// ghost_id is not in keep, or whose basename is not the canonical one for
// that ghost_id (a content edit renamed the slug — the old-slug file is
// stale even though its ID is still live). All three guards from the spec
// are enforced. Old, orphaned *.ghost-tmp files (left by a crashed
// writeIfChanged) are also reclaimed — only inside the managed subtrees,
// behind the marker guard, and after a grace period that protects an active
// concurrent writer.
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
			if isGhostTempFile(path) {
				if !ghostTempIsOld(path) {
					return nil // concurrent writer or recently crashed write
				}
				_, err := maintenance.RemoveFileIfUnheld(path)
				return err
			}
			if !strings.HasSuffix(path, ".md") {
				return nil
			}
			if id, ok := hasGhostID(path); ok {
				if canonical, kept := keep[id]; !kept || canonical != filepath.Base(path) {
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
				if err := os.RemoveAll(dir); err != nil {
					return fmt.Errorf("remove orphan folder %s: %w", e.Name(), err)
				}
			}
		}
	}
	return nil
}

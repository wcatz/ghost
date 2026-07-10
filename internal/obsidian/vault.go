package obsidian

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const markerName = ".ghost-vault"

// ensureVault prepares dir as a Ghost-managed mirror target. Fresh or empty
// dirs are initialized with the marker; a non-empty dir without the marker is
// refused — Ghost never adopts a folder it didn't create.
func ensureVault(dir string) error {
	entries, err := os.ReadDir(dir)
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
	tmp := path + ".ghost-tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
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

// prune deletes Ghost-managed .md files under the given vault subtrees whose
// ghost_id is not in keep. All three guards from the spec are enforced.
func prune(root string, subtrees []string, keep map[string]bool) error {
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
			if d.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			if id, ok := hasGhostID(path); ok && !keep[id] {
				return os.Remove(path)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

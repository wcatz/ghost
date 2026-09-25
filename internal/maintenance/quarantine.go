package maintenance

import (
	"fmt"
	"os"
	"path/filepath"
)

// quarantinePath atomically moves path to a unique sibling name. Once the
// rename succeeds, a new writer can only create a fresh file at path; any
// writer that already held the old inode now holds tombstone, where the
// open-file probe can identify it before removal.
func quarantinePath(path string) (string, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	placeholder, err := os.CreateTemp(dir, "."+base+".retention-*")
	if err != nil {
		return "", fmt.Errorf("create quarantine name for %s: %w", path, err)
	}
	tombstone := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		_ = os.Remove(tombstone)
		return "", fmt.Errorf("close quarantine name for %s: %w", path, err)
	}
	if err := os.Remove(tombstone); err != nil {
		return "", fmt.Errorf("remove quarantine placeholder for %s: %w", path, err)
	}
	if err := os.Rename(path, tombstone); err != nil {
		return "", fmt.Errorf("quarantine %s: %w", path, err)
	}
	return tombstone, nil
}

func restoreQuarantine(path, tombstone string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("cannot restore %s: replacement already exists", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat replacement %s: %w", path, err)
	}
	if err := os.Rename(tombstone, path); err != nil {
		return fmt.Errorf("restore %s: %w", path, err)
	}
	return nil
}

func removeQuarantine(tombstone string) error {
	info, err := os.Lstat(tombstone)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat quarantined file %s: %w", tombstone, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to remove non-regular quarantined file %s", tombstone)
	}
	if err := os.Remove(tombstone); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove quarantined file %s: %w", tombstone, err)
	}
	return nil
}

// removeUnheldFile quarantines path, verifies the moved inode is not held, and
// removes it only when the answer is definitive. A held or unverifiable file is
// restored and left for a later pass.
func removeUnheldFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, nil
	}
	tombstone, err := quarantinePath(path)
	if err != nil {
		return false, err
	}
	inUse, probeErr := openFileProbe(tombstone)
	if probeErr != nil {
		return false, errorsJoinRestore(path, tombstone, probeErr)
	}
	if inUse {
		return false, errorsJoinRestore(path, tombstone, nil)
	}
	if err := removeQuarantine(tombstone); err != nil {
		return false, err
	}
	return true, nil
}

func errorsJoinRestore(path, tombstone string, cause error) error {
	if err := restoreQuarantine(path, tombstone); err != nil {
		if cause != nil {
			return fmt.Errorf("%w; %v", cause, err)
		}
		return err
	}
	return cause
}

//go:build linux

package fileguard

import (
	"errors"

	"golang.org/x/sys/unix"
)

func publishNoReplace(stage, path string) error {
	// Filesystems without RENAME_NOREPLACE return EINVAL/ENOSYS; deferring is
	// safer than falling back to replacing rename.
	err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EEXIST) {
		return ErrPublishExists
	}
	return err
}

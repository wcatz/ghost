//go:build linux

package fileguard

import "golang.org/x/sys/unix"

func publishNoReplace(stage, path string) error {
	err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE)
	if err == unix.EEXIST {
		return ErrPublishExists
	}
	return err
}

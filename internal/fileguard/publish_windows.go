//go:build windows

package fileguard

import (
	"errors"

	"golang.org/x/sys/windows"
)

func publishNoReplace(stage, path string) error {
	from, err := windows.UTF16PtrFromString(stage)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	// Flags = 0 is the critical part: MOVEFILE_REPLACE_EXISTING is forbidden.
	if err := windows.MoveFileEx(from, to, 0); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return ErrPublishExists
		}
		return err
	}
	return nil
}

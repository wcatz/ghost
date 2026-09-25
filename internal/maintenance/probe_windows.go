//go:build windows

package maintenance

import (
	"errors"

	"golang.org/x/sys/windows"
)

func renameMeansHeld(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// nativeOpenProbe attempts an exclusive open. Existing holders that do not
// share access report a sharing/access violation, which is treated as held.
func nativeOpenProbe(path string) (inUse, known bool, err error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, false, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return true, true, nil
		}
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return false, true, nil
		}
		return false, false, err
	}
	_ = windows.CloseHandle(handle)
	return false, true, nil
}

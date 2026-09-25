//go:build windows

package fileguard

import (
	"context"
	"errors"

	"golang.org/x/sys/windows"
)

func nativeOpenProbe(ctx context.Context, path string) (inUse, known bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, false, err
	}
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		handle, createErr := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if createErr == nil {
			_ = windows.CloseHandle(handle)
		}
		done <- result{err: createErr}
	}()
	var createErr error
	select {
	case <-ctx.Done():
		return false, false, ctx.Err()
	case res := <-done:
		createErr = res.err
	}
	if createErr != nil {
		if errors.Is(createErr, windows.ERROR_SHARING_VIOLATION) || errors.Is(createErr, windows.ERROR_ACCESS_DENIED) {
			return true, true, nil
		}
		if errors.Is(createErr, windows.ERROR_FILE_NOT_FOUND) || errors.Is(createErr, windows.ERROR_PATH_NOT_FOUND) {
			return false, true, nil
		}
		return false, false, createErr
	}
	return false, true, nil
}

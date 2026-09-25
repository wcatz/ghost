//go:build !windows

package maintenance

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// nativeOpenProbe uses Linux procfs when lsof/fuser are unavailable. It
// returns known=false on platforms without a reliable native handle view.
func nativeOpenProbe(path string) (inUse, known bool, err error) {
	if runtime.GOOS != "linux" {
		return false, false, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, false, err
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return false, false, err
	}
	unverifiable := false
	for _, proc := range procs {
		if !proc.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(proc.Name()); err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", proc.Name(), "fd")
		fds, readErr := os.ReadDir(fdDir)
		if readErr != nil {
			if errors.Is(readErr, os.ErrPermission) {
				unverifiable = true
			}
			continue
		}
		for _, fd := range fds {
			target, linkErr := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if linkErr != nil {
				if errors.Is(linkErr, os.ErrPermission) {
					unverifiable = true
				}
				continue
			}
			if target == abs || target == abs+" (deleted)" {
				return true, true, nil
			}
		}
	}
	if unverifiable {
		return false, false, nil
	}
	return false, true, nil
}

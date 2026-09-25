//go:build linux

package fileguard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func nativeOpenProbe(ctx context.Context, path string) (inUse, known bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, false, err
	}
	resolved := abs
	if evaluated, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		resolved = evaluated
	}
	targetInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, true, nil
		}
		return false, false, err
	}
	stat, ok := targetInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return false, false, fmt.Errorf("stat identity unavailable for %s", path)
	}
	dev := fmt.Sprintf("%02x:%02x", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)))
	ino := strconv.FormatUint(uint64(stat.Ino), 10)

	procs, err := os.ReadDir("/proc")
	if err != nil {
		return false, false, err
	}
	unverifiable := false
	for _, proc := range procs {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		if !proc.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(proc.Name()); err != nil {
			continue
		}
		procDir := filepath.Join("/proc", proc.Name())
		fdDir := filepath.Join(procDir, "fd")
		fds, readErr := os.ReadDir(fdDir)
		if readErr == nil {
			for _, fd := range fds {
				if err := ctx.Err(); err != nil {
					return false, false, err
				}
				fdPath := filepath.Join(fdDir, fd.Name())
				target, linkErr := os.Readlink(fdPath)
				if linkErr == nil && (target == abs || target == resolved ||
					target == abs+" (deleted)" || target == resolved+" (deleted)") {
					return true, true, nil
				}
				fdInfo, fdErr := os.Stat(fdPath)
				if fdErr == nil && os.SameFile(targetInfo, fdInfo) {
					return true, true, nil
				}
				if errors.Is(linkErr, os.ErrPermission) || errors.Is(fdErr, os.ErrPermission) {
					unverifiable = true
				}
			}
		} else if errors.Is(readErr, os.ErrPermission) {
			unverifiable = true
		}

		matches, mapsErr := procMapsContain(ctx, filepath.Join(procDir, "maps"), dev, ino)
		if mapsErr != nil {
			if os.IsNotExist(mapsErr) {
				continue
			}
			if errors.Is(mapsErr, os.ErrPermission) {
				unverifiable = true
				continue
			}
			return false, false, mapsErr
		}
		if matches {
			return true, true, nil
		}
	}
	if unverifiable {
		return false, false, nil
	}
	return false, true, nil
}

func procMapsContain(ctx context.Context, path, dev, ino string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close() //nolint:errcheck
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for lineNo := 0; scanner.Scan(); lineNo++ {
		if lineNo%256 == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 5 && fields[3] == dev && fields[4] == ino {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, ctx.Err()
}

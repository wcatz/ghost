package scratch

import (
	"errors"
	"os"
)

var errNotADir = errors.New("path is not the directory first lstat'ed")

func sameRealDir(path string, want os.FileInfo) error {
	got, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if got.Mode()&os.ModeSymlink != 0 || !got.IsDir() {
		return errNotADir
	}
	if !os.SameFile(want, got) {
		return errNotADir
	}
	return nil
}

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wcatz/ghost/internal/selfupdate"
)

// DevForbidDataDirEnv names data directories a non-release build refuses to open.
// Values are separated by os.PathListSeparator.
const DevForbidDataDirEnv = "GHOST_DEV_FORBID_DATA_DIR"

// CheckDevDataDir reports an error when dataDir is listed in DevForbidDataDirEnv
// and version is not a release build.
func CheckDevDataDir(version, dataDir string) error {
	if selfupdate.IsRelease(version) {
		return nil
	}
	listed := os.Getenv(DevForbidDataDirEnv)
	if listed == "" {
		return nil
	}
	want, err := canonicalDataDir(dataDir)
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(listed, string(os.PathListSeparator)) {
		if entry == "" {
			continue
		}
		got, err := canonicalDataDir(entry)
		if err != nil {
			continue
		}
		if got == want {
			return fmt.Errorf("%s refuses to open the data directory %s: this build is %q, which is not a release, and a development build migrates a store it opens. Run a released ghost against this directory, or unset %s",
				DevForbidDataDirEnv, want, version, DevForbidDataDirEnv)
		}
	}
	return nil
}

func canonicalDataDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dir, rest := abs, ""
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

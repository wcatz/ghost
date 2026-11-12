// Package scratch owns the temporary space Ghost's LLM-harness children use.
package scratch

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/procstat"
)

const envDir = "GHOST_SCRATCH_DIR"

const ownerFile = ".owner"

const startNone = "none"

// Root returns the scratch root, creating it (0700) if needed. The root is
func Root() (string, error) {
	root := os.Getenv(envDir)
	if root == "" {
		dataDir, err := config.DataDir()
		if err != nil {
			return "", fmt.Errorf("scratch root: %w", err)
		}
		root = filepath.Join(dataDir, "scratch")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("scratch root %s: %w", root, err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("scratch root %s: %w", root, err)
	}
	return root, nil
}

// Dir is one per-invocation scratch directory. Its zero value is not usable;
type Dir struct {
	path string
}

// UniqueSuffix returns a token no other run of this binary produces for the same
func UniqueSuffix() (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	return strconv.Itoa(os.Getpid()) + "-" + token, nil
}

func randomToken() (string, error) {
	token, err := randomHex(8)
	if err != nil {
		return "", fmt.Errorf("scratch token: %w", err)
	}
	return token, nil
}

// Open creates a fresh per-invocation directory directly under the root,
func Open() (*Dir, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	pid := os.Getpid()
	path := filepath.Join(root, strconv.Itoa(pid)+"-"+token)
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, fmt.Errorf("scratch dir: %w", err)
	}
	start, haveStart := procstat.StartTime(pid)
	marker := ownerMarkerText(pid, token, start, haveStart)
	if err := os.WriteFile(filepath.Join(path, ownerFile), []byte(marker), 0o600); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("scratch owner marker: %w", err)
	}
	return &Dir{path: path}, nil
}

func ownerMarkerText(pid int, token, start string, haveStart bool) string {
	if !haveStart {
		start = startNone
	}
	return "pid=" + strconv.Itoa(pid) + "\ntoken=" + token + "\nstart=" + start + "\n"
}

var removeTree = os.RemoveAll
var removeFile = os.Remove

// SetRemoveTreeForTest replaces the tree removal for another package's test
func SetRemoveTreeForTest(f func(string) error) (restore func()) {
	prev := removeTree
	removeTree = f
	return func() { removeTree = prev }
}

// Path returns the directory's absolute path.
func (d *Dir) Path() string { return d.path }

func makeRemovable(path string) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return
	}
	if f, err := os.OpenFile(path, ownerOpenFlags, 0); err == nil {
		defer func() { _ = f.Close() }()
		if fi, err := f.Stat(); err != nil || !fi.IsDir() {
			return
		}
		if err := f.Chmod(0o700); err != nil {
			return
		}
		entries, err := f.ReadDir(-1)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if entry.IsDir() {
				makeRemovable(filepath.Join(path, entry.Name()))
			}
		}
		return
	}
	if err := sameRealDir(path, info); err != nil {
		return
	}
	_ = os.Chmod(path, 0o700)
	if err := sameRealDir(path, info); err != nil {
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			makeRemovable(filepath.Join(path, entry.Name()))
		}
	}
}

// Release removes the directory and everything under it. It restores owner
func (d *Dir) Release() {
	if d == nil {
		return
	}
	makeRemovable(d.path)
	_ = os.RemoveAll(d.path)
}

// Reap removes stale per-invocation directories from the scratch root. It
func Reap(maxAge time.Duration) (removed, failed int, err error) {
	root, err := Root()
	if err != nil {
		return 0, 0, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("scratch root %s: %w", root, err)
	}
	now := time.Now()
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			continue // vanished mid-scan
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue // never follow or remove links
		}
		if info.Mode().IsRegular() && isReassessIDFile(entry.Name()) {
			if now.Sub(info.ModTime()) > idFileStaleAge {
				if rmErr := removeFile(path); rmErr == nil {
					removed++
				} else if !os.IsNotExist(rmErr) {
					failed++
				}
			}
			continue
		}
		stale := now.Sub(info.ModTime()) > maxAge
		pid, start, ok := ownerMarker(path)
		switch {
		case !ok:
			if !isScratchDirName(entry.Name()) || !stale {
				continue
			}
		case start == startNone:
			if procstat.IsAlive(pid, "", false) {
				continue
			}
		case start == "":
			if !stale {
				continue
			}
		default:
			if procstat.IsAlive(pid, start, true) {
				continue
			}
		}
		makeRemovable(path)
		if rmErr := removeTree(path); rmErr == nil {
			removed++
		} else if !os.IsNotExist(rmErr) {
			failed++
		}
	}
	return removed, failed, nil
}

const ownerMarkerMaxBytes = 4096

func ownerMarker(dir string) (pid int, start string, ok bool) {
	path := filepath.Join(dir, ownerFile)
	if li, err := os.Lstat(path); err != nil || !li.Mode().IsRegular() {
		return 0, "", false
	}
	f, err := os.OpenFile(path, ownerOpenFlags, 0)
	if err != nil {
		return 0, "", false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0, "", false
	}
	data, err := io.ReadAll(io.LimitReader(f, ownerMarkerMaxBytes+1))
	if err != nil {
		return 0, "", false
	}
	if len(data) > ownerMarkerMaxBytes {
		return 0, "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "pid":
			p, err := strconv.Atoi(value)
			if err != nil || p <= 0 {
				return 0, "", false
			}
			pid, ok = p, true
		case "start":
			start = value
		}
	}
	return pid, start, ok
}

func isScratchDirName(name string) bool {
	pid, rest, found := strings.Cut(name, "-")
	if !found || !isPidSegment(pid) {
		return false
	}
	if len(rest) != 16 {
		return false
	}
	for _, r := range rest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func isPidSegment(s string) bool {
	if s == "" || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

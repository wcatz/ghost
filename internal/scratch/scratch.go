// Package scratch owns the temporary space Ghost's LLM-harness children use.
//
// Every harness spawn (claude, opencode, codex, goose) runs confined to a
// per-invocation directory under a Ghost-owned root, with TMPDIR/TMP/TEMP and
// its working directory pointed inside that directory. The root lives in the
// data dir — or $GHOST_SCRATCH_DIR when that is set — rather than the shared
// system temp because opencode, in particular, writes a hidden JIT-cache
// shared object (~4.7 MiB) into its temp dir on every invocation, and one
// consolidation lifecycle spawns hundreds of harness processes. Left in the
// shared temp dir those droppings accumulate until the filesystem fills, at
// which point every LLM-backed phase fails. Under the owned root each
// invocation's directory is removed by its owner, and Reap collects whatever a
// crashed owner left behind.
//
// Nothing here reads or writes the inherited temp-dir variables: which
// variables a child gets is the spawn site's business (internal/ai pins them),
// and this package must keep working when the system temp dir is broken, which
// is the situation it exists to survive.
package scratch

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wcatz/ghost/internal/config"
)

// envDir names the environment variable that overrides the scratch root. An
// empty value is treated as unset so a blank export cannot silently move the
// root to the process's current directory.
const envDir = "GHOST_SCRATCH_DIR"

// ownerFile is the marker written inside every per-invocation directory. It
// records the pid that created the directory and a random token identifying
// the invocation, one key=value per line.
const ownerFile = ".owner"

// Root returns the scratch root, creating it (0700) if needed. The root is
// $GHOST_SCRATCH_DIR when set and non-empty, otherwise <dataDir>/scratch. The
// permissions are tightened explicitly because MkdirAll leaves an existing
// directory's mode alone and the override may name a pre-existing shared dir.
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
// it is returned only by Open.
type Dir struct {
	path string
}

// Open creates a fresh per-invocation directory directly under the root,
// named <pid>-<random-hex>, and writes the ownerFile marker inside it with the
// creating pid and a random token. The marker lets Reap tell a directory whose
// owner is still running from one abandoned by a crash.
//
// Open touches nothing in the inherited temp dir, so it succeeds even when
// TMPDIR points at a full or nonexistent filesystem — the failure mode Ghost's
// harnesses must survive.
func Open() (*Dir, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	token, err := randomHex(8)
	if err != nil {
		return nil, fmt.Errorf("scratch token: %w", err)
	}
	path := filepath.Join(root, strconv.Itoa(os.Getpid())+"-"+token)
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, fmt.Errorf("scratch dir: %w", err)
	}
	marker := "pid=" + strconv.Itoa(os.Getpid()) + "\ntoken=" + token + "\n"
	if err := os.WriteFile(filepath.Join(path, ownerFile), []byte(marker), 0o600); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("scratch owner marker: %w", err)
	}
	return &Dir{path: path}, nil
}

// Path returns the directory's absolute path.
func (d *Dir) Path() string { return d.path }

// Release removes the directory and everything under it. It is best-effort:
// removal errors are ignored because Release also runs on early returns and on
// paths Reap may already have collected. It is safe to call more than once and
// safe when the directory is already gone.
func (d *Dir) Release() {
	if d == nil {
		return
	}
	_ = os.RemoveAll(d.path)
}

// Reap removes stale per-invocation directories from the scratch root. It
// scans only the root's direct entries and never the root itself.
//
// Only entries demonstrably created by Ghost are eligible for removal: one
// with a readable ownerFile marker, or — for a markerless entry, including one
// whose marker is being written right now — one whose name has exactly the
// shape Open creates. A markerless entry is removed only once its mtime is
// older than maxAge. Anything else, such as a foreign file or directory that
// happens to live under the root, is left alone: $GHOST_SCRATCH_DIR may point
// at a shared directory, and Ghost must never delete what it did not create.
// Symlinks are left alone entirely: the root is Ghost-owned, but a link can
// point anywhere, and removing one must never follow it.
//
// Liveness mirrors internal/mcpinit's pid check (signal 0, which tests
// existence without signaling) and is the primary shield: when the marker
// parses and its pid is alive the entry is skipped unconditionally, with no
// age ceiling and no mtime consultation. A leaked directory can therefore
// persist while its recorded pid is reused by a live unrelated process, which
// is accepted because (a) it holds at most one invocation's droppings — it is
// disk, not correctness — and (b) it is collected as soon as that pid exits.
// No age ceiling applies to a live owner because phase timeouts can be
// configured to 0 (unbounded), so a ceiling could delete a live invocation's
// scratch directory; an unconditional shield cannot.
//
// Removal failures and entries that vanish mid-scan are ignored — several
// Ghost processes (one MCP server per client session) may reap concurrently,
// so races are normal — and only entries actually removed are counted. A
// missing root is not an error; an unreadable one is returned so the caller
// can warn, but callers proceed either way.
func Reap(maxAge time.Duration) (int, error) {
	root, err := Root()
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("scratch root %s: %w", root, err)
	}
	now := time.Now()
	removed := 0
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			continue // vanished mid-scan
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue // never follow or remove links
		}
		stale := now.Sub(info.ModTime()) > maxAge
		if pid, ok := ownerPID(path); ok {
			// The marker proves ownership, and a live owner is shielded
			// unconditionally: phase timeouts may be unbounded, so any age
			// ceiling could tear a running invocation's scratch away.
			if processAlive(pid) {
				continue
			}
		} else {
			// No marker: only Open's exact name shape proves this is Ghost's,
			// and only the age backstop justifies removal.
			if !isScratchDirName(entry.Name()) || !stale {
				continue
			}
		}
		if err := os.RemoveAll(path); err == nil {
			removed++
		}
	}
	return removed, nil
}

// ownerPID reads the pid from dir's ownerFile marker. ok is false when the
// marker is missing, unreadable, or malformed; without a marker, Reap collects
// the directory only when its name matches the Open shape and the mtime age
// backstop has passed.
func ownerPID(dir string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(dir, ownerFile))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || key != "pid" {
			continue
		}
		pid, err := strconv.Atoi(value)
		if err != nil || pid <= 0 {
			return 0, false
		}
		return pid, true
	}
	return 0, false
}

// isScratchDirName reports whether name has exactly the shape Open creates:
// <decimal pid>-<16 lowercase hex chars>. It exists because Reap must only
// remove entries it can prove Ghost created — an override root may be shared —
// so for a markerless entry the name is the only ownership evidence there is.
// The match is deliberately strict (no sign, spaces, extra dashes, or
// uppercase hex) so a foreign name cannot accidentally qualify.
func isScratchDirName(name string) bool {
	pid, rest, found := strings.Cut(name, "-")
	if !found || pid == "" {
		return false
	}
	for _, r := range pid {
		if r < '0' || r > '9' {
			return false
		}
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

// processAlive reports whether pid names a running process, mirroring
// internal/mcpinit's isProcessAlive on POSIX. Windows' Process.Signal does not
// support signal 0, but FindProcess there already opens the process and fails
// when it is gone, so reaching the platform check means alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// randomHex returns n random bytes as lowercase hex.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

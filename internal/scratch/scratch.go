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
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/procstat"
)

// envDir names the environment variable that overrides the scratch root. An
// empty value is treated as unset so a blank export cannot silently move the
// root to the process's current directory.
const envDir = "GHOST_SCRATCH_DIR"

// ownerFile is the marker written inside every per-invocation directory, one
// key=value per line:
//
//	pid=<creating pid>
//	token=<random per-invocation token>
//	start=<process creation-time token, or "none" when unavailable>
//
// The start line carries the same PID-reuse token internal/mcpinit records in
// its .pid files (internal/procstat): Reap compares it against the live
// process's token so a pid the OS recycled after the owner died is detected
// instead of being mistaken for a live owner. The startNone sentinel is
// distinct from a missing line: it says the platform cannot supply tokens at
// all, so Reap still shields a live owner unconditionally. Only a marker with
// no start line at all (written before tokens existed) falls back to Reap's
// age backstop.
const ownerFile = ".owner"

// startNone is the ownerFile start line's sentinel for a platform (or read)
// where procstat.StartTime cannot supply a creation-time token. Without it, a
// marker from such a platform would be indistinguishable from a true legacy
// marker and would lose the unconditional shield while its owner is alive —
// exactly the shield Open's callers rely on.
const startNone = "none"

// RootPath returns the configured scratch root without creating it. The
// permissions are tightened only by Root, after the directory exists.
func RootPath() (string, error) {
	root := os.Getenv(envDir)
	if root == "" {
		dataDir, err := config.DataDirPath()
		if err != nil {
			return "", fmt.Errorf("scratch root: %w", err)
		}
		root = filepath.Join(dataDir, "scratch")
	}
	return root, nil
}

// Root returns the scratch root, creating it (0700) if needed. The root is
// $GHOST_SCRATCH_DIR when set and non-empty, otherwise <dataDir>/scratch. The
// permissions are tightened explicitly because MkdirAll leaves an existing
// directory's mode alone and the override may name a pre-existing shared dir.
func Root() (string, error) {
	root, err := RootPath()
	if err != nil {
		return "", err
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
// creating pid, a random per-invocation token, and the process's
// creation-time token. The marker lets Reap tell a directory whose owner is
// still running from one abandoned by a crash, and the creation-time token
// additionally lets it tell a live owner from a pid the OS recycled after the
// owner exited.
//
// When the token cannot be read — an unsupported platform, an unreadable proc
// entry — the marker records the explicit startNone sentinel instead of
// omitting the line. That is NOT a legacy marker: it says liveness is all
// this process can prove, so Reap keeps shielding the directory while the pid
// is alive and removes it once the pid is dead. (Only a marker written before
// tokens existed has no start line, and only that form falls back to the age
// backstop.)
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

// ownerMarkerText builds the .owner marker content. When haveStart is false
// the start line carries the startNone sentinel rather than being omitted, so
// Reap can tell a platform that cannot supply tokens (shield a live pid) from
// a true pre-token legacy marker (age backstop only).
func ownerMarkerText(pid int, token, start string, haveStart bool) string {
	if !haveStart {
		start = startNone
	}
	return "pid=" + strconv.Itoa(pid) + "\ntoken=" + token + "\nstart=" + start + "\n"
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
// A marked entry is owned and live only when its pid is alive AND, when the
// marker carries a creation-time token, that token still matches the live
// process's — the same pid-reuse token internal/mcpinit uses for .pid files. A
// dead pid, or a live pid whose token differs (the OS recycled it to an
// unrelated process), means the directory is abandoned and it is removed
// regardless of age. A startNone sentinel means the platform cannot supply
// tokens: liveness alone is all that marker can prove, so a live pid is still
// shielded unconditionally and a dead one is removed. Only a marker with no
// start line at all predates tokens: it is not shielded on liveness alone and
// is removed once older than maxAge, so the transition cannot pin old leaks
// forever. A genuine live owner — pid alive with a matching token, or with the
// sentinel — is shielded unconditionally, with no age ceiling and no mtime
// consultation, because phase timeouts can be configured to 0 (unbounded), so
// any ceiling could delete a live invocation's scratch directory.
//
// The bound is therefore honest: a pid the OS recycled is detected by the
// token mismatch and collected immediately, a directory abandoned by a dead
// owner is collected on the next reap, and a live owner's leaked directories
// last only as long as that owner lives — its exit turns them into the
// dead-pid case for the next reap.
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
		pid, start, ok := ownerMarker(path)
		switch {
		case !ok:
			// No marker: only Open's exact name shape proves this is Ghost's,
			// and only the age backstop justifies removal.
			if !isScratchDirName(entry.Name()) || !stale {
				continue
			}
		case start == startNone:
			// The platform cannot supply creation-time tokens, so liveness is
			// all this marker can prove. haveToken=false is IsAlive's explicit
			// no-token mode (signal 0 / STILL_ACTIVE), not a fake token, and a
			// live owner is shielded unconditionally.
			if procstat.IsAlive(pid, "", false) {
				continue
			}
		case start == "":
			// True pre-token legacy marker: liveness alone does not shield it;
			// only the age backstop does.
			if !stale {
				continue
			}
		default:
			// Token present: alive and matching is a live owner; dead or a
			// recycled pid (token mismatch) is abandoned.
			if procstat.IsAlive(pid, start, true) {
				continue
			}
		}
		if err := os.RemoveAll(path); err == nil {
			removed++
		}
	}
	return removed, nil
}

// ownerMarker reads dir's ownerFile marker. ok is false when the marker is
// missing, unreadable, or has no usable pid; without one, Reap collects the
// directory only when its name matches the Open shape and the mtime age
// backstop has passed. start is the recorded process creation-time token, the
// startNone sentinel when the writer could not read one, or "" for a true
// legacy marker written before start lines existed.
func ownerMarker(dir string) (pid int, start string, ok bool) {
	data, err := os.ReadFile(filepath.Join(dir, ownerFile))
	if err != nil {
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

// randomHex returns n random bytes as lowercase hex.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

package mcpinit

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/config"
)

// The working-moment channel (a block delivered beside a user message or an
// edit's result) keeps one small file per session in the data directory:
//
//	working-moment-<session>.log
//
// It answers two questions across separate short-lived hook processes: which
// rows has this session already been shown, and how many bytes of the host's
// output allowance has the session spent. The session-start block writes the
// first entry (its rows and its bytes), so a row it delivered is never repeated
// and the two channels draw on one budget.
//
// Naming, sanitising and sweeping follow the lifecycle markers beside it: the
// file lives in config.DataDirPath() (a refused resolve means do nothing, never
// a fallback), the session id is untrusted and is mapped onto a file-name-safe
// stem by sanitizeMarkerProject (an empty id means there is no marker and so no
// delivery), the file is private (0600), and anything older than
// workMomentMarkerMaxAge is deleted the next time a session starts.
//
// Concurrency. Parallel tool calls in one turn run their hooks at the same time,
// so a read-modify-write would deliver a row twice or lose a spend. The file is
// therefore APPEND-ONLY and every entry is claimed before it is used: a hook
// appends one line (O_APPEND, under PIPE_BUF), re-reads the file and delivers
// only if its own entry is VALID, where validity is a pure function of the file
// read in order — an entry is valid when none of its ids was claimed by an
// earlier valid entry and its bytes fit what the earlier valid entries left. Two
// hooks that race therefore agree on who won, and the loser emits nothing.
const (
	workMomentMarkerPrefix = "working-moment-"
	workMomentMarkerSuffix = ".log"
	// workMomentMarkerMaxAge is the sweep horizon. A session is not that long
	// lived, and a marker outliving its session only costs a file.
	workMomentMarkerMaxAge = 7 * 24 * time.Hour
	// workMomentBudget is the session's TOTAL allowance across both channels,
	// the host's own 10,000-character output limit: the session-start block and
	// every working-moment delivery together stay under it.
	workMomentBudget = sessionHostLimit
	// workMomentMarkerMaxBytes bounds what a read will parse, so a marker that
	// grew without limit cannot turn a hook into a long read.
	workMomentMarkerMaxBytes = 256 << 10
	startToken               = "start"
)

// momentEntry is one claim in the marker.
type momentEntry struct {
	token string
	bytes int
	ids   []string
}

// momentState is the marker read in order: the ids already delivered and the
// bytes already spent, counting only the valid entries.
type momentState struct {
	delivered map[string]bool
	spent     int
	// valid reports, per token, whether that entry was valid.
	valid map[string]bool
}

// momentMarkerPath is the marker's path for a session, or "" when there is no
// marker to be had: no session id, or a data directory this process may not
// resolve.
func momentMarkerPath(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	dataDir, err := config.DataDirPath()
	if err != nil || dataDir == "" {
		return ""
	}
	stem := sanitizeMarkerProject(sessionID)
	if !safeProjectIDComponent(stem) {
		return ""
	}
	return filepath.Join(dataDir, workMomentMarkerPrefix+stem+workMomentMarkerSuffix)
}

// readMomentState parses the marker in order. A missing file is an empty
// session; an unreadable one is an error, which the caller turns into silence.
// With no start entry the session start is ASSUMED to have spent its whole cap
// (sessionStartByteCap): a session that began before this build, or whose start
// could not write, must not be allowed to overspend on a guess.
func readMomentState(path string) (momentState, error) {
	st := momentState{delivered: map[string]bool{}, valid: map[string]bool{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			st.spent = sessionStartByteCap
			return st, nil
		}
		return st, err
	}
	defer f.Close() //nolint:errcheck
	sc := bufio.NewScanner(io.LimitReader(f, workMomentMarkerMaxBytes))
	sc.Buffer(make([]byte, 0, 64<<10), 64<<10)
	sawStart := false
	for sc.Scan() {
		e, ok := parseMomentEntry(sc.Text())
		if !ok {
			continue
		}
		clash := false
		for _, id := range e.ids {
			if st.delivered[id] {
				clash = true
				break
			}
		}
		if clash || st.spent+e.bytes > workMomentBudget {
			st.valid[e.token] = false
			continue
		}
		for _, id := range e.ids {
			st.delivered[id] = true
		}
		st.spent += e.bytes
		st.valid[e.token] = true
		if e.token == startToken {
			sawStart = true
		}
	}
	if err := sc.Err(); err != nil {
		return st, err
	}
	if !sawStart {
		st.spent += sessionStartByteCap
	}
	return st, nil
}

// parseMomentEntry reads `D <token> <bytes> <id> <id> ...`.
func parseMomentEntry(line string) (momentEntry, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "D" {
		return momentEntry{}, false
	}
	n, err := strconv.Atoi(fields[2])
	if err != nil || n < 0 {
		return momentEntry{}, false
	}
	return momentEntry{token: fields[1], bytes: n, ids: fields[3:]}, true
}

// appendMomentEntry appends one claim as a single O_APPEND write.
func appendMomentEntry(path string, e momentEntry) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("D %s %d %s\n", e.token, e.bytes, strings.Join(e.ids, " "))
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// recordSessionStartDelivery writes the session-start block's own entry: the
// ids of the rows it rendered and the bytes it spent. Best-effort, and silent on
// every failure (including a refused data directory): the block is already on
// its way and a missing marker only makes the later channel more cautious.
// It also sweeps markers older than workMomentMarkerMaxAge.
func recordSessionStartDelivery(sessionID string, ids []string, bytes int) {
	path := momentMarkerPath(sessionID)
	if path == "" {
		return
	}
	sweepMomentMarkers(filepath.Dir(path), time.Now())
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		if momentIDOK(id) {
			clean = append(clean, id)
		}
	}
	_ = appendMomentEntry(path, momentEntry{token: startToken, bytes: bytes, ids: clean})
}

// momentIDOK reports whether an id can travel on a marker line: non-empty and
// free of whitespace, which would split it.
func momentIDOK(id string) bool {
	return id != "" && !strings.ContainsAny(id, " \t\r\n")
}

// sweepMomentMarkers deletes this feature's markers older than the horizon. It
// only ever touches regular files with its own prefix and suffix.
func sweepMomentMarkers(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, workMomentMarkerPrefix) || !strings.HasSuffix(name, workMomentMarkerSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if now.Sub(info.ModTime()) > workMomentMarkerMaxAge {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

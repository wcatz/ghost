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

const (
	workMomentMarkerPrefix   = "working-moment-"
	workMomentMarkerSuffix   = ".log"
	workMomentMarkerMaxAge   = 7 * 24 * time.Hour
	workMomentBudget         = sessionHostLimit
	workMomentMarkerMaxBytes = 256 << 10
	startToken               = "start"
)

type momentEntry struct {
	token string
	bytes int
	ids   []string
}

type momentState struct {
	delivered map[string]bool
	spent     int
	valid     map[string]bool
}

func momentMarkerPath(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	dataDir, err := config.DataDirPath()
	if err != nil || dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, workMomentMarkerPrefix+markerStem(sessionID)+workMomentMarkerSuffix)
}

func readMomentState(path string) (momentState, error) {
	st := momentState{delivered: map[string]bool{}, valid: map[string]bool{}}
	f, err := openMarker(path)
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
	st.spent = sessionStartByteCap
	assumed := true
	for sc.Scan() {
		e, ok := parseMomentEntry(sc.Text())
		if !ok {
			continue
		}
		base := st.spent
		isStart := e.token == startToken
		if isStart && assumed {
			base -= sessionStartByteCap
		}
		clash := false
		if !isStart {
			for _, id := range e.ids {
				if st.delivered[id] {
					clash = true
					break
				}
			}
		}
		if clash || base+e.bytes > workMomentBudget {
			st.valid[e.token] = false
			if isStart {
				st.spent = base + e.bytes
				assumed = false
			}
			continue
		}
		for _, id := range e.ids {
			st.delivered[id] = true
		}
		st.spent = base + e.bytes
		st.valid[e.token] = true
		if isStart {
			assumed = false
		}
	}
	if err := sc.Err(); err != nil {
		return st, err
	}
	return st, nil
}

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

func appendMomentEntry(path string, e momentEntry) error {
	f, err := os.OpenFile(path, momentAppendOpenFlags, 0o600) // #nosec G304 -- a marker inside the Ghost data directory, not caller input
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err != nil {
			return err
		}
		return fmt.Errorf("%w (%s)", errNotRegularMarker, info.Mode().Type())
	}
	line := fmt.Sprintf("D %s %d %s\n", e.token, e.bytes, strings.Join(e.ids, " "))
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

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

func momentIDOK(id string) bool {
	return id != "" && !strings.ContainsAny(id, " \t\r\n")
}

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

package mcpinit

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

const logRotateCap = 5 << 20

var createLog = func(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w (%s)", path, errNotRegularMarker, info.Mode().Type())
	}
	return openRegularAppend(path, logAppendOpenFlags)
}

var createOperatorLog = func(path string) (*os.File, error) {
	return openRegularAppend(path, operatorLogOpenFlags)
}

func openRegularAppend(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags, 0o600) // #nosec G304 -- a log inside the Ghost data directory or the operator's own GHOST_LOG_FILE
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w (%s)", errNotRegularMarker, info.Mode().Type())
	}
	return f, nil
}

func openLogForAppend(path string) (*os.File, error) {
	return openLogWith(path, createLog)
}

func openLogWith(path string, createLog func(string) (*os.File, error)) (*os.File, error) {
	rotatedPath := path + ".1"
	rotateLogIfOversized(path, rotatedPath, os.Lstat)

	if f, err := createLog(path); err == nil {
		return f, nil
	} else if _, lerr := os.Lstat(path); lerr == nil {
		return nil, err
	} else if fi, rerr := os.Lstat(rotatedPath); rerr != nil || !fi.Mode().IsRegular() {
		return nil, err
	} else if f2, err2 := createLog(rotatedPath); err2 == nil {
		slog.Warn("ghost: log could not be opened under its own name, appending through the rotated copy instead",
			"path", path, "rotated", rotatedPath, "error", err)
		return f2, nil
	} else {
		return nil, fmt.Errorf("open log %s after rotating it to %s: %w", path, rotatedPath, errors.Join(err, err2))
	}
}

func rotateLogIfOversized(path, rotatedPath string, probe func(string) (os.FileInfo, error)) bool {
	if !logAtOrOverCap(path, probe) {
		return false
	}
	if !logAtOrOverCap(path, probe) {
		return false
	}
	return os.Rename(path, rotatedPath) == nil
}

func logAtOrOverCap(path string, probe func(string) (os.FileInfo, error)) bool {
	info, err := probe(path)
	return err == nil && info.Mode().IsRegular() && info.Size() >= logRotateCap
}

// LogRotateCap is logRotateCap for a caller outside this package.
const LogRotateCap = logRotateCap

// OpenLogForAppend is openLogForAppend for a caller outside this package: the
// MCP server's GHOST_LOG_FILE redirect names an arbitrary path rather than a
// data-dir file, and takes the same cap, single ".1" copy and 0600 create. The
// path is the operator's choice, so a symlink there is followed; a FIFO or
// other special file is refused.
func OpenLogForAppend(path string) (*os.File, error) {
	return openLogWith(path, createOperatorLog)
}

// RotatingLog is an append-only log writer that holds the cap while it is
// open. openLogForAppend checks the size once, at open, which is enough for a
// log opened per run but not for the MCP server's GHOST_LOG_FILE redirect: the
// server keeps one descriptor for its whole life, so under GHOST_DEBUG a
// session that stays open for days would grow the file without bound.
type RotatingLog struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	size   int64
	limit  int64
	closed bool
}

const retryAfterFailedRotation = 1 << 20

// OpenRotatingLog opens path through OpenLogForAppend and returns a writer
// that keeps it within the cap for as long as it stays open.
func OpenRotatingLog(path string) (*RotatingLog, error) {
	l := &RotatingLog{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *RotatingLog) open() error {
	f, err := OpenLogForAppend(l.path)
	if err != nil {
		return err
	}
	var size int64
	if fi, serr := f.Stat(); serr == nil {
		size = fi.Size()
	}
	l.f, l.size, l.limit = f, size, logRotateCap
	if size >= logRotateCap {
		l.limit = size + retryAfterFailedRotation
	}
	return nil
}

// Write appends p to the rotating log.
func (l *RotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, os.ErrClosed
	}
	if l.f != nil && l.size >= l.limit {
		_ = l.f.Close()
		l.f = nil
	}
	if l.f == nil {
		if err := l.open(); err != nil {
			return len(p), nil
		}
	}
	n, _ := l.f.Write(p)
	l.size += int64(n)
	return len(p), nil
}

// Close closes the rotating log.
func (l *RotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

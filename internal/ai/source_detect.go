package ai

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ErrUndetectableHarness is returned when no calling harness can be determined.
// Callers must surface it with an actionable --source hint rather than
// defaulting to another harness.
var ErrUndetectableHarness = errors.New("cannot determine the calling harness (no --source and no claude/opencode/codex/goose ancestor detected); pass --source claude-code|opencode|codex|goose")

// DetectSource reports the CLI harness that invoked the current process, or ""
// when it cannot be determined. Harnesses announce themselves in the
// environment (OPENCODE, CLAUDECODE, CLAUDE_CODE_ENTRYPOINT) when they spawn
// ghost; when those markers are absent — a manual `ghost supersede` run from a
// shell inside a harness session — Linux falls back to walking the ancestor
// process chain. Callers must treat "" as "undetectable" and fail with an
// actionable error: defaulting to some other harness would bill the wrong
// subscription and betray the user's routing choice.
func DetectSource() string {
	if s := detectSourceFromEnv(os.Getenv); s != "" {
		return s
	}
	// /proc is Linux-only; other platforms get the environment markers only.
	if runtime.GOOS != "linux" {
		return ""
	}
	return detectSourceFromProc("/proc", os.Getpid())
}

// detectSourceFromEnv maps harness environment markers to source tokens.
// lookup is os.Getenv in production; tests inject a fake so they stay
// independent of the test runner's own environment.
func detectSourceFromEnv(lookup func(string) string) string {
	if lookup("OPENCODE") != "" {
		return "opencode"
	}
	if lookup("CLAUDECODE") != "" || lookup("CLAUDE_CODE_ENTRYPOINT") != "" {
		return "claude-code"
	}
	return ""
}

// jsRuntimes are the process names a JS-packaged harness may appear as in
// /proc/<pid>/comm. For those, the harness name lives in the script argument
// instead: `node /usr/local/bin/opencode` reports comm "node" with the harness
// in argv[1]. Debian/Ubuntu ship the runtime as "nodejs".
var jsRuntimes = map[string]bool{"node": true, "nodejs": true, "bun": true, "deno": true}

// detectSourceFromProc walks the ancestor chain starting at startPID (the
// caller's own pid, whose comm is ghost and never matches) and returns the
// source token of the first harness process, or "". root is "/proc" in
// production and a fake tree in tests. The walk stops at pid 1 (init) or after
// maxAncestorHops, bounding a malformed or cyclic tree.
func detectSourceFromProc(root string, startPID int) string {
	const maxAncestorHops = 20
	pid := startPID
	for hops := 0; hops < maxAncestorHops && pid > 1; hops++ {
		if s := sourceForProcess(root, pid); s != "" {
			return s
		}
		ppid, err := parentPID(root, pid)
		if err != nil || ppid <= 0 || ppid == pid {
			return ""
		}
		pid = ppid
	}
	return ""
}

// sourceForProcess maps one /proc entry to a source token: the trimmed comm,
// or — when comm is a JS runtime — the basename of the script argument from
// cmdline, where a script-packaged harness's name actually appears.
func sourceForProcess(root string, pid int) string {
	dir := filepath.Join(root, strconv.Itoa(pid))
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(comm))
	if s := sourceFromProcessName(name); s != "" {
		return s
	}
	if !jsRuntimes[name] {
		return ""
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return ""
	}
	// argv[0] is the runtime itself (`node`); the script path follows. Scan
	// every argument so a runtime flag between them cannot hide the script.
	for _, arg := range strings.Split(string(cmdline), "\x00")[1:] {
		if s := sourceFromProcessName(filepath.Base(arg)); s != "" {
			return s
		}
	}
	return ""
}

// sourceFromProcessName maps a process comm or argv[0] basename to a source
// token, or "" when it is not a known harness.
func sourceFromProcessName(name string) string {
	switch name {
	case "claude", "claude-code":
		return "claude-code"
	case "opencode":
		return "opencode"
	case "codex":
		return "codex"
	case "goose":
		return "goose"
	}
	return ""
}

// parentPID reads ppid (field 4) from /proc/<pid>/stat. The comm field (field
// 2) may itself contain spaces and parentheses, so parse from the last ')' and
// take the second field after it (state is first, ppid second) rather than
// splitting the whole line.
func parentPID(root string, pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	line := string(data)
	closeIdx := strings.LastIndex(line, ")")
	if closeIdx < 0 {
		return 0, fmt.Errorf("malformed stat for pid %d: no comm terminator", pid)
	}
	fields := strings.Fields(line[closeIdx+1:]) // state, ppid, ...
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed stat for pid %d: missing ppid field", pid)
	}
	return strconv.Atoi(fields[1])
}

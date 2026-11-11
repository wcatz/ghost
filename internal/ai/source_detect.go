package ai

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

var ErrUndetectableHarness = errors.New("cannot determine the calling harness (no --source and no claude/opencode/codex/goose ancestor detected); pass --source claude-code|opencode|codex|goose")

func DetectSource() string {
	if s := detectSourceFromEnv(os.Getenv); s != "" {
		return s
	}

	switch runtime.GOOS {
	case "linux":
		return detectSourceFromProc("/proc", os.Getpid())
	case "darwin":
		return detectSourceFromPS(psCommand, os.Getpid())
	}
	return ""
}

func psCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

func detectSourceFromEnv(lookup func(string) string) string {
	if lookup("OPENCODE") != "" {
		return "opencode"
	}
	if lookup("CLAUDECODE") != "" || lookup("CLAUDE_CODE_ENTRYPOINT") != "" {
		return "claude-code"
	}
	return ""
}

var jsRuntimes = map[string]bool{"node": true, "nodejs": true, "bun": true, "deno": true}

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

	for _, arg := range strings.Split(string(cmdline), "\x00")[1:] {
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}
		return sourceFromScriptPath(arg)
	}
	return ""
}

func sourceFromScriptPath(path string) string {
	if s := sourceFromProcessName(filepath.Base(path)); s != "" {
		return s
	}
	for _, segment := range strings.Split(path, "/") {
		if s := sourceFromProcessName(strings.TrimSuffix(segment, filepath.Ext(segment))); s != "" {
			return s
		}
	}
	return ""
}

func sourceFromProcessName(name string) string {
	switch strings.TrimSuffix(name, ".exe") {
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

func detectSourceFromPS(run func(name string, args ...string) ([]byte, error), startPID int) string {
	const maxAncestorHops = 20
	pid := startPID
	for hops := 0; hops < maxAncestorHops && pid > 1; hops++ {
		ppidOut, err := run("ps", "-o", "ppid=", "-p", strconv.Itoa(pid))
		if err != nil {
			return ""
		}
		ppid, err := strconv.Atoi(strings.TrimSpace(string(ppidOut)))
		if err != nil {
			return ""
		}
		commOut, err := run("ps", "-o", "comm=", "-p", strconv.Itoa(pid))
		if err != nil {
			return ""
		}
		comm := strings.TrimSpace(string(commOut))
		if s := sourceFromProcessName(comm); s != "" {
			return s
		}
		if jsRuntimes[comm] {
			out, err := run("ps", "-o", "command=", "-p", strconv.Itoa(pid))
			if err == nil {

				for i, token := range strings.Fields(string(out)) {
					if i == 0 || strings.HasPrefix(token, "-") {
						continue
					}
					if s := sourceFromScriptPath(token); s != "" {
						return s
					}
					break
				}
			}
		}
		if ppid <= 1 || ppid == pid {
			return ""
		}
		pid = ppid
	}
	return ""
}

const HostSessionEnv = "CLAUDE_CODE_SESSION_ID"

func IsHostSession(root string, pid int) bool {
	if pid == 0 {
		pid = os.Getpid()
	}
	ppid, err := parentPID(root, pid)
	if err != nil || ppid <= 1 {
		return false
	}

	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(ppid), "environ"))
	if err != nil {
		return false
	}

	return !bytes.Contains(data, []byte(HostSessionEnv+"="))
}

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
	fields := strings.Fields(line[closeIdx+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed stat for pid %d: missing ppid field", pid)
	}
	return strconv.Atoi(fields[1])
}

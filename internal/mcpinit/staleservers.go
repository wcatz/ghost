package mcpinit

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// StaleGhostServer is one running `ghost mcp` process that is not running the
// binary this command is.
type StaleGhostServer struct {
	PID        int
	ParentPID  int
	ParentName string
	ExePath    string
	ExeDeleted bool
}

// Reason returns why the server is considered stale.
func (s StaleGhostServer) Reason(installed string) string {
	if s.ExeDeleted {
		return fmt.Sprintf("its executable %s has been deleted (replaced by a newer ghost)", strings.TrimSuffix(s.ExePath, deletedSuffix))
	}
	return fmt.Sprintf("it is running %s, not this ghost at %s", s.ExePath, installed)
}

const deletedSuffix = " (deleted)"

// ReportStaleServers writes a line per stale server to w, and returns how many it
// found. It reads the real proc tree.
func ReportStaleServers(w io.Writer, installed string) int {
	return ReportStaleServersTo(w, procRoot, installed)
}

// ReportStaleServersTo is ReportStaleServers with the proc tree injected, so a
// test can read a fixture instead of the machine's live processes.
func ReportStaleServersTo(w io.Writer, root, installed string) int {
	servers, checked := StaleGhostServers(root, installed)
	if !checked || len(servers) == 0 {
		return 0
	}
	_, _ = fmt.Fprintf(w, "%d running ghost mcp %s a different ghost binary than this one:\n",
		len(servers), plural(len(servers), "server runs", "servers run"))
	for _, s := range servers {
		_, _ = fmt.Fprintf(w, "  pid %d — %s; started by %s (pid %d)\n",
			s.PID, s.Reason(installed), s.ParentName, s.ParentPID)
	}
	_, _ = fmt.Fprintln(w, "  → restart those clients so they start this ghost; until then their writes may be refused")
	return len(servers)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

var procRoot = "/proc"

func isGhostMCPProcess(root string, pid int) (bool, string, error) {
	dir := filepath.Join(root, strconv.Itoa(pid))
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil {
		return false, "", err
	}
	if strings.TrimSpace(string(comm)) != "ghost" {
		return false, "", nil
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return false, "", err
	}
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	if len(args) != 2 || args[1] != "mcp" {
		return false, "", nil
	}
	return true, strings.TrimSpace(string(comm)), nil
}

func exeTarget(root string, pid int) (string, bool, error) {
	target, err := os.Readlink(filepath.Join(root, strconv.Itoa(pid), "exe"))
	if err != nil {
		return "", false, err
	}
	return target, strings.HasSuffix(target, deletedSuffix), nil
}

func procPPID(root string, pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	line := string(data)
	idx := strings.LastIndexByte(line, ')')
	if idx < 0 || idx+2 >= len(line) {
		return 0, fmt.Errorf("malformed stat for pid %d: no comm terminator", pid)
	}
	fields := strings.Fields(line[idx+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed stat for pid %d: missing ppid field", pid)
	}
	return strconv.Atoi(fields[1])
}

func procComm(root string, pid int) string {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func sameFile(a, b string) (same, known bool) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, false
	}
	return os.SameFile(ai, bi), true
}

func listPIDs(root string) ([]int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}

func staleOnProc(root, installed string) ([]StaleGhostServer, bool) {
	if installed == "" {
		return nil, false
	}
	pids, err := listPIDs(root)
	if err != nil {
		return nil, false
	}
	var stale []StaleGhostServer
	for _, pid := range pids {
		isServer, _, err := isGhostMCPProcess(root, pid)
		if err != nil || !isServer {
			continue
		}
		exe, deleted, err := exeTarget(root, pid)
		if err != nil {
			continue
		}
		if !deleted {
			same, known := sameFile(installed, exe)
			if !known || same {
				continue
			}
		}
		ppid, err := procPPID(root, pid)
		if err != nil {
			continue
		}
		stale = append(stale, StaleGhostServer{
			PID:        pid,
			ParentPID:  ppid,
			ParentName: procComm(root, ppid),
			ExePath:    exe,
			ExeDeleted: deleted,
		})
	}
	return stale, true
}

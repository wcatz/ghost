package mcpinit

// The operator-facing half of #746: which running `ghost mcp` servers are
// executing a binary other than the one this command is.
//
// Note what that is NOT. It is not a check for "the installed ghost is out of
// date", because `installed` is simply the path of the binary running this
// command and this code never discovers an installation. A `go run` build, a
// `make build` output and a ~/bin copy are all ordinary ways to invoke the
// command, and a developer running one of them has current clients. So the two
// kinds of finding are worded from the evidence each one carries: a DELETED
// executable is a fact about the world and is reported as replaced, while a
// plain mismatch says only "a different ghost binary than this one".
//
// A running server holds the inode of the executable it started from. When
// `ghost upgrade` replaces that file, the old process keeps running on the old
// code and nothing says so: the pid files this package writes are for the
// lifecycle and obsidian workers, not for MCP servers, and no client reports
// its server's version. On the install the issue was found on, three v0.33.0
// servers ran for ~40 hours behind a binary that had since been deleted, and
// every write they took in that window skipped the history and evidence records
// a newer schema requires.
//
// The listing is BEST-EFFORT and strictly READ-ONLY. It never signals, kills or
// otherwise touches a process. The remedy is a client restart, only the
// operator knows which client is safe to interrupt, and a diagnostic that can
// end a process is a different tool with a different blast radius — so this one
// can only ever say what it found.
//
// Detection is by EXECUTABLE IDENTITY, not by a version string. A version would
// be a better answer, and it is not available: the running process's version is
// a string inside a binary this code must not execute, and `ghost --version` on
// another process's behalf is both a subprocess and a version that a
// half-replaced file may not even report. The installed path and the deletion
// marker are facts the kernel already keeps for us.

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
	// PID is the server's process id.
	PID int
	// ParentPID is its parent — the MCP client, or a proxy in front of one.
	ParentPID int
	// ParentName is that parent's comm, which is the actionable half: it turns
	// a pid into "the Claude Code session to restart".
	ParentName string
	// ExePath is the executable the process is running, as the kernel reports
	// it. It carries a " (deleted)" suffix when the file is gone, which is why
	// ExeDeleted exists as a separate field rather than being parsed back out
	// of this string by every caller.
	ExePath string
	// ExeDeleted says the executable has been unlinked — the state
	// `ghost upgrade` leaves behind.
	ExeDeleted bool
}

// Reason is one clause saying why this process is listed, for a report line. It
// is a method rather than a stored string so the wording lives beside the facts
// it describes, and so a caller cannot render a process without saying why.
//
// The two cases are worded from the evidence each one actually carries, which
// is the whole reason they are different sentences. ExeDeleted is a FACT about
// the world: the kernel says this file is gone, and something replaced it. A
// plain mismatch is not: `installed` is the path of the binary running THIS
// command, so the only thing a mismatch establishes is that the server is not
// that file. It may be a second install, a `go run` build, or a copy in ~/bin —
// and calling it "the installed ghost" or saying it "is no longer installed"
// would send an operator to reinstall a binary that is already fine.
func (s StaleGhostServer) Reason(installed string) string {
	if s.ExeDeleted {
		return fmt.Sprintf("its executable %s has been deleted (replaced by a newer ghost)", strings.TrimSuffix(s.ExePath, deletedSuffix))
	}
	return fmt.Sprintf("it is running %s, not this ghost at %s", s.ExePath, installed)
}

// deletedSuffix is what the kernel appends to a /proc/<pid>/exe readlink whose
// target has been unlinked. The process is still executing that inode, so the
// link resolves for reading but no longer names a file on disk.
const deletedSuffix = " (deleted)"

// ReportStaleServers writes a line per stale server to w, and returns how many it
// found. It reads the real proc tree.
func ReportStaleServers(w io.Writer, installed string) int {
	return ReportStaleServersTo(w, procRoot, installed)
}

// ReportStaleServersTo is ReportStaleServers with the proc tree injected, so a
// test can read a fixture instead of the machine's live processes.
//
// It says NOTHING on a healthy machine and on a platform that cannot answer.
// Silence is the contract: a permanent section an operator learns to skip is
// worse than no section, and on a platform with no /proc the honest answer is
// "not checked" rather than "none found" — those are different facts, and only
// one of them is true.
func ReportStaleServersTo(w io.Writer, root, installed string) int {
	servers, checked := StaleGhostServers(root, installed)
	if !checked || len(servers) == 0 {
		return 0
	}
	// Write errors are dropped, deliberately, and the `_, _` is the record of
	// that decision rather than a way to silence the linter. This is a
	// best-effort report to a stdout: a full disk or a closed pipe costs the
	// operator this one screen, and the caller is a status command whose whole
	// job is to print, not to return an error. Returning one would make a
	// diagnostic able to fail the command that carries every other diagnostic
	// — the same blast-radius argument as not signalling a process. It matches
	// how the rest of this package writes its reports (codex.go).
	//
	// The header says what was COMPARED, not what is installed. `installed` is
	// this process's own binary — a `go run` build, a `make build` output or a
	// ~/bin copy are all legitimate ways to run this command — so "is no longer
	// installed" is a claim about an installation this code never discovered.
	// "a different ghost binary than this one" is the fact, and it is the fact
	// the reader can act on.
	_, _ = fmt.Fprintf(w, "%d running ghost mcp %s a different ghost binary than this one:\n",
		len(servers), plural(len(servers), "server runs", "servers run"))
	for _, s := range servers {
		_, _ = fmt.Fprintf(w, "  pid %d — %s; started by %s (pid %d)\n",
			s.PID, s.Reason(installed), s.ParentName, s.ParentPID)
	}
	// The remedy, once, and named as the operator's action rather than the
	// program's: only they know which session is safe to interrupt.
	//
	// The consequence is HEDGED on purpose. A server on a replaced binary
	// refuses its writes only once a newer Ghost has migrated the store past it,
	// and this code never opens the store — so "their writes may be refused"
	// is what the evidence supports, and ghost_health is where the actual
	// refusal is reported. Asserting it here would be a claim about a database
	// state this scan did not read.
	_, _ = fmt.Fprintln(w, "  → restart those clients so they start this ghost; until then their writes may be refused")
	return len(servers)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// procRoot is the tree the scan reads. It is a variable so a test can point it
// at a fixture: a test that walked the real /proc would read other users'
// processes and assert against whatever the machine happened to be running.
var procRoot = "/proc"

// isGhostMCPProcess reports whether a /proc entry is a `ghost mcp` process.
//
// It matches on BOTH the executable's base name and the `mcp` subcommand, not
// on either alone. A bare comm match would list the CLI's own lifecycle worker
// (`ghost lifecycle`) as a server to restart, which is a different process with
// a different lifecycle; and a bare subcommand match would list any process
// whose arguments happen to include "mcp".
//
// The subcommand must be the LAST argument, which is stricter than "args[1] is
// mcp" and is the shape cmd/ghost/main.go actually dispatches on: bare `ghost mcp`
// runs the server, `ghost mcp init` and `ghost mcp status` are short-lived commands
// that return before runMCP, and `ghost mcp <anything>` is a usage error. Matching
// the prefix listed all three, and the reachable case is this feature's own — an
// operator running `~/bin/ghost mcp status` while their clients run
// `/usr/local/bin/ghost mcp` would have their own status command reported under
// "restart those clients". Requiring no further argument is also exactly the argv
// the shipped installers spawn: `command`/`args` are `["…ghost", "mcp"]` for
// claude-code, opencode, codex and goose, so this matches servers and nothing else.
func isGhostMCPProcess(root string, pid int) (bool, string, error) {
	dir := filepath.Join(root, strconv.Itoa(pid))
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil {
		return false, "", err
	}
	if strings.TrimSpace(string(comm)) != "ghost" {
		return false, "", nil
	}
	// argv rather than comm: the subcommand is an argument, and the comm of a
	// replaced binary still reads "ghost" long after its file is gone — which
	// is the case this whole scan exists to find.
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return false, "", err
	}
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	// Exactly two arguments: the binary and the bare `mcp`. Not `len(args) >= 2
	// && args[1] == "mcp"`, which also matched `ghost mcp status`.
	if len(args) != 2 || args[1] != "mcp" {
		return false, "", nil
	}
	return true, strings.TrimSpace(string(comm)), nil
}

// exeTarget reads /proc/<pid>/exe and reports the path plus whether the file has
// been unlinked.
//
// It reads the LINK rather than stat-ing the path: after an unlink the path no
// longer resolves, so a stat would report "missing" for a file the process is
// demonstrably still executing, and would equally report "missing" for a path
// this user simply may not traverse. The kernel's own " (deleted)" marker is the
// only signal that distinguishes the two.
func exeTarget(root string, pid int) (string, bool, error) {
	target, err := os.Readlink(filepath.Join(root, strconv.Itoa(pid), "exe"))
	if err != nil {
		return "", false, err
	}
	return target, strings.HasSuffix(target, deletedSuffix), nil
}

// procPPID reads a process's parent pid from /proc/<pid>/stat field 4.
//
// The comm field (field 2) may itself contain spaces and parentheses — a
// process is free to be called ") S 1 (" — so the line is split from the LAST
// ')' and the second field after it taken, which is ppid. Splitting the whole
// line is the bug this avoids.
// procPPID reads field (4) out of /proc/<pid>/stat.
//
// The comm field is the only parenthesised thing on the line, and it is written
// RAW — fs/proc/array.c's do_task_stat calls `proc_task_name(m, task, false)`,
// which is a `%.64s` of a 15-byte task comm. So the closing paren of the comm
// field is the LAST ')' in the line, and scanning to the LAST one is what makes
// a comm containing spaces or parentheses safe. Scanning to the FIRST one is the
// bug, and it is the bug an adversarial review claimed this code had.
//
// Two claims were checked against the kernel source rather than believed:
//
//   - Fields 48-51 (arg_start, arg_end, env_start, env_end) are NOT
//     parenthesised. do_task_stat writes them `seq_put_decimal_ull(m, " ",
//     mm->arg_start)`, bare. An older proc(5) said otherwise; the man page
//     dropped the claim (man-pages 6.19) because it was never true.
//   - A comm with ')' and '(' in it is handled, and measurably so: a live process
//     whose comm was set to "weird ) ( name" still yields the right ppid from the
//     last ')'.
//
// TestTheStatParserTakesTheParentPIDFromTheKernelsOwnFieldOrder pins both, so
// this question is settled by a test rather than by the next round of the same
// argument.
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

// procComm reads a process's comm, or "" when it cannot be read. A parent this
// scan cannot name is still worth reporting — the pid is actionable on its own.
func procComm(root string, pid int) string {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// sameFile reports whether two paths name the same file, following symlinks so
// an installed binary reached through a link and the one a process is executing
// compare equal.
//
// The error is NOT a formality, and returning a bare bool here was a real bug
// caught in review: a failure to resolve is not inequality, and a two-valued
// return cannot say so. The reachable case is a `ghost mcp` server owned by
// another user — a system-wide install under a root-owned /opt at 0700 — whose
// /proc/<pid>/exe this user cannot stat. The old code returned false, the one
// caller read false as "a different file", and the report told the operator to
// restart a perfectly healthy client. A three-valued answer is the minimum that
// carries "we do not know" through to the decision, and the caller skips on it
// rather than guessing.
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

// listPIDs returns the numeric entries under root, ascending. A non-numeric
// entry is skipped: /proc carries per-process subdirectories and the scan only
// wants the pids.
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

// staleOnProc is the Linux implementation, and the only one that can answer.
//
// The boolean is `checked`, and it is false for a tree that could not be read at
// all, OR for a caller that named no installed binary to compare against. That
// second case is the one worth being explicit about: with `installed == ""` every
// live `ghost mcp` process looks like a mismatch, and the honest answer is not
// "all of your servers are stale" but "this scan cannot tell you". Reporting the
// whole fleet is the failure mode this scan exists to prevent, so the guard
// belongs HERE rather than only at the one call site that happens to check — a
// second caller passing "" should not be able to reach the wrong answer.
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
			// Unreadable or not a server: skipped, never guessed at.
			continue
		}
		exe, deleted, err := exeTarget(root, pid)
		if err != nil {
			continue
		}
		// Two independent facts, either of which means the process is not
		// running this binary: the file is gone (upgrade replaced it), or it is
		// a different file (a second install, or a build from source).
		//
		// The third case — the comparison could not be made — is NOT one of
		// them, and reporting it as either would be a guess dressed as a
		// finding. A server whose executable this user cannot stat is skipped,
		// which leaves it unreported rather than wrongly reported.
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

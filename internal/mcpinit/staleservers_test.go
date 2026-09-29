//go:build linux

package mcpinit

// #746, the operator-facing half: a running `ghost mcp` whose executable has
// been replaced or deleted is invisible until a save is refused. The three
// servers the issue found had been running for ~40 hours behind a binary
// `ghost upgrade` had already deleted.
//
// The listing is best-effort and READ-ONLY. It never signals, kills, or
// otherwise touches a process: the remedy is a client restart, and only the
// operator knows which client to leave alone. A diagnostic that can end a
// process is a different tool with a different blast radius.
//
// Every case runs against a FAKE /proc tree through an injectable root. A test
// that walked the real one would read other users' processes, be
// machine-dependent, and — worst — assert against whatever the CI runner
// happened to be running.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// procEntry writes one fake /proc/<pid> directory. comm is what `ps` shows,
// cmdline is NUL-separated as the kernel writes it, and stat carries the ppid
// field the walk needs.
type procEntry struct {
	pid     int
	comm    string
	cmdline []string
	// exeLink is written as a SYMLINK when set, standing in for
	// /proc/<pid>/exe. Left empty, no link is created, which is what a process
	// this user may not inspect looks like.
	exeLink string
	// exeDeleted writes the target with the kernel's " (deleted)" suffix,
	// which is how Linux reports an unlinked-but-running executable.
	exeDeleted bool
	// statOverride replaces the generated stat line, for malformed cases.
	statOverride string
	ppid         int
}

func writeFakeProc(t *testing.T, root string, entries ...procEntry) {
	t.Helper()
	for _, e := range entries {
		dir := filepath.Join(root, itoa(e.pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if e.comm != "" {
			if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(e.comm+"\n"), 0o644); err != nil { //nolint:gosec
				t.Fatalf("write comm: %v", err)
			}
		}
		if len(e.cmdline) > 0 {
			joined := strings.Join(e.cmdline, "\x00") + "\x00"
			if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(joined), 0o644); err != nil { //nolint:gosec
				t.Fatalf("write cmdline: %v", err)
			}
		}
		stat := e.statOverride
		if stat == "" {
			stat = kernelStatLine(e.pid, e.comm, e.ppid)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil { //nolint:gosec
			t.Fatalf("write stat: %v", err)
		}
		if e.exeLink != "" {
			target := e.exeLink
			if e.exeDeleted {
				target += " (deleted)"
			}
			link := filepath.Join(dir, "exe")
			// A second entry for the same pid REPLACES the first: that is how a
			// test models the same pid before and after `ghost upgrade`, which
			// is the only transition this scan cares about.
			_ = os.Remove(link)
			if err := os.Symlink(target, link); err != nil {
				t.Fatalf("symlink exe: %v", err)
			}
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// kernelStatLine emits /proc/<pid>/stat in the KERNEL's shape: all 52 fields, in
// the order fs/proc/array.c's do_task_stat writes them.
//
// The earlier version of this fixture emitted sixteen fields and called that
// "the kernel's layout", which made it useless for the one question the stat
// file exists to answer here — how do you find the end of the comm field? A
// truncated line and a full one differ in exactly the places a parser gets
// wrong, so a fixture that shortens the line is a fixture that cannot catch the
// bug it exists to catch. Review raised this, and it was right about the fixture
// even though it drew the wrong conclusion from it (see
// TestTheStatParserTakesTheParentPIDFromTheKernelsOwnFieldOrder).
//
// Two properties of the real line are load-bearing and are reproduced here
// rather than trimmed:
//
//   - comm is the ONLY parenthesised field. do_task_stat writes fields 48-51
//     (arg_start, arg_end, env_start, env_end) as plain numbers —
//     `seq_put_decimal_ull(m, " ", mm->arg_start)` — with no parentheses. An
//     older proc(5) claimed they were parenthesised; the man page dropped the
//     claim (man-pages 6.19) because it was never true.
//   - comm is written RAW: `proc_task_name(m, task, false)` prints `%.64s` of the
//     task comm, which is 15 bytes and may contain SPACES and PARENTHESES. So
//     the closing paren of the line's only parenthesised field is the LAST ')'
//     in the line, and finding the first one is the bug.
func kernelStatLine(pid int, comm string, ppid int) string {
	f := make([]string, 0, 52)
	// (1) pid
	f = append(f, itoa(pid))
	// (2) comm, raw, in parentheses — nothing after it has a ')'
	f = append(f, "("+comm+")")
	// (3) state
	f = append(f, "S")
	// (4) ppid  <- the field this scan reads
	f = append(f, itoa(ppid))
	// (5)-(8) pgrp, session, tty_nr, tpgid
	f = append(f, "1", "1", "0", "-1")
	// (9)-(20) flags, minflt, cminflt, majflt, cmajflt, utime, stime, cutime,
	// cstime, priority, nice, num_threads
	f = append(f, "4194560", "100", "0", "0", "0", "1", "2", "3", "4", "5", "6", "7")
	// (21) itrealvalue, (22) starttime, (23) vsize, (24) rss, (25) rsslim
	f = append(f, "0", "9876543", "1048576", "4096", "18446744073709551615")
	// (26)-(36) startcode .. nswap
	f = append(f, "4194304", "4198400", "140737488347136", "0", "0", "0", "0", "0", "0", "0", "0")
	// (37) cnswap
	f = append(f, "0")
	// (38)-(41) exit_signal, processor, rt_priority, policy
	f = append(f, "17", "5", "0", "0")
	// (42)-(44) delayacct_blkio_ticks, guest_time, cguest_time
	f = append(f, "0", "0", "0")
	// (45)-(47) start_data, end_data, start_brk
	f = append(f, "94678405376", "94678442752", "94678418432")
	// (48)-(51) arg_start, arg_end, env_start, env_end — plain numbers, NOT
	// parenthesised. This is the tail a parser that assumes a trailing ")"
	// group would trip over, and the reason they are written out in full.
	f = append(f, "140735000000", "140735000123", "140735000456", "140735000789")
	// (52) exit_code
	f = append(f, "0")
	return strings.Join(f, " ") + "\n"
}

// The parent-pid field is found by scanning to the LAST ')' in the stat line, and
// the cases below are why that is the right end to scan from.
//
// An adversarial review claimed this was a blocker: that "the last ')' in a real
// /proc/<pid>/stat is NOT the end of comm", citing fields 48-51 being parenthesised.
// They are not. fs/proc/array.c's do_task_stat writes arg_start, arg_end,
// env_start and env_end as plain numbers — `seq_put_decimal_ull(m, " ",
// mm->arg_start)` — and the comm is the only parenthesised field on the line. An
// older proc(5) said otherwise and the man page dropped the claim (man-pages
// 6.19); it was never true. Verified here against the kernel source and against
// a real /proc on this machine.
//
// The opposite was true, and it is what the last-')' scan is FOR: comm is
// `%.64s` of a 15-byte field written raw, so it can contain spaces AND
// parentheses, and taking the FIRST ')' is the bug. Measured with a live
// process whose comm was set to "weird ) ( name": the last ')' still yields the
// correct ppid, and the first one does not.
//
// This test exists so the next person to raise it is answered by a test rather
// than by another round of the same argument, and so a future kernel that DID
// parenthesise the tail would be caught here rather than in production.
func TestTheStatParserTakesTheParentPIDFromTheKernelsOwnFieldOrder(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "ghost")
	if err := os.WriteFile(installed, []byte("current build"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write installed binary: %v", err)
	}
	// A comm that is hostile to a first-')' parse: spaces, and a ')' and a '('
	// in the wrong places. Measured against the kernel, this is close to the
	// worst case TASK_COMM_LEN allows.
	const nastyComm = "we)ird (x"
	root := filepath.Join(dir, "proc")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir proc root: %v", err)
	}
	other := filepath.Join(dir, "old-ghost")
	if err := os.WriteFile(other, []byte("older build"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write the other binary: %v", err)
	}
	// The nasty comm is on the PARENT, and it belongs there rather than on the
	// server: a server this scan reports has comm "ghost" (that is the first
	// half of how it is recognised), so a hostile comm can never appear in the
	// line the scan parses for a server's OWN parent. The parent is not filtered
	// by comm at all — it is whatever client or proxy started the server — and
	// the parent is exactly where the actionable name comes from, so this is the
	// parse that has to be right.
	writeFakeProc(t, root,
		procEntry{pid: 500, comm: "ghost", cmdline: []string{other, "mcp"}, ppid: 4242, exeLink: other},
		procEntry{pid: 4242, comm: nastyComm, cmdline: []string{"node", "client.js"}, ppid: 1, exeLink: "/usr/bin/node"},
	)

	// The load-bearing property of the fixture, asserted rather than assumed:
	// NOTHING after field 47 contains a ')'. The comm's own closing paren is
	// therefore the last one on the line, which is what makes scanning to the
	// last ')' correct and scanning to the first one a bug.
	line := kernelStatLine(500, "ghost", 4242)
	if got := strings.Count(line, ")"); got != 1 {
		t.Fatalf("a comm without parentheses yields %d ')', want exactly 1 — the kernel parenthesises "+
			"only comm, so a count other than 1 means this fixture is not the kernel's shape and it "+
			"cannot answer the question it exists to answer. Line: %s", got, line)
	}
	if i := strings.Index(line, "("); i < 0 {
		t.Fatal("fixture has no opening paren for comm")
	}
	// And a comm that contains ')' has exactly one MORE than the delimiters,
	// never a trailing group.
	if got := strings.Count(kernelStatLine(1, nastyComm, 1), ")"); got != 2 {
		t.Errorf("a comm containing ')' yields %d ')' on the line, want 2 (its own, plus the delimiter)", got)
	}
	found, _ := StaleGhostServers(root, installed)
	if len(found) != 1 {
		t.Fatalf("found %+v, want the one server on a different binary", found)
	}
	if found[0].PID != 500 || found[0].ParentPID != 4242 {
		t.Errorf("got pid %d parent %d, want 500/4242 — the ppid was read from the wrong field, so the "+
			"report would name a pid rather than the client to restart", found[0].PID, found[0].ParentPID)
	}
	if found[0].ParentName != nastyComm {
		t.Errorf("ParentName = %q, want %q — the parent's comm is the actionable half of the line",
			found[0].ParentName, nastyComm)
	}

	// The same parse, directly, on a comm that is hostile to a first-')' scan.
	if got, err := procPPID(root, 4242); err != nil {
		t.Errorf("procPPID on a comm containing ')' and ' ': %v", err)
	} else if got != 1 {
		t.Errorf("procPPID = %d, want 1 — the scan took the first ')' and read a field out of the middle "+
			"of the comm", got)
	}

	// The fixture is the kernel's shape or it answers nothing. This compares the
	// field count against a REAL /proc/<pid>/stat on whatever machine is running
	// the test, which is the only comparison that cannot go stale: if a kernel
	// adds or drops a field, this fails here instead of the whole file's coverage
	// quietly becoming meaningless. An earlier version of this fixture emitted 16
	// fields and called it "the kernel's layout", which is precisely how a parser
	// bug in the tail of the line would be invisible to the suite.
	real, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatalf("read this process's own stat: %v", err)
	}
	// The line under test is the one writeFakeProc actually WROTE, read back off
	// disk. Comparing kernelStatLine's return value would not test the fixture
	// — it would test the helper while the fixture kept emitting something else,
	// which is exactly the arrangement that let the 16-field line through the
	// first time.
	written, err := os.ReadFile(filepath.Join(root, "500", "stat"))
	if err != nil {
		t.Fatalf("read the fixture's stat: %v", err)
	}
	// Both sides counted from the '(' that opens comm, so the pid is excluded
	// from both and the comparison is like-for-like.
	countFromComm := func(s string) int {
		i := strings.IndexByte(s, '(')
		if i < 0 {
			t.Fatalf("no '(' in %q", s)
		}
		return len(strings.Fields(s[i:]))
	}
	if got, want := countFromComm(string(written)), countFromComm(string(real)); got != want {
		t.Errorf("the fixture wrote %d fields after comm, this kernel writes %d — every assertion in "+
			"this file depends on the fixture being the kernel's shape. Line: %s", got, want, written)
	}
}

// Only the BARE `ghost mcp` is the server. `ghost mcp init` and `ghost mcp
// status` are short-lived CLI commands that main.go dispatches before it ever
// reaches runMCP, and `ghost mcp <anything-else>` is a usage error — so matching
// args[1] == "mcp" alone lists two non-servers.
//
// The reachable case is the two-installs case this feature exists for: an
// operator running `~/bin/ghost mcp status` while their clients run
// `/usr/local/bin/ghost mcp` would have their own status command listed under
// "restart those clients", with the remedy aimed at it.
func TestOnlyTheBareGhostMCPInvocationIsAServer(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "ghost")
	if err := os.WriteFile(installed, []byte("current build"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write installed binary: %v", err)
	}
	other := filepath.Join(dir, "old-ghost")
	if err := os.WriteFile(other, []byte("older build"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write the other binary: %v", err)
	}
	root := filepath.Join(dir, "proc")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir proc root: %v", err)
	}
	writeFakeProc(t, root,
		// The one that IS a server, and is running a different binary.
		procEntry{pid: 600, comm: "ghost", cmdline: []string{other, "mcp"}, ppid: 42, exeLink: other},
		// A `ghost mcp status` from the same other binary. Short-lived, exits
		// immediately, and restarting "the client" behind it is meaningless.
		procEntry{pid: 601, comm: "ghost", cmdline: []string{other, "mcp", "status"}, ppid: 43, exeLink: other},
		// A `ghost mcp init` likewise.
		procEntry{pid: 602, comm: "ghost", cmdline: []string{other, "mcp", "init"}, ppid: 43, exeLink: other},
		// And one that is neither, which main.go rejects as a usage error.
		procEntry{pid: 603, comm: "ghost", cmdline: []string{other, "mcp", "nonsense"}, ppid: 43, exeLink: other},
		procEntry{pid: 42, comm: "claude", cmdline: []string{"claude"}, ppid: 1, exeLink: "/usr/bin/node"},
		procEntry{pid: 43, comm: "bash", cmdline: []string{"bash"}, ppid: 1, exeLink: "/bin/bash"},
	)

	found, _ := StaleGhostServers(root, installed)
	if len(found) != 1 || found[0].PID != 600 {
		t.Fatalf("found %+v, want only pid 600 (the bare `ghost mcp` server) — `ghost mcp status`, "+
			"`ghost mcp init` and a usage error are not servers, and telling the operator to restart "+
			"them is advice with nothing behind it", found)
	}
}

// A malformed stat line is skipped rather than guessed at, which is what keeps a
// truncated read from producing a plausible wrong parent.
func TestTheStatParserRefusesALineItCannotRead(t *testing.T) {
	root := t.TempDir()
	// Each case gets a REAL pid directory, and the assertion reads that pid. The
	// first version of this test wrote the stat files into directories named
	// after the case and then asked for pid 0, so every case "passed" on a
	// file-not-found error rather than on the malformed line — which is the same
	// class of mistake as a green test standing in for a mechanism it never
	// exercised.
	pid := 0
	for _, c := range []struct{ name, line string }{
		{"no closing paren", "1234 (ghost S 1"},
		{"parens but nothing after", "1234 (ghost)"},
		{"no fields after comm", "1234 (ghost) \n"},
		{"ppid is not a number", "1234 (ghost) S parent 1 0\n"},
		{"empty file", ""},
	} {
		pid++
		dir := filepath.Join(root, itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(c.line), 0o644); err != nil { //nolint:gosec
			t.Fatalf("write stat: %v", err)
		}
		if got, err := procPPID(root, pid); err == nil {
			t.Errorf("%s: procPPID = %d, want an error — a parent read out of a line that cannot be "+
				"parsed would be a guess wearing a pid's clothes", c.name, got)
		}
		// And the premise: the file this case is about IS there, so the error
		// above came from the line and not from a missing directory.
		if _, err := os.Stat(filepath.Join(dir, "stat")); err != nil {
			t.Fatalf("%s: the fixture did not write the stat file it is testing: %v", c.name, err)
		}
	}
}

// A server whose executable has been unlinked is the case the issue is about:
// `ghost upgrade` replaces the file, the old inode stays alive as long as the
// process runs, and nothing anywhere says so.
func TestStaleServersFindsARunningServerWhoseExecutableWasDeleted(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root,
		procEntry{pid: 100, comm: "ghost", cmdline: []string{"/usr/local/bin/ghost", "mcp"}, ppid: 42, exeLink: "/usr/local/bin/ghost", exeDeleted: true},
		procEntry{pid: 42, comm: "claude", cmdline: []string{"claude", "--dangerously-skip"}, ppid: 1, exeLink: "/usr/bin/node"},
		procEntry{pid: 1, comm: "init", cmdline: []string{"init"}, ppid: 0, exeLink: "/sbin/init"},
	)

	found, checked := StaleGhostServers(root, "/usr/local/bin/ghost")
	if !checked {
		t.Fatal("the scan reported itself unchecked on a tree it could read")
	}
	if len(found) != 1 {
		t.Fatalf("found %d stale servers, want 1: %+v", len(found), found)
	}
	got := found[0]
	if got.PID != 100 {
		t.Errorf("PID = %d, want 100", got.PID)
	}
	// The parent is the actionable half: it names WHICH client to restart, and
	// without it an operator has three pids and no idea what any of them is.
	if got.ParentPID != 42 {
		t.Errorf("ParentPID = %d, want 42", got.ParentPID)
	}
	if got.ParentName != "claude" {
		t.Errorf("ParentName = %q, want %q — the parent is what tells the operator which client to restart", got.ParentName, "claude")
	}
	if !got.ExeDeleted {
		t.Error("ExeDeleted = false, want true for a process whose executable was unlinked")
	}
}

// A current server is not reported. A listing that names every running server
// trains the operator to skip it, which is the one behaviour that would make
// this feature useless in the incident it exists for.
//
// BOTH binaries are REAL files in the fixture, and the second one has to be for
// a reason the earlier version of this fixture got wrong. The comparison is
// os.SameFile, and it is now three-valued: a path that cannot be stat'd is
// UNKNOWN, which the scan skips (see
// TestAServerWhoseExecutableCannotBeReadIsNotCalledStale), not "different". A
// second binary spelled `/opt/old/ghost` with nothing behind it is therefore
// exactly the unreadable case, and this test failed once sameFile learned to say
// so — correctly, because the fixture was asserting a mismatch it had not
// actually created. Two real files, two real inodes, one genuine mismatch.
func TestStaleServersIgnoresCurrentAndUnrelatedProcesses(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "ghost")
	if err := os.WriteFile(installed, []byte("current build"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write installed binary: %v", err)
	}
	// A second install: a different, real file, so a real mismatch.
	otherInstall := filepath.Join(dir, "opt-old-ghost")
	if err := os.WriteFile(otherInstall, []byte("older build"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write the other binary: %v", err)
	}
	root := filepath.Join(dir, "proc")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir proc root: %v", err)
	}
	writeFakeProc(t, root,
		// The current server: same executable, not deleted.
		procEntry{pid: 100, comm: "ghost", cmdline: []string{installed, "mcp"}, ppid: 42, exeLink: installed},
		// A ghost that is NOT an mcp server — the CLI's own lifecycle worker.
		procEntry{pid: 101, comm: "ghost", cmdline: []string{installed, "lifecycle", "ghost"}, ppid: 1, exeLink: installed},
		// An unrelated program.
		procEntry{pid: 102, comm: "postgres", cmdline: []string{"postgres", "-D", "/var/lib/pg"}, ppid: 1, exeLink: "/usr/lib/postgres/bin/postgres"},
		// A second ghost mcp on a DIFFERENT file — a different install, so a
		// different (and still older) binary. The comparison is against this
		// command's own executable, not merely "is it deleted".
		procEntry{pid: 103, comm: "ghost", cmdline: []string{otherInstall, "mcp"}, ppid: 1, exeLink: otherInstall},
	)

	found, _ := StaleGhostServers(root, installed)
	if len(found) != 1 || found[0].PID != 103 {
		t.Fatalf("found %+v, want only pid 103 (a different ghost binary)", found)
	}
}

// A server this process cannot inspect is skipped rather than reported as
// stale. Guessing "deleted" from a permission error would put a line in front of
// an operator telling them to restart something that is fine.
func TestStaleServersSkipsProcessesItCannotRead(t *testing.T) {
	root := t.TempDir()
	// A pid directory with no comm, no cmdline and no exe link: what another
	// user's process looks like to this user.
	writeFakeProc(t, root, procEntry{pid: 700, statOverride: "700 (secret) S 1 1 0 0 -1 0 1 2 3 4 5 6 7 8\n"})

	found, _ := StaleGhostServers(root, "/usr/local/bin/ghost")
	if len(found) != 0 {
		t.Fatalf("found %+v, want nothing — an unreadable entry is unknown, not stale", found)
	}
}

// A caller that named no installed binary gets the honest "cannot tell you",
// not a list of every ghost mcp on the machine.
//
// This is the failure the scan exists to prevent, reached from the other end:
// with nothing to compare against, every live server mismatches by definition,
// and an operator shown "3 stale servers" when the answer is unknown will
// restart three working clients on a hunch. `ghost mcp status` checks for this
// before it calls, so this pins the guard in the FUNCTION rather than at the one
// call site that happens to remember it — a second caller passing "" should not
// be able to reach the wrong answer.
func TestStaleServersCannotAnswerWithoutAnInstalledBinary(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "ghost")
	if err := os.WriteFile(installed, []byte("current build"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write installed binary: %v", err)
	}
	root := filepath.Join(dir, "proc")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir proc root: %v", err)
	}
	writeFakeProc(t, root,
		// This one is genuinely the installed binary and genuinely current. It
		// would be reported as stale if the empty path were compared like any
		// other.
		procEntry{pid: 200, comm: "ghost", cmdline: []string{installed, "mcp"}, ppid: 42, exeLink: installed},
	)

	found, checked := StaleGhostServers(root, "")
	if checked {
		t.Error("checked = true with no installed binary to compare against — the scan can answer nothing here")
	}
	if len(found) != 0 {
		t.Fatalf("found %+v, want nothing — with nothing to compare against, every server looks like a mismatch, "+
			"and reporting the whole fleet is the exact error this scan must not make", found)
	}
}

// The scan is read-only by construction. It must not create, remove or signal
// anything under the tree it reads, because the remedy belongs to the operator
// and a diagnostic that could end a process is a different tool.
func TestStaleServersDoesNotTouchTheTree(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root,
		procEntry{pid: 100, comm: "ghost", cmdline: []string{"/usr/local/bin/ghost", "mcp"}, ppid: 42, exeLink: "/usr/local/bin/ghost", exeDeleted: true},
	)
	before := snapshotTree(t, root)

	StaleGhostServers(root, "/usr/local/bin/ghost") //nolint:errcheck

	if after := snapshotTree(t, root); !equalStrings(before, after) {
		t.Errorf("the scan changed the tree it was reading:\nbefore %v\nafter  %v", before, after)
	}
}

func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		kind := "f"
		if info.IsDir() {
			kind = "d"
		}
		out = append(out, kind+" "+rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk tree: %v", err)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

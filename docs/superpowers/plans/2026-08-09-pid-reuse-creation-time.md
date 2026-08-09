# PID-Reuse Detection via Process Creation Time — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `isAlive()` in `internal/mcpinit` detect PID reuse (a dead process's PID handed to an unrelated new process) across Linux, macOS, and Windows, closing item 3 of GitHub issue #252.

**Architecture:** Record each spawned process's OS-assigned creation time as an opaque string token alongside its PID in the `.pid` file (`"<pid>:<token>"`). At liveness-check time, re-derive the current holder's token and require an exact match; mismatch means the PID was recycled. A bare PID with no colon is the legacy format and falls back to today's PID-only check. Every new syscall path fails open (treats unreadable/unsupported as "can't tell, don't break the caller").

**Tech Stack:** Go 1.26, `golang.org/x/sys/unix` (darwin), `golang.org/x/sys/windows` (already a direct dependency), `/proc` on Linux. No new dependencies.

**Design spec:** `docs/superpowers/specs/2026-08-09-pid-reuse-creation-time-design.md`

---

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `internal/mcpinit/obsidiansync_linux.go` | Create | `processStartTime` for Linux (boot_id + `/proc/<pid>/stat` starttime) |
| `internal/mcpinit/obsidiansync_linux_test.go` | Create | Tests for the above |
| `internal/mcpinit/obsidiansync_darwin.go` | Create | `processStartTime` for macOS (`SysctlKinfoProc` wall-clock `Timeval`) |
| `internal/mcpinit/obsidiansync_darwin_test.go` | Create | Tests for the above |
| `internal/mcpinit/obsidiansync_unix_other.go` | Create | `processStartTime` fallback (`"", false`) for any other Unix |
| `internal/mcpinit/obsidiansync_windows.go` | Modify | Add `processStartTime`; change `isProcessAlive` to 3-arg token-aware form |
| `internal/mcpinit/obsidiansync_unix.go` | Modify | Change `isProcessAlive` to 3-arg token-aware form (shared by linux+darwin+other unix) |
| `internal/mcpinit/obsidiansync.go` | Modify | `isAlive` parses `"pid:token"`; `ensureObsidianSyncRunning` writes via token-aware `atomicWritePID` |
| `internal/mcpinit/stophook.go` | Modify | `atomicWritePID` gains token params; `claimPidFile`'s placeholder write gets a token too; both spawn functions pass tokens |
| `internal/mcpinit/obsidiansync_test.go` | Modify | New tests for token-format `isAlive` behavior |
| `internal/mcpinit/stophook_test.go` | Modify | New test for `claimPidFile`'s placeholder-token fix |

No new packages, no new files outside `internal/mcpinit`.

---

### Task 1: Linux `processStartTime`

**Files:**
- Create: `internal/mcpinit/obsidiansync_linux.go`
- Test: `internal/mcpinit/obsidiansync_linux_test.go`

- [ ] **Step 1: Write the failing test**

```go
//go:build linux

package mcpinit

import (
	"os"
	"testing"
)

func TestProcessStartTime_OwnPID(t *testing.T) {
	token, ok := processStartTime(os.Getpid())
	if !ok {
		t.Fatal("processStartTime(own pid) ok = false, want true")
	}
	if token == "" {
		t.Error("processStartTime(own pid) returned an empty token with ok=true")
	}
}

func TestProcessStartTime_Stable(t *testing.T) {
	a, okA := processStartTime(os.Getpid())
	b, okB := processStartTime(os.Getpid())
	if !okA || !okB {
		t.Fatal("processStartTime(own pid) failed on a repeat call")
	}
	if a != b {
		t.Errorf("processStartTime(own pid) not stable across calls: %q vs %q", a, b)
	}
}

func TestProcessStartTime_ImplausiblePID(t *testing.T) {
	if _, ok := processStartTime(999999999); ok {
		t.Error("processStartTime(implausible pid) ok = true, want false")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcpinit/... -run TestProcessStartTime -v`
Expected: FAIL with `undefined: processStartTime`

- [ ] **Step 3: Write minimal implementation**

```go
//go:build linux

package mcpinit

import (
	"os"
	"strconv"
	"strings"
)

// processStartTime returns an opaque token identifying pid's process
// creation instant, or ("", false) if it can't be determined (process
// gone, unreadable /proc entry, permission denied).
//
// The raw source — /proc/<pid>/stat field 22 ("starttime") — is clock
// ticks since boot, not wall-clock, so it is NOT by itself reboot-safe:
// .pid files persist in dataDir across reboots, and a freshly-started,
// unrelated process after a reboot can coincidentally have the same
// boot-relative tick count a stale file recorded before the reboot. The
// machine's boot ID (a fresh UUID generated at every boot) is prefixed so
// a pre-reboot token can never match a post-reboot one, regardless of tick
// coincidence.
func processStartTime(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	line := string(data)

	// comm (field 2) is wrapped in parens and can itself contain spaces and
	// parens, so split on the *last* ')' to find where it ends unambiguously.
	idx := strings.LastIndexByte(line, ')')
	if idx == -1 || idx+2 >= len(line) {
		return "", false
	}
	// Fields after the split start at field 3 (state); field 22 (starttime)
	// is therefore remainder-index 22-3 = 19.
	fields := strings.Fields(line[idx+2:])
	const starttimeIdx = 19
	if starttimeIdx >= len(fields) {
		return "", false
	}
	starttime := fields[starttimeIdx]

	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(bootID)) + "-" + starttime, true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mcpinit/... -run TestProcessStartTime -v`
Expected: PASS (all 3 subtests)

- [ ] **Step 5: Commit**

```bash
git add internal/mcpinit/obsidiansync_linux.go internal/mcpinit/obsidiansync_linux_test.go
git commit -m "feat(mcpinit): add Linux process creation-time token"
```

---

### Task 2: Darwin `processStartTime`

**Files:**
- Create: `internal/mcpinit/obsidiansync_darwin.go`
- Test: `internal/mcpinit/obsidiansync_darwin_test.go`

This task cannot run on this Linux dev machine — verify with `GOOS=darwin GOARCH=arm64 go vet ./internal/mcpinit/...` and `GOOS=darwin GOARCH=amd64 go build ./internal/mcpinit/...` instead of executing the test. The test itself is still written now so it runs for real in a future macOS CI run or local macOS dev loop.

- [ ] **Step 1: Write the test (cannot execute locally — write it first per TDD intent, verify by cross-compiling)**

```go
//go:build darwin

package mcpinit

import (
	"os"
	"testing"
)

func TestProcessStartTime_OwnPID(t *testing.T) {
	token, ok := processStartTime(os.Getpid())
	if !ok {
		t.Fatal("processStartTime(own pid) ok = false, want true")
	}
	if token == "" {
		t.Error("processStartTime(own pid) returned an empty token with ok=true")
	}
}

func TestProcessStartTime_Stable(t *testing.T) {
	a, okA := processStartTime(os.Getpid())
	b, okB := processStartTime(os.Getpid())
	if !okA || !okB {
		t.Fatal("processStartTime(own pid) failed on a repeat call")
	}
	if a != b {
		t.Errorf("processStartTime(own pid) not stable across calls: %q vs %q", a, b)
	}
}

func TestProcessStartTime_ImplausiblePID(t *testing.T) {
	if _, ok := processStartTime(999999999); ok {
		t.Error("processStartTime(implausible pid) ok = true, want false")
	}
}
```

- [ ] **Step 2: Verify it fails to compile (proves the symbol doesn't exist yet)**

Run: `GOOS=darwin GOARCH=arm64 go vet ./internal/mcpinit/...`
Expected: FAIL with `undefined: processStartTime`

- [ ] **Step 3: Write minimal implementation**

```go
//go:build darwin

package mcpinit

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// processStartTime returns an opaque token identifying pid's process
// creation instant, or ("", false) if it can't be determined. KinfoProc's
// P_starttime is a wall-clock Timeval (seconds+microseconds since epoch),
// so — unlike Linux's boot-relative ticks — it is already reboot-safe with
// no extra prefixing needed.
func processStartTime(pid int) (string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", false
	}
	tv := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%d", tv.Sec, tv.Usec), true
}
```

- [ ] **Step 4: Verify it builds and vets clean**

Run: `GOOS=darwin GOARCH=arm64 go vet ./internal/mcpinit/... && GOOS=darwin GOARCH=amd64 go build ./internal/mcpinit/...`
Expected: no output, exit 0

- [ ] **Step 5: Commit**

```bash
git add internal/mcpinit/obsidiansync_darwin.go internal/mcpinit/obsidiansync_darwin_test.go
git commit -m "feat(mcpinit): add Darwin process creation-time token"
```

---

### Task 3: Fallback `processStartTime` for other Unix

**Files:**
- Create: `internal/mcpinit/obsidiansync_unix_other.go`

No test file — this is a one-line intentional no-op, and its only observable behavior (fail open to legacy PID-only liveness) is already covered by Task 5/6's `isProcessAlive` tests using `haveToken=false`.

- [ ] **Step 1: Write the implementation**

```go
//go:build !windows && !linux && !darwin

package mcpinit

// processStartTime has no known implementation on this OS. Returning
// ("", false) degrades callers to legacy PID-only liveness checking,
// identical to today's behavior — no regression, just no reuse detection
// on whatever unlisted Unix this is.
func processStartTime(pid int) (string, bool) {
	return "", false
}
```

- [ ] **Step 2: Verify it compiles for a representative excluded OS**

Run: `GOOS=freebsd GOARCH=amd64 go build ./internal/mcpinit/...`
Expected: no output, exit 0 (this also proves the build-tag exclusion is correct: if it overlapped with linux/darwin/windows, those builds would now fail on a duplicate `processStartTime` definition — check that too)
Run: `go build ./internal/mcpinit/... && GOOS=darwin GOARCH=arm64 go build ./internal/mcpinit/... && GOOS=windows GOARCH=amd64 go build ./internal/mcpinit/...`
Expected: no output, exit 0 for all three

- [ ] **Step 3: Commit**

```bash
git add internal/mcpinit/obsidiansync_unix_other.go
git commit -m "feat(mcpinit): fall back to legacy liveness on unlisted Unix"
```

---

### Task 4: Windows `processStartTime`

**Files:**
- Modify: `internal/mcpinit/obsidiansync_windows.go`
- Create: `internal/mcpinit/obsidiansync_windows_test.go`

Same cross-compile-only caveat as Task 2 — this cannot execute on this Linux machine.

- [ ] **Step 1: Write the test**

```go
//go:build windows

package mcpinit

import (
	"os"
	"testing"
)

func TestProcessStartTime_OwnPID(t *testing.T) {
	token, ok := processStartTime(os.Getpid())
	if !ok {
		t.Fatal("processStartTime(own pid) ok = false, want true")
	}
	if token == "" {
		t.Error("processStartTime(own pid) returned an empty token with ok=true")
	}
}

func TestProcessStartTime_Stable(t *testing.T) {
	a, okA := processStartTime(os.Getpid())
	b, okB := processStartTime(os.Getpid())
	if !okA || !okB {
		t.Fatal("processStartTime(own pid) failed on a repeat call")
	}
	if a != b {
		t.Errorf("processStartTime(own pid) not stable across calls: %q vs %q", a, b)
	}
}

func TestProcessStartTime_ImplausiblePID(t *testing.T) {
	if _, ok := processStartTime(999999999); ok {
		t.Error("processStartTime(implausible pid) ok = true, want false")
	}
}
```

- [ ] **Step 2: Verify it fails to compile**

Run: `GOOS=windows GOARCH=amd64 go vet ./internal/mcpinit/...`
Expected: FAIL with `undefined: processStartTime`

- [ ] **Step 3: Add the implementation to `obsidiansync_windows.go`**

Add `"strconv"` to the existing import block, then append below `isProcessAlive` (implementation unchanged for now — signature change is Task 6):

```go
// processStartTime returns an opaque token identifying pid's process
// creation instant, or ("", false) if it can't be determined. The
// creation Filetime is wall-clock (100ns intervals since 1601-01-01), so
// it is already reboot-safe with no extra prefixing needed.
func processStartTime(pid int) (string, bool) {
	if pid <= 0 || int64(pid) > 0xFFFFFFFF {
		return "", false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	var creationTime, exitTime, kernelTime, userTime windows.Filetime
	if err := windows.GetProcessTimes(h, &creationTime, &exitTime, &kernelTime, &userTime); err != nil {
		return "", false
	}
	value := uint64(creationTime.HighDateTime)<<32 | uint64(creationTime.LowDateTime)
	return strconv.FormatUint(value, 10), true
}
```

The full updated import block:

```go
import (
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)
```

- [ ] **Step 4: Verify it builds and vets clean**

Run: `GOOS=windows GOARCH=amd64 go vet ./internal/mcpinit/... && GOOS=windows GOARCH=arm64 go build ./internal/mcpinit/...`
Expected: no output, exit 0

- [ ] **Step 5: Commit**

```bash
git add internal/mcpinit/obsidiansync_windows.go internal/mcpinit/obsidiansync_windows_test.go
git commit -m "feat(mcpinit): add Windows process creation-time token"
```

---

### Task 5: Token-aware `isProcessAlive` on POSIX

**Files:**
- Modify: `internal/mcpinit/obsidiansync_unix.go`
- Modify: `internal/mcpinit/obsidiansync_test.go` (add tests; this file has no build tag, so it runs on every OS including Windows in CI... it doesn't today since it's Unix-focused, but check: existing `obsidiansync_test.go` has no build tag and calls `isAlive`, `ensureObsidianSyncRunning` — those are OS-agnostic entry points already, so this is fine. New tests added here must also be OS-agnostic; anything Unix-signal-specific goes in Task 5 only if it needs no build tag. Since `isAlive`/`isProcessAlive` behavior differs on Windows only in the underlying liveness primitive, not the token contract, token behavior tests belong in the OS-agnostic file (they exercise `isAlive`, not `isProcessAlive` directly) — see Task 7.)

This task only touches the POSIX signal-0 file itself.

- [ ] **Step 1: Write the failing test** (Linux-only, since it needs `processStartTime` from Task 1; darwin gets the same coverage for free via Task 2's build)

```go
//go:build linux

package mcpinit

import (
	"os"
	"testing"
)

func TestIsProcessAlive_TokenMatch(t *testing.T) {
	token, ok := processStartTime(os.Getpid())
	if !ok {
		t.Fatal("processStartTime(own pid) failed; can't set up test")
	}
	if !isProcessAlive(os.Getpid(), token, true) {
		t.Error("isProcessAlive(own pid, own true token) = false, want true")
	}
}

func TestIsProcessAlive_TokenMismatch(t *testing.T) {
	if isProcessAlive(os.Getpid(), "definitely-not-the-real-token", true) {
		t.Error("isProcessAlive(own pid, wrong token) = true, want false (PID-reuse must be detected)")
	}
}

func TestIsProcessAlive_NoToken(t *testing.T) {
	if !isProcessAlive(os.Getpid(), "", false) {
		t.Error("isProcessAlive(own pid, haveToken=false) = false, want true (legacy behavior preserved)")
	}
}

func TestIsProcessAlive_DeadPIDIgnoresToken(t *testing.T) {
	if isProcessAlive(999999999, "anything", true) {
		t.Error("isProcessAlive(implausible pid, ...) = true, want false")
	}
}
```

Add this file as `internal/mcpinit/obsidiansync_linux_test.go`'s continuation — actually, append these functions into the *same* `obsidiansync_linux_test.go` created in Task 1 (same build tag, same file, avoids a near-duplicate file). Skip creating a new file for this step; edit the Task 1 file instead.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcpinit/... -run TestIsProcessAlive -v`
Expected: FAIL — `isProcessAlive` still takes 1 argument, so this won't compile: `too many arguments in call to isProcessAlive`

- [ ] **Step 3: Change `isProcessAlive`'s signature and implementation**

In `internal/mcpinit/obsidiansync_unix.go`, replace:

```go
// isProcessAlive reports whether pid names a running process, by sending
// it signal 0 — this checks existence and permission without actually
// signaling the process.
func isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
```

with:

```go
// isProcessAlive reports whether pid names a running process, by sending
// it signal 0 — this checks existence and permission without actually
// signaling the process. If haveToken is true, a live PID is additionally
// required to still carry wantToken as its creation-time token — a
// mismatch means the OS recycled pid to an unrelated process after the
// original one exited, which plain signal-0 can't distinguish from the
// original still running. A transient failure to read the fresh token
// fails open to "alive" rather than flapping a live process to "dead".
func isProcessAlive(pid int, wantToken string, haveToken bool) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if proc.Signal(syscall.Signal(0)) != nil {
		return false
	}
	if !haveToken {
		return true
	}
	token, ok := processStartTime(pid)
	if !ok {
		return true
	}
	return token == wantToken
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mcpinit/... -run TestIsProcessAlive -v`
Expected: PASS (all 4 subtests). Note: the full package won't compile yet — `obsidiansync.go`'s `isAlive` and `obsidiansync_windows.go`'s `isProcessAlive` still call/define the old 1-arg form. Run the narrower Linux-only test target instead if the full package fails to build:

Run: `go vet ./internal/mcpinit/... 2>&1 | head -20`
Expected at this point: errors from `obsidiansync.go` (`isAlive` calling `isProcessAlive(pid)` with 1 arg) and `obsidiansync_windows.go` (duplicate-signature mismatch is fine since it's build-tag excluded on Linux, but its own `isProcessAlive(pid int) bool` doesn't match this file if both were ever compiled together — they never are, different build tags, so no conflict). This is expected and resolved by Task 8 (which updates `isAlive`'s caller); do not fix it here.

- [ ] **Step 5: Commit**

```bash
git add internal/mcpinit/obsidiansync_unix.go internal/mcpinit/obsidiansync_linux_test.go
git commit -m "feat(mcpinit): make POSIX isProcessAlive token-aware"
```

---

### Task 6: Token-aware `isProcessAlive` on Windows

**Files:**
- Modify: `internal/mcpinit/obsidiansync_windows.go`
- Modify: `internal/mcpinit/obsidiansync_windows_test.go`

- [ ] **Step 1: Write the failing test** (append to the file created in Task 4)

```go
func TestIsProcessAlive_TokenMatch(t *testing.T) {
	token, ok := processStartTime(os.Getpid())
	if !ok {
		t.Fatal("processStartTime(own pid) failed; can't set up test")
	}
	if !isProcessAlive(os.Getpid(), token, true) {
		t.Error("isProcessAlive(own pid, own true token) = false, want true")
	}
}

func TestIsProcessAlive_TokenMismatch(t *testing.T) {
	if isProcessAlive(os.Getpid(), "definitely-not-the-real-token", true) {
		t.Error("isProcessAlive(own pid, wrong token) = true, want false (PID-reuse must be detected)")
	}
}

func TestIsProcessAlive_NoToken(t *testing.T) {
	if !isProcessAlive(os.Getpid(), "", false) {
		t.Error("isProcessAlive(own pid, haveToken=false) = false, want true (legacy behavior preserved)")
	}
}
```

- [ ] **Step 2: Verify it fails to compile**

Run: `GOOS=windows GOARCH=amd64 go vet ./internal/mcpinit/...`
Expected: FAIL — `too many arguments in call to isProcessAlive`

- [ ] **Step 3: Change `isProcessAlive`'s signature and implementation**

Replace:

```go
// isProcessAlive reports whether pid names a running process. Unlike POSIX,
// Windows aggressively recycles PIDs, so a PID that opens successfully but
// has already exited (GetExitCodeProcess returns anything but
// STILL_ACTIVE) is treated as not alive — otherwise a reused PID belonging
// to an unrelated process would be mistaken for the one that owned the PID
// file.
func isProcessAlive(pid int) bool {
	if pid <= 0 || int64(pid) > 0xFFFFFFFF {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	const stillActive = 259 // STILL_ACTIVE, per the Win32 GetExitCodeProcess docs

	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	return exitCode == stillActive
}
```

with:

```go
// isProcessAlive reports whether pid names a running process. Unlike POSIX,
// Windows aggressively recycles PIDs, so a PID that opens successfully but
// has already exited (GetExitCodeProcess returns anything but
// STILL_ACTIVE) is treated as not alive — otherwise a reused PID belonging
// to an unrelated process would be mistaken for the one that owned the PID
// file. If haveToken is true, a live PID is additionally required to still
// carry wantToken as its creation-time token — a mismatch means the OS
// recycled pid to an unrelated process after the original one exited,
// which STILL_ACTIVE alone can't distinguish from the original still
// running. A transient failure to read the fresh token fails open to
// "alive" rather than flapping a live process to "dead".
func isProcessAlive(pid int, wantToken string, haveToken bool) bool {
	if pid <= 0 || int64(pid) > 0xFFFFFFFF {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	const stillActive = 259 // STILL_ACTIVE, per the Win32 GetExitCodeProcess docs

	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	if exitCode != stillActive {
		return false
	}
	if !haveToken {
		return true
	}
	token, ok := processStartTime(pid)
	if !ok {
		return true
	}
	return token == wantToken
}
```

- [ ] **Step 4: Verify it builds and vets clean**

Run: `GOOS=windows GOARCH=amd64 go vet ./internal/mcpinit/... && GOOS=windows GOARCH=arm64 go build ./internal/mcpinit/...`
Expected: this still fails until Task 8 fixes `obsidiansync.go`'s caller — expected, same as Task 5 Step 4; do not fix it here.

- [ ] **Step 5: Commit**

```bash
git add internal/mcpinit/obsidiansync_windows.go internal/mcpinit/obsidiansync_windows_test.go
git commit -m "feat(mcpinit): make Windows isProcessAlive token-aware"
```

---

### Task 7: `atomicWritePID` and `claimPidFile` gain tokens (closes the placeholder-write gap)

**Files:**
- Modify: `internal/mcpinit/stophook.go`
- Modify: `internal/mcpinit/stophook_test.go`

This is Fable's Defect 3 fix: `claimPidFile`'s placeholder write (`os.Getpid()`, before `cmd.Start()`) currently has no creation-time guard, so if one of the three early-return paths after it fires, the placeholder is stuck on disk indistinguishable from a genuinely-stale PID once the short-lived hook process exits and its PID gets reused.

- [ ] **Step 1: Write the failing test**

```go
// TestClaimPidFile_PlaceholderCarriesToken proves the placeholder PID
// claimPidFile writes (the caller's own PID, before it has spawned
// anything) is written in the same "pid:token" format as a real spawn —
// otherwise an abandoned placeholder (caller hits an early-return failure
// path after claiming but before cmd.Start()) has no creation-time guard
// and reproduces the exact PID-reuse bug this design fixes.
func TestClaimPidFile_PlaceholderCarriesToken(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "resolve-test.pid")

	if !claimPidFile(pidPath) {
		t.Fatal("claimPidFile on an empty slot = false, want true")
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pidPath: %v", err)
	}
	content := strings.TrimSpace(string(data))
	pidStr, token, haveToken := strings.Cut(content, ":")
	if _, err := strconv.Atoi(pidStr); err != nil {
		t.Errorf("placeholder content has no valid leading PID: %q", content)
	}
	// processStartTime can legitimately return ok=false on some platforms
	// (obsidiansync_unix_other.go, or a transient read failure) — when it
	// does, the placeholder correctly falls back to the bare-PID legacy
	// format instead of claiming a token it doesn't have.
	if token, ok := processStartTime(os.Getpid()); ok && (!haveToken || token == "") {
		t.Errorf("processStartTime succeeded (token=%q) but placeholder has no token: %q", token, content)
	}
	_ = token
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcpinit/... -run TestClaimPidFile_PlaceholderCarriesToken -v`
Expected: FAIL — today's `claimPidFile` writes a bare PID with no colon, so on any platform where `processStartTime(os.Getpid())` succeeds, `haveToken` is false and the assertion trips.

- [ ] **Step 3: Update `atomicWritePID` and `claimPidFile`**

Replace:

```go
// atomicWritePID writes pid into path via write-temp-then-rename so a
// concurrent reader (e.g. another caller's claimPidFile, which reads this
// file under its own lock) never observes a truncated or partially-written
// file — os.WriteFile's open+truncate+write is not atomic and this call site
// runs outside any lock.
func atomicWritePID(path string, pid int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
```

with:

```go
// atomicWritePID writes pid (and, when haveToken is true, its creation-time
// token in "pid:token" form — see processStartTime) into path via
// write-temp-then-rename so a concurrent reader (e.g. another caller's
// claimPidFile, which reads this file under its own lock) never observes a
// truncated or partially-written file — os.WriteFile's
// open+truncate+write is not atomic and this call site runs outside any
// lock. When haveToken is false, the bare-PID legacy format is written,
// same as before this token support existed.
func atomicWritePID(path string, pid int, token string, haveToken bool) error {
	content := strconv.Itoa(pid)
	if haveToken {
		content += ":" + token
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
```

Replace:

```go
	if isAlive(pidPath) {
		return false
	}
	return atomicWritePID(pidPath, os.Getpid()) == nil
}
```

with:

```go
	if isAlive(pidPath) {
		return false
	}
	token, haveToken := processStartTime(os.Getpid())
	return atomicWritePID(pidPath, os.Getpid(), token, haveToken) == nil
}
```

(This is the end of `claimPidFile` — only its last 3 lines change; the lock-file setup above it is untouched.)

Now fix the two remaining call sites in the same file — `spawnResolveIfConfigured` and `spawnSupersedeIfConfigured` — both currently end with:

```go
	_ = atomicWritePID(pidPath, cmd.Process.Pid)
	_ = cmd.Process.Release()
```

Replace **both** occurrences with:

```go
	token, haveToken := processStartTime(cmd.Process.Pid)
	_ = atomicWritePID(pidPath, cmd.Process.Pid, token, haveToken)
	_ = cmd.Process.Release()
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mcpinit/... -run TestClaimPidFile -v`
Expected: PASS, including the two pre-existing `TestClaimPidFile_ConcurrentCallersOnlyOneWins*` tests (unaffected — they only assert PID parses, which still holds with the new `"pid:token"` format).

- [ ] **Step 5: Commit**

```bash
git add internal/mcpinit/stophook.go internal/mcpinit/stophook_test.go
git commit -m "fix(mcpinit): give claimPidFile's placeholder write a creation-time token"
```

---

### Task 8: `isAlive` parses the token format; `ensureObsidianSyncRunning` writes one

**Files:**
- Modify: `internal/mcpinit/obsidiansync.go`
- Modify: `internal/mcpinit/obsidiansync_test.go`

This is the task that makes the whole package compile again (Tasks 5–6 changed `isProcessAlive`'s signature; `isAlive` is its only caller and still calls the old 1-arg form until now).

- [ ] **Step 1: Write the failing tests**

Add to `internal/mcpinit/obsidiansync_test.go`:

```go
func TestIsAlive_LegacyBarePIDStillWorks(t *testing.T) {
	// A pre-upgrade pidfile (no colon) must keep working exactly as before —
	// no migration step, no regression, just no reuse detection.
	pidPath := filepath.Join(t.TempDir(), "obsidian-sync.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isAlive(pidPath) {
		t.Error("isAlive() = false for a legacy bare-PID file naming this process, want true")
	}
}

func TestIsAlive_TokenFormatOwnPID(t *testing.T) {
	token, ok := processStartTime(os.Getpid())
	if !ok {
		t.Skip("processStartTime unsupported on this platform; nothing to test here")
	}
	pidPath := filepath.Join(t.TempDir(), "obsidian-sync.pid")
	content := strconv.Itoa(os.Getpid()) + ":" + token
	if err := os.WriteFile(pidPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isAlive(pidPath) {
		t.Error("isAlive() = false for own pid:token, want true")
	}
}

func TestIsAlive_TokenMismatchMeansReuse(t *testing.T) {
	if _, ok := processStartTime(os.Getpid()); !ok {
		t.Skip("processStartTime unsupported on this platform; nothing to test here")
	}
	pidPath := filepath.Join(t.TempDir(), "obsidian-sync.pid")
	content := strconv.Itoa(os.Getpid()) + ":stale-token-from-a-different-process-instance"
	if err := os.WriteFile(pidPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if isAlive(pidPath) {
		t.Error("isAlive() = true for own pid with a mismatched token, want false (this is the PID-reuse regression test)")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go build ./internal/mcpinit/...`
Expected: FAIL — `isAlive`'s call `isProcessAlive(pid)` now has too few arguments (`isProcessAlive` takes 3 as of Task 5/6)

- [ ] **Step 3: Update `isAlive` and `ensureObsidianSyncRunning`**

Replace:

```go
// isAlive reports whether pidPath names a PID file for a process that is
// still running. It never treats a stale or missing PID file as an error —
// the caller's only decision is "spawn a new one, or not". The liveness
// check itself is platform-specific — see isProcessAlive in
// obsidiansync_unix.go / obsidiansync_windows.go.
func isAlive(pidPath string) bool {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	return isProcessAlive(pid)
}
```

with:

```go
// isAlive reports whether pidPath names a PID file for a process that is
// still running. It never treats a stale or missing PID file as an error —
// the caller's only decision is "spawn a new one, or not". The file holds
// either a bare PID (legacy format, liveness-only) or "pid:token" (see
// processStartTime), in which case a live PID additionally has to still
// carry that token to be reported alive — see isProcessAlive in
// obsidiansync_unix.go / obsidiansync_windows.go.
func isAlive(pidPath string) bool {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	pidStr, token, haveToken := strings.Cut(strings.TrimSpace(string(data)), ":")
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false
	}
	return isProcessAlive(pid, token, haveToken)
}
```

Then replace `ensureObsidianSyncRunning`'s write line:

```go
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	_ = cmd.Process.Release()
```

with:

```go
	token, haveToken := processStartTime(cmd.Process.Pid)
	_ = atomicWritePID(pidPath, cmd.Process.Pid, token, haveToken)
	_ = cmd.Process.Release()
```

This drops the direct `os.WriteFile` call in favor of the same atomic, token-aware helper the two stop-hook spawners use — one write path instead of two slightly different ones. `strconv` stays imported (still used by `isAlive`'s `strconv.Atoi`); no import list changes needed in this file.

- [ ] **Step 4: Run test to verify it passes**

Run: `go build ./... && go vet ./... && go test ./internal/mcpinit/... -v`
Expected: builds clean; all tests pass, including every pre-existing test in `obsidiansync_test.go` and `stophook_test.go` (none of their assertions depended on the old bare-PID write format surviving — `TestIsAlive_LiveProcess` writes a bare PID directly via `os.WriteFile` and still hits the legacy fallback path, which is intentionally preserved).

- [ ] **Step 5: Commit**

```bash
git add internal/mcpinit/obsidiansync.go internal/mcpinit/obsidiansync_test.go
git commit -m "feat(mcpinit): isAlive detects PID reuse via creation-time token"
```

---

### Task 9: Full cross-platform verification

**Files:** none (verification only)

- [ ] **Step 1: Run the full test suite and vet on the native platform**

Run: `go vet ./... && go test ./...`
Expected: all packages pass, no vet warnings

- [ ] **Step 2: Cross-compile every goreleaser target**

Run:
```bash
GOOS=linux   GOARCH=amd64 go build ./... && \
GOOS=linux   GOARCH=arm64 go build ./... && \
GOOS=darwin  GOARCH=amd64 go build ./... && \
GOOS=darwin  GOARCH=arm64 go build ./... && \
GOOS=windows GOARCH=amd64 go build ./... && \
GOOS=windows GOARCH=arm64 go build ./... && \
echo ALL_OK
```
Expected: `ALL_OK` printed, no build errors on any target — this is the only verification available for the darwin/windows code paths on this Linux dev machine, matching CI's own limitation (CI only runs `ubuntu-latest`).

- [ ] **Step 3: Run `go vet` per cross-compiled target too (vet does more static checking than a bare build)**

Run:
```bash
GOOS=darwin  GOARCH=arm64 go vet ./... && \
GOOS=windows GOARCH=amd64 go vet ./... && \
echo VET_OK
```
Expected: `VET_OK` printed

- [ ] **Step 4: No commit needed** — this task is pure verification. If any step fails, fix the offending file from the earlier task that introduced it and re-run this task from Step 1.

---

## Spec Coverage Check

- PID file format (bare = legacy, `pid:token` = new) → Tasks 7, 8. ✓
- Per-platform `processStartTime` (linux/darwin/windows/other) → Tasks 1–4. ✓
- Linux boot_id reboot-safety fix → Task 1. ✓
- Darwin wall-clock `Timeval` string format → Task 2. ✓
- Windows wall-clock `Filetime` string format → Task 4. ✓
- `isProcessAlive` 3-arg token-aware signature (POSIX + Windows) → Tasks 5, 6. ✓
- Fail-open on every new syscall path → Tasks 1–6 (every `processStartTime`/`isProcessAlive` implementation returns/treats failure as "alive"/"unsupported", never errors up). ✓
- Write path: `ensureObsidianSyncRunning`, `spawnResolveIfConfigured`, `spawnSupersedeIfConfigured`, `atomicWritePID` → Tasks 7, 8. ✓
- `claimPidFile` placeholder-write token fix (Defect 3) → Task 7. ✓
- Cross-platform build/vet verification → Task 9. ✓

No spec section is left uncovered.

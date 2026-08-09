# PID-Reuse Detection via Process Creation Time

Closes item 3 of #252 (Windows PID-reuse detection), extended to cover the
identical, previously-accepted gap on POSIX (Linux/macOS signal-0 checks).

## Problem

`isAlive()` in `internal/mcpinit/obsidiansync.go` decides whether a
background process (obsidian-sync, resolve, supersede) is still running by
reading a PID from a `.pid` file and calling platform-specific
`isProcessAlive(pid)`:

- POSIX (`obsidiansync_unix.go`): `os.FindProcess` + signal 0.
- Windows (`obsidiansync_windows.go`): `OpenProcess` +
  `GetExitCodeProcess` == `STILL_ACTIVE`.

Both checks only prove *some* process currently holds that PID number — not
that it's the *same* process instance that wrote the file. Once the original
process exits, the OS is free to hand that PID to any new, unrelated
process. If that reuse happens while a stale `.pid` file still names the old
PID, `isAlive` wrongly reports "still running," and the caller (session-start
hook for obsidian-sync, stop hook for resolve/supersede) skips spawning a
real one — silently breaking the auto-sync/auto-resolve/auto-supersede
features until the file is manually cleared.

All three PID files (`obsidian-sync.pid`, `resolve-<project>.pid`,
`supersede-<project>.pid`) go through this same code path, so fixing it here
fixes all three call sites in `stophook.go` and `obsidiansync.go`.

## Fix

Record each process's OS-assigned creation time alongside its PID at write
time. At read time, re-derive the creation time for whatever process
currently holds that PID and require it to match. A reused PID will almost
certainly have a different creation time, revealing the reuse.

Creation time is never interpreted as wall-clock time or converted across
platforms — it's an opaque per-platform integer, compared only for equality
against a value obtained the same way on the same machine.

## Components

### 1. PID file format

`"<pid>:<creationTime>"`, e.g. `"12345:412039182"`.

A bare integer with no colon (today's on-disk format) parses as *legacy*:
PID known, creation time unknown. Legacy files fall back to today's
PID-only liveness check — no reuse detection, but no regression either. The
next time that slot is claimed and rewritten, it upgrades to the new format
automatically. No migration step or format-version bump is needed.

### 2. Per-platform creation-time reader

New function, one implementation per OS, returning `(0, false)` on any
failure (unreadable proc entry, permission denied, unsupported OS):

```go
func processStartTime(pid int) (start uint64, ok bool)
```

- **`obsidiansync_linux.go`**: read `/proc/<pid>/stat`. The `comm` field
  (2nd field) can itself contain spaces and parentheses, so split on the
  *last* `)` in the line before tokenizing the remainder by spaces — field
  22 overall is then remainder-field 20 (1-indexed after the split).
  Field 22 is `starttime`: clock ticks since boot. Never converted to wall
  time; compared as-is.
- **`obsidiansync_darwin.go`**: `unix.SysctlKinfoProc("kern.proc.pid", pid)`
  (`golang.org/x/sys/unix`, already an indirect dependency) →
  `KinfoProc.Proc.P_starttime`, a `Timeval`. Compared as
  `(Sec, Usec)` tuple equality.
- **`obsidiansync_windows.go`** (extends the existing file): use the
  process handle already opened by `isProcessAlive` to call
  `windows.GetProcessTimes`, taking the creation `Filetime` as a `uint64`.
- **`obsidiansync_unix_other.go`** (`//go:build !windows && !linux && !darwin`):
  always returns `(0, false)`. Covers any other Unix goreleaser might target
  in the future; degrades to PID-only liveness, matching today's behavior
  exactly.

### 3. `isProcessAlive` signature change (all platform files)

```go
func isProcessAlive(pid int, wantStart uint64, haveStart bool) bool
```

Existing liveness check runs first (unchanged). If it reports alive and
`haveStart` is true, additionally calls `processStartTime(pid)` for a fresh
reading and requires it to equal `wantStart` — mismatch means the PID was
recycled, so report not-alive. If the fresh reading itself fails
(`ok == false`), fail open to "alive" (today's behavior) rather than
flapping a live process to "dead" because of a transient read error.

### 4. Write path

`ensureObsidianSyncRunning`, `spawnResolveIfConfigured`,
`spawnSupersedeIfConfigured`, `atomicWritePID`, and `claimPidFile` all
currently write a bare PID. After `cmd.Start()`, read the new child's start
time via `processStartTime(cmd.Process.Pid)` and write `"pid:starttime"` (or
bare `pid` when `ok` is false — same as today, and forward-compatible with
the legacy-format fallback in `isAlive`).

`isAlive(pidPath)` itself keeps its existing signature; internally it now
splits on `:`, parses both parts when present, and calls the 3-arg
`isProcessAlive`.

## Error handling

Every new syscall path fails open to today's PID-only behavior rather than
erroring — consistent with this file's existing contract that PID-file
logic must never block or fail the session-start/stop hooks. No new error
returns propagate to callers; `isAlive` keeps returning a plain `bool`.

## Testing

- Platform-specific unit tests (build-tag gated, matching the existing
  `obsidiansync_unix.go` / `obsidiansync_windows.go` test split) for each new
  `processStartTime` implementation and for `isProcessAlive`'s 3-arg form:
  - Own PID + own true start time → alive.
  - Own PID + a deliberately wrong start time → not alive (this is the
    regression test for PID-reuse; it doesn't require actually triggering
    PID reuse, just asserting the mismatch path).
  - Legacy bare-PID file (no colon) → behaves exactly as before
    (liveness-only).
  - Malformed file content → not alive (existing behavior, unchanged).
- No changes needed to existing `obsidiansync_test.go` / `stophook_test.go`
  tests that exercise the public `isAlive(pidPath)` surface, since that
  signature is unchanged.
- `go vet ./...`, `go build ./...` for all three GOOS targets
  (`GOOS=linux`, `GOOS=darwin`, `GOOS=windows`) since CI only runs
  `ubuntu-latest` and cross-compiles the others — this is the only
  verification available for the darwin/windows paths short of manual
  testing on those OSes.

## Scope check

This is a self-contained change to `internal/mcpinit`: 3-4 new small
platform-specific files plus signature changes to 3 existing call sites
(`obsidiansync.go`, `stophook.go` write paths) and no new dependencies. Fits
a single implementation plan.

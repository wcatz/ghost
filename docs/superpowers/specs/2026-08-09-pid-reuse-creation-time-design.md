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
platforms or arithmetically compared — it's an **opaque per-platform
string token**, compared only for exact equality against a token obtained
the same way on the same machine. A string (not an integer) is required
because the per-platform sources aren't uniformly representable as one
numeric type (see Component 2), and because Linux's raw source value is not
by itself reboot-safe (see below).

## Components

### 1. PID file format

`"<pid>:<creationToken>"`, e.g. `"12345:a1b2c3d4-412039182"`.

A bare integer with no colon (today's on-disk format) parses as *legacy*:
PID known, creation token unknown. Legacy files fall back to today's
PID-only liveness check — no reuse detection, but no regression either. The
next time that slot is claimed and rewritten, it upgrades to the new format
automatically. No migration step or format-version bump is needed.

### 2. Per-platform creation-token reader

New function, one implementation per OS, returning `("", false)` on any
failure (unreadable proc entry, permission denied, unsupported OS):

```go
func processStartTime(pid int) (token string, ok bool)
```

- **`obsidiansync_linux.go`**: read `/proc/<pid>/stat`. The `comm` field
  (2nd field) can itself contain spaces and parentheses, so split on the
  *last* `)` in the line before tokenizing the remainder by spaces — field
  22 overall is then remainder-field 20 (1-indexed after the split).
  Field 22 is `starttime`: clock ticks since boot — **not wall-clock, and
  not reboot-safe on its own**: `.pid` files persist in `dataDir` across
  reboots, and after a reboot a freshly-started, unrelated process can
  coincidentally have the same boot-relative tick count a stale file
  recorded before the reboot, silently defeating the whole check for
  exactly the stale-file scenario this fix targets. Fixed by prefixing the
  machine's boot ID: read `/proc/sys/kernel/random/boot_id` (a fresh UUID
  generated at every boot) once and use `token = bootID + "-" + starttime`.
  Any reboot changes `bootID`, so a pre-reboot token can never match a
  post-reboot one, regardless of tick coincidence. If `boot_id` is
  unreadable, return `("", false)` (fails open to legacy PID-only
  liveness — same as any other read failure on this platform).
- **`obsidiansync_darwin.go`**: `unix.SysctlKinfoProc("kern.proc.pid", pid)`
  (`golang.org/x/sys/unix`, already a **direct** dependency — imported
  today by `obsidiansync_windows.go`) → `KinfoProc.Proc.P_starttime`, a
  `Timeval{Sec, Usec}`. `Timeval` is wall-clock (seconds since epoch), so it
  is already reboot-safe with no extra prefixing needed. Format as
  `token = fmt.Sprintf("%d.%d", Sec, Usec)`.
- **`obsidiansync_windows.go`** (extends the existing file): use the
  process handle already opened by `isProcessAlive` to call
  `windows.GetProcessTimes`, taking the creation `Filetime` (also
  wall-clock — 100ns intervals since 1601-01-01 — so already reboot-safe).
  Combine its `HighDateTime`/`LowDateTime` fields into a single `uint64`
  (`Filetime.Nanoseconds()`'s underlying value, or the equivalent bit-shift)
  and format with `strconv.FormatUint(value, 10)`.
- **`obsidiansync_unix_other.go`** (`//go:build !windows && !linux && !darwin`):
  always returns `("", false)`. Covers any other Unix goreleaser might
  target in the future; degrades to PID-only liveness, matching today's
  behavior exactly.

### 3. `isProcessAlive` signature change (all platform files)

```go
func isProcessAlive(pid int, wantToken string, haveToken bool) bool
```

Existing liveness check runs first (unchanged). If it reports alive and
`haveToken` is true, additionally calls `processStartTime(pid)` for a fresh
reading and requires it to equal `wantToken` (plain string equality) —
mismatch means the PID was recycled, so report not-alive. If the fresh
reading itself fails (`ok == false`), fail open to "alive" (today's
behavior) rather than flapping a live process to "dead" because of a
transient read error.

### 4. Write path

`ensureObsidianSyncRunning`, `spawnResolveIfConfigured`,
`spawnSupersedeIfConfigured`, `atomicWritePID`, and `claimPidFile` all
currently write a bare PID; `claimPidFile` writes one in *two* places that
both need to move to the new format:

- **Placeholder claim** (`claimPidFile`, before spawning): today it writes
  `os.Getpid()` — the stop-hook's own short-lived PID — as a lock-holding
  placeholder, then three early-return paths in the caller
  (`os.Executable` failure, log-file `OpenFile` failure, `cmd.Start()`
  failure) can leave that placeholder on disk permanently after the hook
  process exits, with no way to distinguish it from a stale reused PID —
  reproducing the exact bug this design fixes. Fix: `claimPidFile` computes
  its own token via `processStartTime(os.Getpid())` and writes
  `"pid:token"` (or bare `pid` if unsupported) for the placeholder too, so
  even an abandoned placeholder is correctly detected as dead once the hook
  process exits and its PID is reused.
- **Real write** (after `cmd.Start()` succeeds): read the new child's token
  via `processStartTime(cmd.Process.Pid)` and write `"pid:token"` (or bare
  `pid` when `ok` is false).

Both writes go through one shared helper so the format logic isn't
duplicated:

```go
func atomicWritePID(path string, pid int) error // existing signature, callers updated below
```

becomes

```go
func atomicWritePID(path string, pid int, token string, haveToken bool) error
```

which formats `"pid:token"` or bare `pid` internally before the existing
temp-file-then-rename logic. Every call site (`claimPidFile`'s placeholder
write, `claimPidFile`'s real write, `ensureObsidianSyncRunning`,
`spawnResolveIfConfigured`, `spawnSupersedeIfConfigured`) computes its own
`processStartTime` result and passes it through this one function — no
formatting logic duplicated across call sites.

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
  - Own PID + own true token → alive.
  - Own PID + a deliberately wrong token string → not alive (this is the
    regression test for PID-reuse; it doesn't require actually triggering
    PID reuse, just asserting the mismatch path).
  - Legacy bare-PID file (no colon) → behaves exactly as before
    (liveness-only).
  - Malformed file content → not alive (existing behavior, unchanged).
  - Linux only: two `processStartTime` calls for the same live PID return
    identical tokens (boot_id + starttime is a stable read, not
    regenerated per call).
- `claimPidFile` test: simulate a failure on one of the three post-claim,
  pre-`cmd.Start()` paths (e.g. inject a `cmd.Start()` failure) and assert
  that the leftover placeholder file, once the test's own PID naturally
  exits/changes, is correctly detected as not-alive rather than wedging the
  lock forever — covers the fix for the placeholder-write gap.
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

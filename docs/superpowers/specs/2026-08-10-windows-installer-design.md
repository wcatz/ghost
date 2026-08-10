# Windows one-command installer (`install.ps1`)

## Problem

Filed by Skinner (opencode agent testing Ghost on a UTM Windows VM):

- **#257**: No one-command install on Windows. Today, getting `ghost.exe` onto a
  Windows machine requires either cross-compiling and `scp`-ing the binary by
  hand, or manually downloading and unzipping a GitHub release asset.
- **#258**: No PATH setup as part of install. Even after the binary is placed,
  the user must run `setx PATH "%PATH%;C:\Users\ghost\bin"` by hand, in a shell
  where `%PATH%` still expands to the *old* value (so it silently drops
  anything set in that same session), only affects *new* processes, and `setx`
  truncates the value at 1024 chars.

POSIX users already have `make install` (copies to `$GOPATH/bin` or
`~/go/bin`). Windows has no equivalent — this design adds one.

## Goals

- A single command Windows users can run to install `ghost` and have it
  immediately usable in a new shell.
- No admin privileges required.
- No new signing/hosting infrastructure — reuse what `.goreleaser.yml`
  already produces (per-OS/arch zip archives + `checksums.txt`).
- Safe to re-run (upgrade in place).

## Non-goals

- No POSIX `install.sh` — #257/#258 are Windows-specific; POSIX already has
  `make install`. Parity script is a separate future issue if wanted.
- No automatic `ghost mcp init` invocation. The installer's job ends at
  "binary on PATH." Wiring Claude Code/opencode remains an explicit, separate,
  already-idempotent step the user runs themselves.
- No package-manager manifest (winget/scoop). Worth revisiting later, but out
  of scope here — the script is the fast, fully-in-our-control option for now.

## Design

### Distribution

`install.ps1` is committed to the repo root and served straight from GitHub:

```powershell
irm https://raw.githubusercontent.com/wcatz/ghost/main/install.ps1 | iex
```

No extra hosting, no release-asset step in goreleaser. The script always
reflects the `main` branch; because it downloads whatever the *latest GitHub
release* is (not a version pinned to itself), a stale copy of the script
served from an old commit still installs the current release correctly — the
script's own logic rarely needs to change once it's correct.

### Flow

1. **Detect architecture.** Use
   `[System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture`
   to map to `amd64` or `arm64`. Any other architecture (e.g. x86) is a hard
   error with a clear message — no silent fallback.
2. **Resolve latest release.** Query
   `https://api.github.com/repos/wcatz/ghost/releases/latest` for the tag
   name; build the expected asset filename
   `ghost_<version>_windows_<arch>.zip` (matching goreleaser's
   `name_template`) and the `checksums.txt` URL for that same release.
3. **Download.** Fetch both files into a temp directory
   (`$env:TEMP\ghost-install-<random>`).
4. **Verify.** Compute SHA256 of the downloaded zip
   (`Get-FileHash -Algorithm SHA256`) and compare against the matching line in
   `checksums.txt`. Mismatch → abort with a clear error, delete the temp dir,
   non-zero exit. No install proceeds on unverified binaries.
5. **Install.** Extract `ghost.exe` from the zip into
   `%LOCALAPPDATA%\ghost\bin`, creating the directory if needed. If a binary
   already exists there, it is overwritten — no prompt, no `-Force` flag
   needed. Re-running the script is how you upgrade, mirroring the
   already-idempotent `ghost mcp init` philosophy in this codebase.
6. **Update PATH.** Read `HKCU:\Environment`'s `PATH` value directly via the
   registry (`Get-ItemProperty`/`Set-ItemProperty`), not `setx`:
   - If `%LOCALAPPDATA%\ghost\bin` is already present (case-insensitive
     segment match), do nothing.
   - Otherwise append it and write back. The registry `REG_EXPAND_SZ` value
     has no 1024-char truncation limit, unlike `setx`.
   - Broadcast `WM_SETTINGCHANGE` via a small inline P/Invoke
     (`SendMessageTimeout` to `HWND_BROADCAST`) so some already-open
     processes (e.g. Explorer) pick up the change without a reboot. Note in
     the script's output that *already-open terminals* still need to be
     restarted — `WM_SETTINGCHANGE` doesn't retroactively update a running
     shell's own environment block.
7. **Report success**, printing the install path and instructing the user to
   open a new terminal and run `ghost mcp init` next.

### Error handling

Each failure mode produces a distinct, specific message and a non-zero exit,
with the temp directory cleaned up in a `finally` block:

- Network/API failure resolving the latest release
- Download failure (either asset)
- Checksum mismatch
- Unsupported architecture
- Extraction failure (corrupt zip)

No partial state is left behind (e.g. a half-extracted binary with PATH
already updated).

### Testing

PowerShell has no direct equivalent to Go's test suite in this repo, so this
isn't TDD in the Go sense used elsewhere in Ghost. The script will be
structured as small, named functions (`Get-LatestReleaseInfo`,
`Test-Checksum`, `Add-UserPathEntry`, `Install-Ghost`) so behavior is at least
readable and reviewable in isolation, but verification is manual: run the
script end-to-end on a real Windows environment (the UTM VM Skinner already
uses, or an equivalent) and confirm `ghost` resolves in a fresh terminal after
a clean run and after a re-run (upgrade path).

## Out of scope / future work

- winget/scoop manifests
- POSIX `install.sh` parity script
- Automatic `ghost mcp init` invocation from the installer

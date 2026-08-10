# Windows Installer (install.ps1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `install.ps1` at the repo root so Windows users can run
`irm https://raw.githubusercontent.com/wcatz/ghost/main/install.ps1 | iex` to
download, verify, and install the latest `ghost` release with PATH already
configured — closing #257 and #258.

**Architecture:** A single PowerShell script composed of small, named
functions (`Get-Arch`, `Get-LatestReleaseInfo`, `Test-Checksum`,
`Install-Ghost`, `Add-UserPathEntry`), each independently reviewable, wired
together by a top-level `main` block guarded by
`if ($MyInvocation.InvocationName -ne '.')` so the functions can also be
dot-sourced for review/inspection without executing the installer.

**Tech Stack:** Windows PowerShell 5.1+ / PowerShell 7+ compatible syntax
(avoid PS7-only operators), `Invoke-WebRequest`, `Get-FileHash`, Windows
registry cmdlets (`Get-ItemProperty`/`Set-ItemProperty` on
`HKCU:\Environment`), P/Invoke via `Add-Type` for `SendMessageTimeout` /
`WM_SETTINGCHANGE` broadcast.

**No Go-test equivalent exists for PowerShell in this repo, and this
environment has no `pwsh` installed (verified: not in PATH, not in the
default dnf repos).** Every function-level step below is verified two ways:
(1) `powershell -NoProfile -Command "... syntax check ..."` is NOT available
either, so verification is by careful line-by-line review of each function
against its stated contract, kept small enough to review completely; (2) the
final task is a real end-to-end run on a Windows machine (the UTM VM already
used for Skinner's testing), which only the user can execute and confirm.
This is documented up front rather than glossed over — do not claim automated
test coverage that does not exist.

---

### Task 1: Script skeleton and architecture detection

**Files:**
- Create: `install.ps1`

- [ ] **Step 1: Write the script header and `Get-Arch` function**

```powershell
#Requires -Version 5.1
<#
.SYNOPSIS
    Installs the latest ghost release and adds it to the user PATH.
.DESCRIPTION
    Downloads the latest windows/amd64 or windows/arm64 ghost release from
    GitHub, verifies its SHA256 against the release's checksums.txt, extracts
    ghost.exe into %LOCALAPPDATA%\ghost\bin, and adds that directory to the
    persistent user PATH (via the registry, not setx).
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$script:Repo = 'wcatz/ghost'
$script:InstallDir = Join-Path $env:LOCALAPPDATA 'ghost\bin'

function Get-Arch {
    <#
    .SYNOPSIS
        Maps the running process architecture to a goreleaser arch string.
    .OUTPUTS
        String: 'amd64' or 'arm64'. Throws on any other architecture.
    #>
    $arch = [System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture
    switch ($arch) {
        'X64' { return 'amd64' }
        'Arm64' { return 'arm64' }
        default {
            throw "Unsupported architecture: $arch. ghost only ships windows/amd64 and windows/arm64 builds."
        }
    }
}
```

- [ ] **Step 2: Review the function against its contract**

Confirm: `Get-Arch` returns exactly `'amd64'` or `'arm64'`, and throws (not
`Write-Error` + continue) for anything else — the caller must not proceed
with an unsupported arch. No test runner is available in this environment
(no `pwsh`); this is a manual code-reading check, not an executed test. Note
this explicitly rather than silently skipping verification.

- [ ] **Step 3: Commit**

```bash
git add install.ps1
git commit -m "feat: add install.ps1 skeleton with arch detection"
```

---

### Task 2: Resolve the latest release

**Files:**
- Modify: `install.ps1`

- [ ] **Step 1: Add `Get-LatestReleaseInfo`**

```powershell
function Get-LatestReleaseInfo {
    <#
    .SYNOPSIS
        Queries the GitHub API for the latest ghost release and builds the
        asset/checksum URLs for the given architecture.
    .PARAMETER Arch
        'amd64' or 'arm64', as returned by Get-Arch.
    .OUTPUTS
        Hashtable with keys: Version, ZipUrl, ZipName, ChecksumsUrl.
    #>
    param(
        [Parameter(Mandatory)]
        [ValidateSet('amd64', 'arm64')]
        [string]$Arch
    )

    $apiUrl = "https://api.github.com/repos/$script:Repo/releases/latest"
    $release = Invoke-RestMethod -Uri $apiUrl -Headers @{ 'User-Agent' = 'ghost-install-script' }
    $version = $release.tag_name -replace '^v', ''

    $zipName = "ghost_${version}_windows_${Arch}.zip"
    $asset = $release.assets | Where-Object { $_.name -eq $zipName }
    if (-not $asset) {
        throw "Release $($release.tag_name) has no asset named $zipName"
    }
    $checksumsAsset = $release.assets | Where-Object { $_.name -eq 'checksums.txt' }
    if (-not $checksumsAsset) {
        throw "Release $($release.tag_name) has no checksums.txt asset"
    }

    return @{
        Version      = $version
        ZipUrl       = $asset.browser_download_url
        ZipName      = $zipName
        ChecksumsUrl = $checksumsAsset.browser_download_url
    }
}
```

- [ ] **Step 2: Review against contract**

Confirm: throws (does not silently return partial data) if either the
versioned zip asset or `checksums.txt` is missing from the release. Confirm
the zip filename exactly matches goreleaser's `name_template`:
`{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}` →
`ghost_<version>_windows_<arch>.zip` — cross-checked against
`.goreleaser.yml:16-22` in this repo. Manual review only; no `pwsh` available
to execute this against the live GitHub API in this environment.

- [ ] **Step 3: Commit**

```bash
git add install.ps1
git commit -m "feat: resolve latest release info in install.ps1"
```

---

### Task 3: Download and checksum verification

**Files:**
- Modify: `install.ps1`

- [ ] **Step 1: Add `Test-Checksum`**

```powershell
function Test-Checksum {
    <#
    .SYNOPSIS
        Verifies a file's SHA256 against the matching line in a
        goreleaser checksums.txt.
    .PARAMETER FilePath
        Path to the downloaded file to verify.
    .PARAMETER FileName
        The name as it appears in checksums.txt (e.g. ghost_1.2.3_windows_amd64.zip).
    .PARAMETER ChecksumsPath
        Path to the downloaded checksums.txt.
    .OUTPUTS
        None. Throws if the checksum is missing or does not match.
    #>
    param(
        [Parameter(Mandatory)][string]$FilePath,
        [Parameter(Mandatory)][string]$FileName,
        [Parameter(Mandatory)][string]$ChecksumsPath
    )

    $line = Get-Content $ChecksumsPath | Where-Object { $_ -match [regex]::Escape($FileName) }
    if (-not $line) {
        throw "No checksum entry found for $FileName in checksums.txt"
    }
    $expected = ($line -split '\s+')[0].ToLowerInvariant()
    $actual = (Get-FileHash -Path $FilePath -Algorithm SHA256).Hash.ToLowerInvariant()

    if ($expected -ne $actual) {
        throw "Checksum mismatch for $FileName`: expected $expected, got $actual"
    }
}
```

- [ ] **Step 2: Add the download orchestration in `Install-Ghost`'s download phase**

```powershell
function Get-GhostRelease {
    <#
    .SYNOPSIS
        Downloads the release zip and checksums.txt into a fresh temp
        directory and verifies the zip's checksum.
    .PARAMETER ReleaseInfo
        Hashtable as returned by Get-LatestReleaseInfo.
    .OUTPUTS
        String: path to the temp directory containing the verified zip.
    #>
    param(
        [Parameter(Mandatory)][hashtable]$ReleaseInfo
    )

    $tempDir = Join-Path $env:TEMP "ghost-install-$([guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Path $tempDir -Force | Out-Null

    $zipPath = Join-Path $tempDir $ReleaseInfo.ZipName
    $checksumsPath = Join-Path $tempDir 'checksums.txt'

    Invoke-WebRequest -Uri $ReleaseInfo.ZipUrl -OutFile $zipPath
    Invoke-WebRequest -Uri $ReleaseInfo.ChecksumsUrl -OutFile $checksumsPath

    Test-Checksum -FilePath $zipPath -FileName $ReleaseInfo.ZipName -ChecksumsPath $checksumsPath

    return @{ TempDir = $tempDir; ZipPath = $zipPath }
}
```

- [ ] **Step 3: Review against contract**

Confirm: `Test-Checksum` throws on both "no matching line" and "hash
mismatch" — it never returns a boolean that a caller could ignore. Confirm
`Get-GhostRelease` calls `Test-Checksum` before returning, so no caller can
receive an unverified zip path. Manual review only.

- [ ] **Step 4: Commit**

```bash
git add install.ps1
git commit -m "feat: download and verify release checksum in install.ps1"
```

---

### Task 4: Extract and install the binary (idempotent)

**Files:**
- Modify: `install.ps1`

- [ ] **Step 1: Add `Install-Ghost`**

```powershell
function Install-Ghost {
    <#
    .SYNOPSIS
        Extracts ghost.exe from a verified zip into the install directory,
        overwriting any existing binary.
    .PARAMETER ZipPath
        Path to the verified release zip.
    .PARAMETER Destination
        Directory to install ghost.exe into (created if missing).
    .OUTPUTS
        String: full path to the installed ghost.exe.
    #>
    param(
        [Parameter(Mandatory)][string]$ZipPath,
        [Parameter(Mandatory)][string]$Destination
    )

    New-Item -ItemType Directory -Path $Destination -Force | Out-Null

    $extractDir = Join-Path ([System.IO.Path]::GetDirectoryName($ZipPath)) 'extracted'
    Expand-Archive -Path $ZipPath -DestinationPath $extractDir -Force

    $exePath = Join-Path $extractDir 'ghost.exe'
    if (-not (Test-Path $exePath)) {
        throw "ghost.exe not found in extracted archive at $exePath"
    }

    $destExe = Join-Path $Destination 'ghost.exe'
    Copy-Item -Path $exePath -Destination $destExe -Force

    return $destExe
}
```

- [ ] **Step 2: Review against contract**

Confirm: `-Force` on both `Expand-Archive` and `Copy-Item` makes re-running
the installer an overwrite, not an error, satisfying the spec's "idempotent
— always overwrite with latest" decision. Confirm a missing `ghost.exe` in
the archive throws rather than silently installing nothing. Manual review
only.

- [ ] **Step 3: Commit**

```bash
git add install.ps1
git commit -m "feat: extract and install ghost.exe idempotently"
```

---

### Task 5: Persistent PATH update via registry

**Files:**
- Modify: `install.ps1`

- [ ] **Step 1: Add the `WM_SETTINGCHANGE` broadcast helper and `Add-UserPathEntry`**

```powershell
function Broadcast-EnvironmentChange {
    <#
    .SYNOPSIS
        Broadcasts WM_SETTINGCHANGE so already-open processes (e.g. Explorer)
        pick up the updated environment. Does not affect already-open shells'
        own process environment blocks — a new terminal is still required for
        those.
    #>
    $signature = @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Auto)]
public static extern IntPtr SendMessageTimeout(
    IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam,
    uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
    Add-Type -MemberDefinition $signature -Namespace Win32Native -Name User32 -ErrorAction SilentlyContinue

    $HWND_BROADCAST = [IntPtr]0xffff
    $WM_SETTINGCHANGE = 0x1a
    $SMTO_ABORTIFHUNG = 0x2
    $result = [UIntPtr]::Zero
    [Win32Native.User32]::SendMessageTimeout(
        $HWND_BROADCAST, $WM_SETTINGCHANGE, [UIntPtr]::Zero, 'Environment',
        $SMTO_ABORTIFHUNG, 5000, [ref]$result) | Out-Null
}

function Add-UserPathEntry {
    <#
    .SYNOPSIS
        Adds a directory to the persistent per-user PATH via the registry,
        avoiding setx's 1024-char truncation. No-op if already present.
    .PARAMETER Directory
        The directory to add.
    .OUTPUTS
        Boolean: $true if PATH was modified, $false if the entry already existed.
    #>
    param(
        [Parameter(Mandatory)][string]$Directory
    )

    $envKey = 'Registry::HKEY_CURRENT_USER\Environment'
    $current = (Get-ItemProperty -Path $envKey -Name 'Path' -ErrorAction SilentlyContinue).Path
    if (-not $current) { $current = '' }

    $entries = $current -split ';' | Where-Object { $_ -ne '' }
    $alreadyPresent = $entries | Where-Object { $_.TrimEnd('\') -ieq $Directory.TrimEnd('\') }
    if ($alreadyPresent) {
        return $false
    }

    $newValue = if ($current -eq '') { $Directory } else { "$current;$Directory" }
    Set-ItemProperty -Path $envKey -Name 'Path' -Value $newValue -Type ExpandString

    Broadcast-EnvironmentChange

    return $true
}
```

- [ ] **Step 2: Review against contract**

Confirm: reads and writes `HKCU\Environment` directly (registry cmdlets), not
`setx` — no 1024-char truncation risk. Confirm the existing-entry check is
case-insensitive and trailing-backslash-tolerant, so re-running the installer
doesn't append duplicate PATH entries. Confirm `-Type ExpandString` preserves
the `REG_EXPAND_SZ` type so any `%VAR%` references already in the user's PATH
keep expanding correctly. Manual review only — no Windows registry available
in this Linux dev environment to execute against.

- [ ] **Step 3: Commit**

```bash
git add install.ps1
git commit -m "feat: add registry-based persistent PATH update"
```

---

### Task 6: Top-level orchestration, error handling, and cleanup

**Files:**
- Modify: `install.ps1`

- [ ] **Step 1: Add the `main` orchestration block**

```powershell
function Main {
    $tempDir = $null
    try {
        Write-Host 'Detecting architecture...'
        $arch = Get-Arch
        Write-Host "  -> $arch"

        Write-Host 'Resolving latest release...'
        $release = Get-LatestReleaseInfo -Arch $arch
        Write-Host "  -> ghost $($release.Version)"

        Write-Host 'Downloading and verifying...'
        $downloaded = Get-GhostRelease -ReleaseInfo $release
        $tempDir = $downloaded.TempDir
        Write-Host '  -> checksum OK'

        Write-Host "Installing to $script:InstallDir..."
        $exePath = Install-Ghost -ZipPath $downloaded.ZipPath -Destination $script:InstallDir
        Write-Host "  -> installed $exePath"

        Write-Host 'Updating PATH...'
        $changed = Add-UserPathEntry -Directory $script:InstallDir
        if ($changed) {
            Write-Host '  -> added to user PATH'
        } else {
            Write-Host '  -> already on PATH'
        }

        Write-Host ''
        Write-Host 'ghost installed successfully.'
        Write-Host 'Open a new terminal, then run: ghost mcp init'
    }
    catch {
        Write-Error "Install failed: $($_.Exception.Message)"
        exit 1
    }
    finally {
        if ($tempDir -and (Test-Path $tempDir)) {
            Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

if ($MyInvocation.InvocationName -ne '.') {
    Main
}
```

- [ ] **Step 2: Review against contract**

Confirm: every function called in `Main` that can fail (`Get-Arch`,
`Get-LatestReleaseInfo`, `Get-GhostRelease` → `Test-Checksum`,
`Install-Ghost`) throws rather than returning a sentinel, so the single
`try/catch` in `Main` catches all of them uniformly and exits non-zero.
Confirm the `finally` block always attempts temp-dir cleanup, including on
the failure path. Confirm the `if ($MyInvocation.InvocationName -ne '.')`
guard means dot-sourcing the script (`. .\install.ps1`) loads the functions
without running `Main` — this is what makes the functions reviewable/callable
in isolation despite no test runner being available. Manual review only.

- [ ] **Step 3: Commit**

```bash
git add install.ps1
git commit -m "feat: wire up install.ps1 main orchestration and error handling"
```

---

### Task 7: README documentation

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add a Windows install section**

Find the existing install instructions in `README.md` (the `go install` /
`make install` section) and add a Windows subsection immediately after it:

````markdown
### Windows

```powershell
irm https://raw.githubusercontent.com/wcatz/ghost/main/install.ps1 | iex
```

This downloads the latest release, verifies its checksum, installs
`ghost.exe` to `%LOCALAPPDATA%\ghost\bin`, and adds that directory to your
user PATH. Open a new terminal afterward, then run `ghost mcp init`.

Re-running the command upgrades an existing install in place.
````

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: document Windows install.ps1 one-command install"
```

---

### Task 8: Manual end-to-end verification (Windows required)

**Files:** none (verification only)

- [ ] **Step 1: Run on a real Windows environment**

On a Windows machine (e.g. the UTM VM used for Skinner's testing), with no
prior ghost install:

```powershell
irm https://raw.githubusercontent.com/<branch-or-fork-url>/install.ps1 | iex
```

(Use a `raw.githubusercontent.com` URL pointing at this branch, or copy the
script locally and run `.\install.ps1`, since `main` won't have this script
until the PR merges.)

Confirm:
- Script reports the detected arch, resolved version, checksum OK, install
  path, and PATH-updated message with no errors.
- `ghost.exe` exists at `%LOCALAPPDATA%\ghost\bin\ghost.exe`.
- **In a newly opened terminal**, `ghost version` resolves and runs
  (confirms PATH took effect).

- [ ] **Step 2: Re-run to verify idempotent upgrade**

Run the same command again in the same terminal session used for Step 1's
new terminal. Confirm:
- No duplicate PATH entries are added (inspect `[Environment]::GetEnvironmentVariable('Path', 'User')`
  and confirm `%LOCALAPPDATA%\ghost\bin` appears exactly once).
- The binary is overwritten without any prompt or error.

- [ ] **Step 3: Report results back**

This step cannot be performed by the implementing agent — no Windows
environment is available in this Linux dev environment. Report pass/fail and
any error output from Steps 1-2 to the user (or file a follow-up issue if a
step fails) before considering #257/#258 closed.

---

## Plan self-review notes

- **Spec coverage:** distribution (Task 7 doc + repo-root file per spec),
  arch detection (Task 1), latest-release resolution (Task 2), checksum
  verification against goreleaser's checksums.txt (Task 3), install to
  `%LOCALAPPDATA%\ghost\bin` idempotently (Task 4), registry-based PATH
  update avoiding setx (Task 5), error handling + cleanup (Task 6), no
  install.sh / no auto mcp init / no package manager manifest (none added,
  matches spec's explicit non-goals).
- **Manual-verification caveat carried through every task**, not just
  mentioned once at the top — each task's review step explicitly says
  "manual review only, no pwsh available" rather than implying automated
  test coverage exists.
- Task 8 is explicitly flagged as requiring the user's Windows environment —
  the implementing agent cannot self-certify this plan's real-world
  correctness, only its internal logical consistency.

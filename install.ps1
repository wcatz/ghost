#Requires -Version 5.1
<#
.SYNOPSIS
    Installs the latest ghost release and adds it to the user PATH.
.DESCRIPTION
    Downloads the latest windows/amd64 or windows/arm64 ghost release from
    GitHub, verifies its SHA256 against the release's checksums.txt, verifies the
    release's build attestation, extracts ghost.exe into
    %LOCALAPPDATA%\ghost\bin, and adds that directory to the persistent user PATH
    (via the registry, not setx).

    The checksum is an integrity check: checksums.txt is published in the same
    release as the archive it vouches for, so anyone able to replace the archive
    can replace the manifest too. The attestation is the check that says which
    workflow built the bytes, and it is what `ghost upgrade` has enforced since
    v0.43.0 (see docs/cli.md).

    The attestation is verified by `gh attestation verify`, so the GitHub CLI must
    be installed and logged in (`gh auth login`). Nothing is downloaded or
    installed unless that check runs, unless the release predates attestations
    entirely, or unless -SkipAttestation (or $env:GHOST_SKIP_ATTESTATION) is
    given — see Resolve-AttestationDecision for exactly what each of those means.
#>
[CmdletBinding()]
param(
    # Proceed when the attestation could not be CHECKED — no gh on PATH, or gh
    # not logged in. It does not override an attestation that was checked and
    # did not verify: that case is fatal with no flag, because an attacker who can
    # mint one bundle of their own would otherwise be handed the whole feature.
    [switch]$SkipAttestation
)

$ErrorActionPreference = 'Stop'

[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$script:Repo = 'wcatz/ghost'
# The workflow `actions/attest-build-provenance` runs in, as a repository-relative
# path. The certificate identity this script requires is this path joined to the
# GitHub URL, plus the ref, so the three statements of the policy — repository,
# workflow, tag — are one string that cannot be half-right. It is the same
# identity selfupdate.ReleaseWorkflowIdentity builds, and the reason both live in
# a constant rather than being written out twice.
$script:ReleaseWorkflow = '.github/workflows/release.yml'
# The first release the workflow attested. Same constant as
# selfupdate.FirstAttestedVersion, and for the same reason: a boundary that is
# computed drifts, and a release older than the boundary cannot have an
# attestation because the step that mints one did not run yet.
$script:FirstAttestedVersion = '0.43.0'

function Get-InstallDir {
    <#
    .SYNOPSIS
        Where ghost.exe is installed, computed on use rather than at parse time.
    .DESCRIPTION
        Not a top-level constant, because %LOCALAPPDATA% has to exist before the
        script can be DOT-SOURCED — and dot-sourcing is a supported mode here, the
        guard at the bottom of this file exists to make it one. Resolving it while
        the file is being read made dot-sourcing fail on a machine where the
        variable is unset, before any of the attestation logic could be called.
    #>
    return Join-Path $env:LOCALAPPDATA 'ghost\bin'
}

function Get-Arch {
    <#
    .SYNOPSIS
        Maps the running process architecture to a goreleaser arch string.
    .OUTPUTS
        String: 'amd64' or 'arm64'. Throws on any other architecture.
    #>
    $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
    switch ($arch) {
        'X64' { return 'amd64' }
        'Arm64' { return 'arm64' }
        default {
            throw "Unsupported architecture: $arch. ghost only ships windows/amd64 and windows/arm64 builds."
        }
    }
}

function Get-LatestReleaseInfo {
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

function Test-Checksum {
    param(
        [Parameter(Mandatory)][string]$FilePath,
        [Parameter(Mandatory)][string]$FileName,
        [Parameter(Mandatory)][string]$ChecksumsPath
    )

    $line = Get-Content $ChecksumsPath | Where-Object { $_ -match "\s+$([regex]::Escape($FileName))$" }
    if (@($line).Count -ne 1) {
        throw "Expected exactly one checksum entry for $FileName, found $(@($line).Count)"
    }
    $expected = ($line -split '\s+')[0].ToLowerInvariant()
    $actual = (Get-FileHash -Path $FilePath -Algorithm SHA256).Hash.ToLowerInvariant()

    if ($expected -ne $actual) {
        throw "Checksum mismatch for $FileName`: expected $expected, got $actual"
    }
}

# --- Build attestation ------------------------------------------------------
#
# The same three questions `ghost upgrade` asks, of the same evidence, and with
# the same answers. Repository, workflow and tag are all required, and the
# check runs after the download and before the archive is unpacked, so nothing
# nobody has vouched for is ever handed to Expand-Archive.

function Get-ComparableVersion {
    <#
    .SYNOPSIS
        The numeric core of a version, or $null when it has none.
    .DESCRIPTION
        Strips a leading v and any prerelease suffix, because the boundary is a
        property of the RELEASE LINE: 0.43.0-rc.1 is older than 0.43.0 and newer
        than 0.42.9, so a prerelease of the first attested version is the first
        attested version. $null is returned rather than a guess for anything that
        is not dotted digits, and a caller that gets $null must treat the version
        as requiring an attestation — "cannot tell" must never read as "old enough
        to skip", which is the same rule selfupdate.AttestationRequiredFor states.

        Deliberately NOT [Parameter(Mandatory)]: an empty string is a real input
        here, not a caller mistake, and it means exactly what `dev` means. A
        mandatory binding turned it into a terminating parameter error instead —
        fail-closed by accident, with a message that says nothing about a
        version, on the one path that is supposed to explain itself.
    #>
    param([AllowEmptyString()][string]$Version)

    if ([string]::IsNullOrWhiteSpace($Version)) { return $null }
    $core = (($Version -replace '^v', '') -split '-')[0]
    if ($core -notmatch '^\d+(\.\d+)*$') { return $null }
    return $core
}

function Compare-CoreVersion {
    <#
    .SYNOPSIS
        Compares two dotted numeric versions, or returns $null if either has none.
    .DESCRIPTION
        The $null tests are explicit rather than left to a count, because `$null -split`
        answers with a ONE-element array holding an empty string, so a length check
        passes on a version that has no numeric core at all and every later
        comparison quietly treats it as 0.0.0. That turned "dev" — which cannot be
        ordered, and must therefore be treated as REQUIRING an attestation — into
        a version older than the boundary, and skipped the check.

        AllowEmptyString here for the same reason as below: an unorderable version
        is an input this function is asked to classify, not a caller error.
    #>
    param(
        [AllowEmptyString()][string]$Left,
        [AllowEmptyString()][string]$Right
    )

    $lc = Get-ComparableVersion $Left
    $rc = Get-ComparableVersion $Right
    if ([string]::IsNullOrEmpty($lc) -or [string]::IsNullOrEmpty($rc)) { return $null }

    $l = $lc -split '\.'
    $r = $rc -split '\.'
    for ($i = 0; $i -lt [Math]::Max($l.Count, $r.Count); $i++) {
        # A missing segment is zero, so 0.43 and 0.43.0 are the same version
        # rather than an error.
        $lv = if ($i -lt $l.Count) { [int]$l[$i] } else { 0 }
        $rv = if ($i -lt $r.Count) { [int]$r[$i] } else { 0 }
        if ($lv -ne $rv) { return [Math]::Sign($lv - $rv) }
    }
    return 0
}

function Test-AttestationRequired {
    <#
    .SYNOPSIS
        Whether a release must carry a build attestation.
    .DESCRIPTION
        Not [Parameter(Mandatory)], for the reason Get-ComparableVersion gives: a
        version that cannot be ordered is the fail-closed case, so it has to be
        answerable rather than a parameter-binding failure.
    #>
    param([AllowEmptyString()][string]$Version)

    $cmp = Compare-CoreVersion $Version $script:FirstAttestedVersion
    if ($null -eq $cmp) { return $true }
    return ($cmp -ge 0)
}

function Get-AttestationIdentity {
    <#
    .SYNOPSIS
        The exact identity the release workflow's attestation must carry.
    .DESCRIPTION
        The Fulcio certificate GitHub mints for a workflow records the workflow's
        own URL in a URI SAN, plus the ref it ran on:

            https://github.com/OWNER/REPO/PATH@refs/tags/vX.Y.Z

        so that single string pins all three of the policy's statements at once.
        They cannot be pinned separately, and gh attestation verify says so: the
        --cert-identity, --signer-repo and --signer-workflow flags are one
        exclusive group, so naming a workflow AND pinning the tag is not a thing
        the flag set can express. The tag is therefore inside the SAN, which is
        also the only way it gets pinned at all — --signer-workflow matches a path
        and would accept the same workflow run from a branch.
    #>
    param([Parameter(Mandatory)][string]$Version)

    $tag = 'v' + ((Get-ComparableVersion $Version) ?? ($Version -replace '^v', ''))
    return [ordered]@{
        Repo         = $script:Repo
        ReleaseTag   = $tag
        CertIdentity = "https://github.com/$($script:Repo)/$($script:ReleaseWorkflow)@refs/tags/$tag"
    }
}

function Get-AttestationVerdict {
    <#
    .SYNOPSIS
        What the attestation check established: Verified, Skipped, Unverifiable
        or Unchecked.
    .DESCRIPTION
        $Probe and $Run are injected so this is testable without gh and without a
        network, the same way internal/selfupdate's tests inject a local release
        server. $Probe is given a command name and returns its full path or $null;
        $Run is given a path, an argument array and a working directory, and
        returns @{ ExitCode; Output }.

        The three-way split is the whole design, and it is the same split
        ghost upgrade makes:

          Verified       — gh checked the bundle and it is this repository's
                           release workflow on this tag.
          Skipped        — the release predates attestations, so none can exist.
          Unverifiable   — gh checked, and the attestation did not verify. FATAL,
                           and no override reaches it.
          Unchecked      — there was no way to check: no gh, or gh not logged in.
                           A property of the MACHINE, not of the archive, which is
                           why this is the only case an override reaches.

        gh attestation verify answers "did not verify" and "nothing to verify"
        with the same exit status, so the script does not try to tell those two
        apart: a non-zero exit from an authenticated gh is Unverifiable, and the
        only distinction it draws is one it can establish for itself, which is
        whether it has a verifier at all.
    #>
    param(
        [Parameter(Mandatory)][string]$FilePath,
        [Parameter(Mandatory)][string]$Version,
        [Parameter(Mandatory)][scriptblock]$Probe,
        [Parameter(Mandatory)][scriptblock]$Run
    )

    if (-not (Test-AttestationRequired $Version)) {
        return @{
            State  = 'Skipped'
            Reason = "$Version predates the first attested release ($($script:FirstAttestedVersion)), so it has no attestation and none is required"
        }
    }

    $identity = Get-AttestationIdentity $Version
    $gh = & $Probe 'gh'
    if (-not $gh) {
        return @{
            State  = 'Unchecked'
            Reason = 'the GitHub CLI (gh) is not installed, and it is what verifies a build attestation'
        }
    }

    $auth = & $Run $gh @('auth', 'status') $null
    if ($auth.ExitCode -ne 0) {
        # Reported as Unchecked rather than Unverifiable because nothing has been
        # said about the archive: gh would not answer. A token is needed even for
        # a public repository, which is gh's own behaviour and not something this
        # script can work around without a second verifier.
        return @{
            State  = 'Unchecked'
            Reason = 'gh is installed but not authenticated; run `gh auth login` to verify the attestation'
        }
    }

    $result = & $Run $gh @(
        'attestation', 'verify', $FilePath,
        '--repo', $identity.Repo,
        '--cert-identity', $identity.CertIdentity
    ) $null

    if ($result.ExitCode -eq 0) {
        return @{
            State  = 'Verified'
            Reason = "the build attestation is signed by $($identity.Repo)/$($script:ReleaseWorkflow) on $($identity.ReleaseTag)"
        }
    }

    $detail = ($result.Output -replace '\s+', ' ').Trim()
    if ($detail.Length -gt 400) { $detail = $detail.Substring(0, 400) + '...' }
    return @{
        State  = 'Unverifiable'
        Reason = "the build attestation did not verify against $($identity.CertIdentity). gh said: $detail"
    }
}

function Resolve-AttestationDecision {
    <#
    .SYNOPSIS
        Whether a verdict permits the install, and what to say about it.
    .DESCRIPTION
        Returns @{ Allow; Severity; Message }. The asymmetry is deliberate and is
        the same one ghost upgrade draws with --allow-unattested: the flag means
        "nobody could be asked", never "the attestation did not check out". An
        attacker who can publish one bundle of their own would otherwise be
        handed the entire feature, so a checked-and-refused attestation is fatal
        and its message names no flag — there is no flag to name.
    #>
    param(
        [Parameter(Mandatory)][hashtable]$Verdict,
        [bool]$Skip
    )

    switch ($Verdict.State) {
        'Verified' {
            return @{ Allow = $true; Severity = 'info'; Message = "  -> $($Verdict.Reason)" }
        }
        'Skipped' {
            # Surfaced, not silent. A pre-cutover release is allowed because
            # nothing could vouch for it, which is a different fact from "something
            # vouched for it", and only the first is true.
            return @{ Allow = $true; Severity = 'info'; Message = "  -> $($Verdict.Reason)" }
        }
        'Unverifiable' {
            return @{
                Allow    = $false
                Severity = 'error'
                Message  = "$($Verdict.Reason). Nothing is installed: an archive whose attestation was checked and did not verify is refused, and there is no flag that overrides it, because anything that could publish a bundle of its own could otherwise replace ghost."
            }
        }
        'Unchecked' {
            if ($Skip) {
                return @{
                    Allow    = $true
                    Severity = 'warning'
                    Message  = "warning: installing WITHOUT a verified build attestation: $($Verdict.Reason). The checksum was checked, but checksums.txt comes from the same release as the archive, so nothing here says which workflow built these bytes."
                }
            }
            return @{
                Allow    = $false
                Severity = 'error'
                Message  = "$($Verdict.Reason), so nothing says which workflow built this archive. Install the GitHub CLI and run 'gh auth login', then run this script again; or pass -SkipAttestation (or set `$env:GHOST_SKIP_ATTESTATION=1) to install anyway, which accepts the archive on the strength of a checksum from the same release and nothing more."
            }
        }
        default {
            return @{
                Allow    = $false
                Severity = 'error'
                Message  = "the attestation check reported an unrecognised state '$($Verdict.State)', which is refused rather than guessed at."
            }
        }
    }
}

function Get-GhostRelease {
    param(
        [Parameter(Mandatory)][hashtable]$ReleaseInfo
    )

    $tempDir = Join-Path $env:TEMP "ghost-install-$([guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Path $tempDir -Force | Out-Null

    $zipPath = Join-Path $tempDir $ReleaseInfo.ZipName
    $checksumsPath = Join-Path $tempDir 'checksums.txt'

    $previousProgressPreference = $ProgressPreference
    $ProgressPreference = 'SilentlyContinue'
    try {
        try {
            Invoke-WebRequest -Uri $ReleaseInfo.ZipUrl -OutFile $zipPath -UseBasicParsing
            Invoke-WebRequest -Uri $ReleaseInfo.ChecksumsUrl -OutFile $checksumsPath -UseBasicParsing
        }
        finally {
            $ProgressPreference = $previousProgressPreference
        }

        Test-Checksum -FilePath $zipPath -FileName $ReleaseInfo.ZipName -ChecksumsPath $checksumsPath
    }
    catch {
        Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
        throw
    }

    return @{ TempDir = $tempDir; ZipPath = $zipPath }
}

function Install-Ghost {
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
    $oldExe = "$destExe.old"
    $hadExisting = Test-Path $destExe
    if ($hadExisting) {
        Move-Item -Path $destExe -Destination $oldExe -Force
    }
    try {
        Copy-Item -Path $exePath -Destination $destExe -Force
    }
    catch {
        if ($hadExisting -and (Test-Path $oldExe)) {
            Move-Item -Path $oldExe -Destination $destExe -Force
        }
        throw
    }
    Remove-Item -Path $oldExe -Force -ErrorAction SilentlyContinue

    return $destExe
}

function Broadcast-EnvironmentChange {
    $signature = @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Auto)]
public static extern IntPtr SendMessageTimeout(
    IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam,
    uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
    if (-not ([System.Management.Automation.PSTypeName]'Win32Native.User32').Type) {
        Add-Type -MemberDefinition $signature -Namespace Win32Native -Name User32
    }

    $HWND_BROADCAST = [IntPtr]0xffff
    $WM_SETTINGCHANGE = 0x1a
    $SMTO_ABORTIFHUNG = 0x2
    $result = [UIntPtr]::Zero
    [Win32Native.User32]::SendMessageTimeout(
        $HWND_BROADCAST, $WM_SETTINGCHANGE, [UIntPtr]::Zero, 'Environment',
        $SMTO_ABORTIFHUNG, 5000, [ref]$result) | Out-Null
}

function Add-UserPathEntry {
    param(
        [Parameter(Mandatory)][string]$Directory
    )

    try {
        $envRegKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
        $current = $envRegKey.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        if (-not $current) { $current = '' }

        $entries = $current -split ';' | Where-Object { $_ -ne '' }
        $normalizedDirectory = [Environment]::ExpandEnvironmentVariables($Directory).TrimEnd('\')
        $alreadyPresent = $entries | Where-Object {
            [Environment]::ExpandEnvironmentVariables($_).TrimEnd('\') -ieq $normalizedDirectory
        }
        if ($alreadyPresent) {
            return $false
        }

        $newValue = if ($current -eq '') { $Directory } else { "$current;$Directory" }
        $envRegKey.SetValue('Path', $newValue, [Microsoft.Win32.RegistryValueKind]::ExpandString)
    }
    catch {
        throw "Failed to update user PATH in the registry: $($_.Exception.Message)"
    }
    finally {
        if ($envRegKey) { $envRegKey.Dispose() }
    }

    Broadcast-EnvironmentChange

    return $true
}

function Find-Command {
    <#
    .SYNOPSIS
        The full path of a command, or $null. Never throws.
    #>
    param([Parameter(Mandatory)][string]$Name)

    $cmd = Get-Command $Name -CommandType Application -ErrorAction SilentlyContinue
    if ($null -eq $cmd) { return $null }
    if ($cmd -is [System.Array]) { $cmd = $cmd[0] }
    return $cmd.Source
}

function Invoke-NativeCommand {
    <#
    .SYNOPSIS
        Runs a command and returns its exit status and combined output.
    .DESCRIPTION
        $ErrorActionPreference is 'Stop' for the whole script, and under Windows
        PowerShell 5.1 a native command writing to stderr can raise a terminating
        error in that mode — which would turn gh's ordinary "this did not verify"
        output into an exception that says so much less. So it is relaxed for the
        duration of the call and restored after, and the exit status is read from
        $LASTEXITCODE rather than inferred.
    #>
    param(
        [Parameter(Mandatory)][string]$Exe,
        [Parameter(Mandatory)][string[]]$Arguments
    )

    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $output = & $Exe @Arguments 2>&1 | Out-String
        return @{ ExitCode = $LASTEXITCODE; Output = $output }
    }
    finally {
        $ErrorActionPreference = $previous
    }
}

function Test-BuildAttestation {
    <#
    .SYNOPSIS
        The check as production wires it, with the real gh.
    #>
    param(
        [Parameter(Mandatory)][string]$FilePath,
        [Parameter(Mandatory)][string]$Version
    )

    $probe = { param($Name) Find-Command $Name }
    $run = { param($Exe, $Arguments, $WorkDir) Invoke-NativeCommand -Exe $Exe -Arguments $Arguments }
    return Get-AttestationVerdict -FilePath $FilePath -Version $Version -Probe $probe -Run $run
}

function Test-AttestationOverridden {
    <#
    .SYNOPSIS
        Whether the operator asked to proceed without a checked attestation.
    .DESCRIPTION
        A named function rather than an inline `-or` in Main, because the second
        half of it is only observable through the environment and Main cannot run
        in a test — it reaches the network, the registry and a real install. The
        env var exists at all because the documented way to run this script is
        `iwr ... | iex`, where a parameter cannot be passed at all, so a
        switch that is the ONLY way to override would be unusable for the users
        most likely to want it.
    #>
    param([switch]$Skip)

    if ($Skip) { return $true }
    if ([string]::IsNullOrWhiteSpace($env:GHOST_SKIP_ATTESTATION)) { return $false }
    return @('1', 'true', 'yes', 'on') -contains $env:GHOST_SKIP_ATTESTATION.Trim().ToLowerInvariant()
}

function Main {
    param([switch]$SkipAttestation)

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

        # After the download, because gh keys the lookup on the digest of the
        # bytes that arrived; before the unpack, so nothing nobody has vouched for
        # is ever handed to a decompressor. Both ends are the same trade ghost
        # upgrade makes, for the same reason.
        $verdict = Test-BuildAttestation -FilePath $downloaded.ZipPath -Version $release.Version
        $decision = Resolve-AttestationDecision -Verdict $verdict -Skip (Test-AttestationOverridden -Skip:$SkipAttestation)
        switch ($decision.Severity) {
            'warning' { Write-Warning $decision.Message }
            'error' { throw $decision.Message }
            default { Write-Host $decision.Message }
        }
        if (-not $decision.Allow) { throw $decision.Message }

        Write-Host "Installing to $(Get-InstallDir)..."
        $exePath = Install-Ghost -ZipPath $downloaded.ZipPath -Destination (Get-InstallDir)
        Write-Host "  -> installed $exePath"

        Write-Host 'Updating PATH...'
        $changed = Add-UserPathEntry -Directory (Get-InstallDir)
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
        Write-Error "Install failed: $($_.Exception.Message)" -ErrorAction Continue
        if ($PSCommandPath) {
            exit 1
        }
        return
    }
    finally {
        if ($tempDir -and (Test-Path $tempDir)) {
            Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

if ($MyInvocation.InvocationName -ne '.') {
    Main -SkipAttestation:$SkipAttestation
}

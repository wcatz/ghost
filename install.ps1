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
    # -creplace, NOT -replace, and this is the site that actually mattered. The
    # version derived here is what Main hands to the attestation check, so a
    # case-insensitive strip turned a tag of V0.42.9 into 0.42.9 on the way in —
    # an older version than the cutover, so the check was SKIPPED, while
    # `ghost upgrade` called the same tag unparseable and required an attestation.
    # Get-ComparableVersion is case-sensitive for the same reason; the two have to
    # agree, and they can only agree if the capital survives all the way here.
    $version = $release.tag_name -creplace '^v', ''

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
        The numeric core of a version, or $null when it is not a version this
        repository recognises.
    .DESCRIPTION
        A mirror of selfupdate.parseVersion and selfupdate.coreVersion, and the
        two must agree on EVERY input or the boundary fails in one direction only —
        the dangerous one. Where they once disagreed, the script said a release
        needed no attestation and the client said it did, and the release
        installed from a checksum alone:

            0.42.9.1   ->  script: not required    client: required
            0.042.9    ->  script: not required    client: required

        Both are UNPARSEABLE, not merely unusual, and an unparseable version is
        REQUIRED — the "cannot tell must never read as old enough to skip" rule.
        So the rules are the client's, exactly:

          - a leading `v` is dropped;
          - build metadata from the FIRST `+` onward is dropped, and it is dropped
            BEFORE the prerelease is split, so `1.0.0-rc.1+build` is `1.0.0` and
            `1.0.0+build-rc` is also `1.0.0`;
          - a prerelease is what follows the FIRST `-`, and it must be a
            dot-separated list of non-empty identifiers of ASCII letters, digits
            and hyphens. An invalid one makes the WHOLE version unparseable —
            including one on an otherwise older line, where dropping it would have
            turned "not a version" into "an old version";
          - EXACTLY three numeric components. Two is ambiguous and four is a
            different thing, and a two-component version in particular is the
            shape that made `1.2` accidentally agree for the wrong reason;
          - each component is `0 | [1-9][0-9]*`. A leading zero is refused, which
            is what separates `0.42.9` from `0.042.9`; a bare `0` is allowed.

        Deliberately NOT [Parameter(Mandatory)]: an empty string is a real input
        here, not a caller mistake, and it means exactly what `dev` means. A
        mandatory binding turned it into a terminating parameter error instead —
        fail-closed by accident, with a message that says nothing about a
        version, on the one path that is supposed to explain itself.
    #>
    param([AllowEmptyString()][string]$Version)

    if ([string]::IsNullOrWhiteSpace($Version)) { return $null }

    # -creplace, NOT -replace. PowerShell's -replace is case-INsensitive, so
    # -replace '^v' also strips an uppercase V and read "V0.42.9" as 0.42.9 — an
    # older version than the cutover, and no attestation. Go uses
    # strings.TrimPrefix(s, "v"), which is case-sensitive, so the client calls
    # "V0.42.9" unparseable and requires one. Same input, opposite answers, and
    # the wrong one is the installer skipping its check.
    $rest = $Version -creplace '^v', ''
    $plus = $rest.IndexOf('+')
    if ($plus -ge 0) { $rest = $rest.Substring(0, $plus) }

    $dash = $rest.IndexOf('-')
    if ($dash -ge 0) {
        $prerelease = $rest.Substring($dash + 1)
        $rest = $rest.Substring(0, $dash)
        if (-not (Test-ValidPrerelease $prerelease)) { return $null }
    }

    $parts = $rest -split '\.'
    if ($parts.Count -ne 3) { return $null }
    foreach ($p in $parts) {
        # \A and \z, not ^ and $: a `$` anchor in .NET also matches just BEFORE a
        # trailing newline, so "9\n" satisfied `^(0|[1-9][0-9]*)$` and [int] then
        # cast "9\n" to 9 without complaint. A tag carrying a newline is not a
        # version, and the client says so.
        # -cnotmatch because -notmatch is case-INsensitive, and a digits-only class
        # is only accidentally immune to that: the case that matters is the
        # prerelease's letters, below.
        if ($p -cnotmatch '\A(0|[1-9][0-9]*)\z') { return $null }
    }
    return ($parts -join '.')
}

function Test-ValidPrerelease {
    <#
    .SYNOPSIS
        Whether s is a dot-separated list of non-empty alphanumeric identifiers.
    .DESCRIPTION
        The only prerelease shape semver defines, and the only one
        selfupdate.validPrerelease accepts: ASCII letters, digits and hyphens, in
        non-empty dot-separated identifiers. An empty identifier, or anything
        outside that set, makes the version unparseable rather than odd.

        The single `+` quantifier is what enforces the non-empty part, and it
        makes two separate emptiness checks redundant — an earlier version had
        one for the whole string and one per identifier, and NEITHER could be
        killed by a mutation, because the identifier regex already rejects both
        cases. `''.Split('.')` is a one-element array holding an empty string, so
        an empty prerelease arrives at the loop and fails there. Two guards that
        cannot disagree with each other are two things to keep in step for
        nothing, so the checks are gone and the comment says why.
    #>
    param([AllowEmptyString()][string]$Prerelease)

    foreach ($id in ($Prerelease -split '\.')) {
        # -cnotmatch, and \A..\z. -notmatch is case-insensitive with .NET Unicode
        # case folding, so '0.42.9-K' (U+212A KELVIN SIGN), '0.42.9-s' (U+017F
        # LONG S) and '0.42.9-I' (U+0130 DOTLESS I) all folded onto ASCII and were
        # accepted — as a prerelease, on an older line, which is a version the
        # check was skipped for. validPrerelease in Go tests the bytes, so all
        # three are unparseable there.
        if ($id -cnotmatch '\A[0-9A-Za-z-]+\z') { return $false }
    }
    return $true
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

        Both sides are EXACTLY three components by the time they arrive —
        Get-ComparableVersion is the only caller path and it refuses anything else —
        so the loop runs three times and indexes directly. The earlier version
        defaulted a missing segment to zero, which is a second way of saying
        "1.2 and 1.2.0 are the same version", and that is exactly the reading the
        client refuses. It was also unreachable-as-a-difference: with three
        components guaranteed, the defaulting branch could never fire, so no test
        could have killed it. It is gone rather than left as a way to reintroduce
        the disagreement.

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
    for ($i = 0; $i -lt 3; $i++) {
        # [long]::TryParse, not a cast. `[int]'3000000000'` THROWS, so a tag with a
        # component past Int32 took the whole installer down with an exception that
        # said nothing about a version — while Go's strconv.Atoi accepts it on a
        # 64-bit platform and calls 3000000000.0.0 newer than the cutover, i.e.
        # REQUIRED. Overflow is the same thing as any other unorderable input here,
        # so it answers $null and the caller requires the attestation. A component
        # too long for Int64 fails TryParse and takes the same path, which is also
        # what strconv.Atoi does with it.
        $lv = 0L
        $rv = 0L
        if (-not [long]::TryParse($l[$i], [ref]$lv)) { return $null }
        if (-not [long]::TryParse($r[$i], [ref]$rv)) { return $null }
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

        And the failure direction is the whole point: $null -eq $cmp means the
        version could not be ordered, and that answers TRUE. Anything that made
        this line answer $false for an unorderable version would install a release
        on a checksum alone, which is what the whole check exists to prevent.
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

        The version core is resolved with an explicit test rather than the `??`
        operator, which is PowerShell 7.0 and later. This script's header declares
        `#Requires -Version 5.1`, `powershell.exe` on Windows is 5.1, and the
        documented way to run this file is `irm … | iex` — which lands in 5.1. A
        7-only operator is a PARSE error, so it fails the whole script rather than
        this one function, and nothing installs at all. The fallback is reachable:
        Test-AttestationRequired fails closed for an unorderable version such as
        `dev`, so exactly the versions with no numeric core are the ones that get
        here.

        AllowEmptyString for the same reason Get-ComparableVersion takes it: an
        unorderable version is an input to classify here, not a caller error, and a
        mandatory binding turned the empty one into a terminating parameter error
        instead of an answer.
    #>
    param([AllowEmptyString()][string]$Version)

    $core = Get-ComparableVersion $Version
    # -creplace for the same reason as in Get-ComparableVersion: this is the
    # fallback for a version with no numeric core, and it must not quietly
    # normalise case on its way into the certificate identity.
    if ([string]::IsNullOrEmpty($core)) { $core = $Version -creplace '^v', '' }
    return [ordered]@{
        Repo         = $script:Repo
        ReleaseTag   = 'v' + $core
        CertIdentity = "https://github.com/$($script:Repo)/$($script:ReleaseWorkflow)@refs/tags/v$core"
    }
}

function Test-GhMachineFailure {
    <#
    .SYNOPSIS
        Whether gh's output says the MACHINE could not reach GitHub, rather than
        that the attestation did not verify.
    .DESCRIPTION
        gh attestation verify exits 1 for both, and there is no exit status that
        separates them — so a script that maps every non-zero exit to "the
        attestation did not verify" reports a DNS failure, a 500, a rate limit, a
        proxy, or a token that expired between two calls, as a fact about the
        ARCHIVE. That is the mistake this repository already made once and wrote
        down: a transient fault reported as a permanent property of a release.

        So the two are told apart, and the direction of a wrong answer is chosen
        deliberately. A recognised machine-side cause is Unchecked — the
        overridable state, and a loud one. Everything else is Unverifiable, so an
        output shape this function has never seen is treated as a REFUSAL rather
        than waved through: the cost of a wrong answer there is an install that
        cannot proceed until whatever is broken is fixed, and the cost in the
        other direction is an archive nobody has vouched for getting installed by
        someone who passed a flag.

        That is the opposite of the fail-open shape a guard script usually has, and
        it is deliberate for the same reason: a pattern that cannot be recognised
        must not be the pattern that lets something through.
    #>
    param([Parameter(Mandatory)][AllowEmptyString()][string]$Output)

    if ([string]::IsNullOrWhiteSpace($Output)) { return $false }
    # Every entry here DOWNGRADES a refusal to the overridable state, so each one
    # has to be a string an attacker cannot put into gh's error text about a
    # certificate. gh quotes attacker-influenced values back in its messages — the
    # certificate's own subject, the repository, the organisation — so a short
    # generic token is a downgrade waiting to be typed. "SSL" and "EOF" were both
    # in an earlier draft of this list and both are gone: "not from a trusted CA"
    # contains one, and any message can be made to contain the other. A bare
    # "proxyconnect" went the same way, because a repository, organisation or
    # workflow name may contain it — so the two-word form a transport failure
    # actually prints is required, and nothing else matches.
    $machineCauses = @(
        'HTTP 429', 'HTTP 500', 'HTTP 502', 'HTTP 503', 'HTTP 504',
        'Bad credentials', 'requires authentication', 'rate limit',
        'dial tcp', 'no such host', 'connection reset', 'connection refused',
        'TLS handshake', 'context deadline', 'i/o timeout', 'no such network',
        'proxyconnect tcp', 'server misbehaving'
    )
    foreach ($cause in $machineCauses) {
        if ($Output.Contains($cause)) { return $true }
    }
    return $false
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
          Unverifiable   — gh completed the check, and the attestation did not
                           hold. FATAL, and no override reaches it.
          Unchecked      — the check could not be run or could not be completed: no
                           gh, gh not logged in, a gh too old to have the
                           subcommand, or GitHub unreachable. A property of the
                           MACHINE, not of the archive, which is why this is the
                           only case an override reaches.

        "Completed the check" is the load-bearing phrase and it is ESTABLISHED,
        not assumed, because gh uses one exit status for "the attestation did not
        verify", "there is no attestation for these bytes", and "I could not reach
        GitHub". Two things establish it: a capability probe, so a gh older than
        the one that added `attestation` is caught before it is asked, and
        Test-GhMachineFailure, which is the only other thing allowed to claim the
        check did not happen — and which returns false for anything it does not
        recognise, so an unknown failure is reported as a refusal.
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

    # The capability probe. A gh from before the `attestation` subcommand existed
    # exits 1 on `gh attestation verify` with `unknown command`, which without this
    # would be a permanent, unoverridable refusal that blames the release for a
    # property of the machine.
    $capability = & $Run $gh @('attestation', 'verify', '--help') $null
    if ($capability.ExitCode -ne 0) {
        return @{
            State  = 'Unchecked'
            Reason = 'this gh is too old to verify attestations: it has no `gh attestation verify` subcommand. Update the GitHub CLI, or run `gh attestation verify` yourself and compare the identity it reports'
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

    if (Test-GhMachineFailure $result.Output) {
        return @{
            State  = 'Unchecked'
            Reason = "gh could not complete the check, so nothing has been said about this archive. gh said: $detail"
        }
    }

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
                Message  = "$($Verdict.Reason), so nothing says which workflow built this archive. Resolve that and run this script again; or pass -SkipAttestation (or set `$env:GHOST_SKIP_ATTESTATION=1) to install anyway, which accepts the archive on the strength of a checksum from the same release and nothing more."
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

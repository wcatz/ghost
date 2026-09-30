# install.ps1's attestation decision, executed.
#
# Run by TestInstallPS1AttestationDecidesTheSameWayGhostUpgradeDoes in
# internal/selfupdate, which supplies the path to install.ps1. It is a plain
# script rather than Pester because Pester is not installed anywhere this
# repository's tests run, and a test that skips itself is not a test.
#
# Nothing here touches the network, a registry, or an installed ghost: every
# function under test takes its outside world as a parameter, and gh is stubbed
# by two scriptblocks. The registry and PATH work in Main are Windows-only and
# are not on this path.

param([Parameter(Mandatory)][string]$Script)

$ErrorActionPreference = 'Stop'

# Dot-sourced, not run: install.ps1's own guard makes dot-sourcing a supported
# mode, and it is the only way to get at its functions without installing ghost.
. $Script

$script:Failures = @()
$script:Checks = 0

function Check {
    param([string]$Name, [bool]$Ok, [string]$Detail = '')
    $script:Checks++
    if (-not $Ok) {
        $script:Failures += "$Name$(if ($Detail) { ": $Detail" })"
    }
}

function Check-Equal {
    param([string]$Name, $Want, $Got)
    Check $Name (($Want | ConvertTo-Json -Compress) -eq ($Got | ConvertTo-Json -Compress)) "want $(($Want | ConvertTo-Json -Compress)), got $(($Got | ConvertTo-Json -Compress))"
}

# --- the version boundary ----------------------------------------------------

# Every row here is a decision `ghost upgrade` makes in
# selfupdate.AttestationRequiredFor, and the Go side has its own test over the
# same table. The two are kept in step by TestTheTwoBoundariesAgree, which runs
# this file's cases through the Go function too.
$boundary = @(
    @{ Version = '0.42.9'; Required = $false },
    @{ Version = 'v0.42.9'; Required = $false },
    @{ Version = '0.42.99'; Required = $false },
    @{ Version = '0.43.0'; Required = $true },
    @{ Version = 'v0.43.0'; Required = $true },
    @{ Version = '0.43'; Required = $true },
    @{ Version = '0.43.0-rc.1'; Required = $true },
    @{ Version = '0.44.1'; Required = $true },
    @{ Version = '1.0.0'; Required = $true },
    # Cannot be ordered, so it REQUIRES the attestation. This is the fail-closed
    # row: `$null -split '\.'` answers with a one-element array holding an empty
    # string, so a length check passes on a version with no numeric core and every
    # later comparison quietly reads it as 0.0.0 — which skipped the check.
    @{ Version = 'dev'; Required = $true },
    @{ Version = 'nightly'; Required = $true },
    @{ Version = ''; Required = $true },
    # --- the shapes the client's parseVersion refuses, and this one used to
    # --- accept. Each of these is UNPARSEABLE rather than unusual, and an
    # --- unparseable version is REQUIRED. Two of them disagreed for a whole
    # --- review cycle and the disagreement was in the dangerous direction: the
    # --- script installed a release from a checksum alone while the client
    # --- refused it.
    @{ Version = '0.42.9.1'; Required = $true },   # four components
    @{ Version = '1.2.3.4'; Required = $true },    # five
    @{ Version = '1.2'; Required = $true },        # two: ambiguous, and it used to compare EQUAL to 1.2.0
    @{ Version = '0.042.9'; Required = $true },    # leading zero in a component
    @{ Version = '01.2.3'; Required = $true },     # ditto
    @{ Version = '0.42.9-!!'; Required = $true },  # invalid prerelease ON AN OLD LINE: dropping it would have
                                                  # turned "not a version" into "an old version", which is the
                                                  # whole hazard — this row is the one that must not go to false
    @{ Version = '0.43.0-a..b'; Required = $true },  # an empty identifier inside the prerelease list
    @{ Version = '0.43.0-'; Required = $true },    # empty prerelease
    @{ Version = 'garbage'; Required = $true },
    @{ Version = 'v'; Required = $true },
    # An UPPERCASE V. PowerShell's -replace is case-insensitive and Go's
    # strings.TrimPrefix is not, so the script used to read this as 0.42.9 — older
    # than the cutover, attestation skipped — while the client called it
    # unparseable and required one. The only row that catches it is one that
    # carries the capital, because every other row here is lower-case.
    @{ Version = 'V0.42.9'; Required = $true },
    @{ Version = 'V0.43.0'; Required = $true },
    @{ Version = 'V'; Required = $true },
    # --- the rows that DISCRIMINATE. Every adversarial row above is on or above
    # --- the cutover, where "unparseable" and "at or after the cutover" happen to
    # --- agree — so a mutation that made the parser stricter, or dropped the
    # --- prerelease and build-metadata handling entirely, still answered `true`
    # --- and still passed. These are the same shapes on an OLDER line, where
    # --- getting the parse wrong changes the answer: a permissive parser that
    # --- refuses 0.42.9-rc.1 would demand an attestation for a release that
    # --- predates the whole mechanism, and the flag would stop being the
    # --- exception it is meant to be.
    @{ Version = '0.42.9-rc.1'; Required = $false },
    @{ Version = '0.42.9+build'; Required = $false },
    @{ Version = '0.42.9-a..b'; Required = $true },
    @{ Version = '0.42.0'; Required = $false },
    @{ Version = '0.9.9'; Required = $false },
    # --- the shapes that DO parse, to keep the mirror honest in the other
    # --- direction: a permissive parse that required an attestation for
    # --- everything would pass the rows above and be wrong.
    @{ Version = '0.0.0'; Required = $false },
    @{ Version = '1.0.0+build'; Required = $true },
    @{ Version = '1.0.0-rc.1+build'; Required = $true },
    @{ Version = '10.20.30'; Required = $true },
    @{ Version = '0.43.0-00'; Required = $true },
    @{ Version = '0.43.0-0'; Required = $true },
    @{ Version = '0.43.0+a-b'; Required = $true }
)
foreach ($row in $boundary) {
    Check-Equal "attestation required for '$($row.Version)'" $row.Required (Test-AttestationRequired $row.Version)
}

# Emitted so the Go side can check these against selfupdate.AttestationRequiredFor
# rather than trusting a second table of its own. Two tables agreeing by
# inspection is a coincidence; one table, compared, is a contract.
#
# An ARRAY of records, not a map keyed by the version. PowerShell hashtables key
# CASE-INSENSITIVELY — `[ordered]@{}` included, which is not obvious and cost three
# rows: 'V0.42.9' overwrote 'v0.42.9', 'V0.43.0' overwrote 'v0.43.0', and 'V'
# overwrote 'v', so the payload carried 34 entries for a 37-row table and the
# Go side compared the UPPERCASE spelling while the lowercase row was never checked
# at all. A key that is only distinct by case is not a key.
$emitted = @(
    foreach ($row in $boundary) {
        [ordered]@{ Version = $row.Version; Required = [bool](Test-AttestationRequired $row.Version) }
    }
)
# The control for that: a payload that quietly lost rows would still be valid JSON
# and would still be compared — against fewer versions, all of which happen to
# agree. So the count is checked against the table, here, where a shortfall is
# visible.
if ($emitted.Count -ne $boundary.Count) {
    Write-Host "FAIL: emitted $($emitted.Count) boundary rows for a $($boundary.Count)-row table, so some versions would never be compared"
    exit 1
}
Write-Host "BOUNDARY $($emitted | ConvertTo-Json -Compress)"

# The prerelease row is specifically the release LINE: 0.43.0-rc.1 is older than
# 0.43.0 and newer than 0.42.9, so it is attested. A naive string compare would
# put it below 0.43.0 lexically and skip the check.
Check-Equal '0.43.0-rc.1 is on the 0.43.0 line' $true (Test-AttestationRequired '0.43.0-rc.1')

# --- the identity ------------------------------------------------------------

$identity = Get-AttestationIdentity '0.44.1'
Check-Equal 'identity repo' 'wcatz/ghost' $identity.Repo
Check-Equal 'identity tag' 'v0.44.1' $identity.ReleaseTag
# All three of the policy's statements are in this one string, because gh's
# --cert-identity / --signer-repo / --signer-workflow are one exclusive group and
# the tag cannot be pinned any other way.
Check-Equal 'identity pins repo, workflow and tag' `
    'https://github.com/wcatz/ghost/.github/workflows/release.yml@refs/tags/v0.44.1' $identity.CertIdentity
Check-Equal 'identity tolerates a v-prefixed version' `
    'https://github.com/wcatz/ghost/.github/workflows/release.yml@refs/tags/v0.43.0' (Get-AttestationIdentity 'v0.43.0').CertIdentity
Check-Equal 'identity carries no branch' $true (Get-AttestationIdentity '0.44.1').CertIdentity.Contains('refs/tags/')
Check-Equal 'identity names no other workflow' $false (Get-AttestationIdentity '0.44.1').CertIdentity.Contains('plugin')

# --- the verdict, over every combination of what gh can answer ---------------

# A stub gh. $HasGh is whether the command is found; $AuthExit is `gh auth
# status`'s exit status; $VerifyExit is `gh attestation verify`'s; $Calls records
# the argument lists so a test can assert the POLICY that was passed, not only the
# verdict it produced.
function New-Stub {
    param(
        [bool]$HasGh = $true,
        [int]$AuthExit = 0,
        [int]$CapabilityExit = 0,
        [int]$VerifyExit = 0,
        [string]$VerifyOutput = 'Loaded 1 attestation from GitHub API'
    )
    $calls = [System.Collections.ArrayList]::new()
    $probe = {
        param($Name)
        if ($HasGh) { return '/usr/bin/gh' }
        return $null
    }.GetNewClosure()
    $run = {
        param($Exe, $Arguments, $WorkDir)
        # A PSCustomObject, not the array itself: PowerShell ENUMERATES a
        # string[] through the pipeline, so recording the arguments bare means
        # every later Where-Object receives seven loose strings instead of one
        # call, and the policy assertions below read $null for all of them.
        [void]$calls.Add([PSCustomObject]@{ Exe = $Exe; Args = [string[]]$Arguments })
        if ($Arguments[0] -eq 'auth') { return @{ ExitCode = $AuthExit; Output = 'not logged in' } }
        if ($Arguments.Count -eq 3 -and $Arguments[2] -eq '--help') {
            return @{ ExitCode = $CapabilityExit; Output = 'Verify the integrity and provenance of an artifact' }
        }
        return @{ ExitCode = $VerifyExit; Output = $VerifyOutput }
    }.GetNewClosure()
    return @{ Probe = $probe; Run = $run; Calls = $calls }
}

function Get-VerdictWith {
    param([hashtable]$Stub, [string]$Version = '0.44.1', [string]$File = '/tmp/ghost.zip')
    return Get-AttestationVerdict -FilePath $File -Version $Version -Probe $Stub.Probe -Run $Stub.Run
}

$stub = New-Stub
Check-Equal 'gh verifies the attestation' 'Verified' (Get-VerdictWith $stub).State

# The arguments are the policy. Without this the verdict tests would pass even if
# the script asked gh to verify against the wrong repository, the wrong workflow,
# or no tag at all — which is the failure the whole change exists to prevent.
# The capability probe is the first `attestation` call, so the verify is the second.
$attestationCalls = @($stub.Calls | Where-Object { $_.Args[0] -eq 'attestation' })
$verifyArgs = @($attestationCalls[1].Args)
Check-Equal 'the capability is probed before the archive is' 'attestation verify --help' ($attestationCalls[0].Args -join ' ')
Check-Equal 'gh is asked for the attestation of the file' '/tmp/ghost.zip' $verifyArgs[2]
Check-Equal 'gh is scoped to the repository' 'wcatz/ghost' $verifyArgs[4]
Check-Equal 'gh is given the pinned cert identity' `
    'https://github.com/wcatz/ghost/.github/workflows/release.yml@refs/tags/v0.44.1' $verifyArgs[6]
# --signer-workflow and --cert-identity are the same exclusive group in gh, and
# naming both is an error, so a script that passed both would fail on every run.
Check-Equal 'gh is not given a second, conflicting identity flag' $false ($verifyArgs -contains '--signer-workflow')
Check-Equal 'gh is not asked to skip the transparency log' $false ($verifyArgs -contains '--no-public-good')

Check-Equal 'a pre-cutover release is skipped' 'Skipped' (Get-VerdictWith (New-Stub) -Version '0.42.9').State
Check-Equal 'no gh at all is Unchecked' 'Unchecked' (Get-VerdictWith (New-Stub -HasGh $false)).State
Check-Equal 'an unauthenticated gh is Unchecked' 'Unchecked' (Get-VerdictWith (New-Stub -AuthExit 1)).State
Check-Equal 'a checked attestation that failed is Unverifiable' 'Unverifiable' (Get-VerdictWith (New-Stub -VerifyExit 1)).State
Check-Equal 'a gh usage error is Unverifiable, not Unchecked' 'Unverifiable' (Get-VerdictWith (New-Stub -VerifyExit 1 -VerifyOutput 'unknown flag: --nonsense')).State

# --- a verifier that cannot finish is not a verifier that says no ------------
#
# gh exits 1 for "did not verify", "there is no attestation" and "I could not reach
# GitHub" alike. Mapping all three to the fatal state reports a DNS failure or a
# 500 as a permanent property of the archive — the mistake this repository already
# made once, in the other direction, and wrote down.

# A gh too old to have the subcommand is caught by the probe, before it is asked
# to verify anything.
$oldGh = New-Stub -CapabilityExit 1 -VerifyOutput 'unknown command "attestation" for "gh"'
Check-Equal 'a gh without the subcommand is Unchecked, not Unverifiable' 'Unchecked' (Get-VerdictWith $oldGh).State
Check-Equal 'a gh without the subcommand never verifies the archive' 0 `
    (@($oldGh.Calls | Where-Object { $_.Args[0] -eq 'attestation' -and $_.Args.Count -ne 3 }).Count)
Check-Equal 'a gh without the subcommand is told to update' $true `
    ((Get-VerdictWith $oldGh).Reason -match 'too old')

foreach ($outage in @(
    'HTTP 500: Internal Server Error',
    'HTTP 429: Too Many Requests',
    'Get "https://api.github.com/...": dial tcp: lookup api.github.com: no such host',
    'Get "https://api.github.com/...": net/http: TLS handshake timeout',
    'Get "https://api.github.com/...": proxyconnect tcp: dial tcp: connection refused',
    'HTTP 401: Bad credentials',
    'context deadline exceeded'
)) {
    $v = Get-VerdictWith (New-Stub -VerifyExit 1 -VerifyOutput $outage)
    Check-Equal "'$($outage.Substring(0, [Math]::Min(30, $outage.Length)))' is Unchecked" 'Unchecked' $v.State
    Check-Equal "'$($outage.Substring(0, [Math]::Min(30, $outage.Length)))' does not blame the archive" $false `
        ($v.Reason -match 'did not verify')
}

# A refusal the matcher does not recognise is a refusal. This is the direction the
# whole split is chosen for: an unknown output shape must never become the
# overridable state.
foreach ($refusal in @(
    'Error: verifying with issuer "sigstore.dev"',
    'one or more attestations did not match the artifact',
    'no attestations found for subject',
    'something this script has never seen'
)) {
    $v = Get-VerdictWith (New-Stub -VerifyExit 1 -VerifyOutput $refusal)
    Check-Equal "'$($refusal.Substring(0, [Math]::Min(30, $refusal.Length)))' is Unverifiable" 'Unverifiable' $v.State
}
Check-Equal 'an empty failure output is still a refusal' 'Unverifiable' `
    (Get-VerdictWith (New-Stub -VerifyExit 1 -VerifyOutput '')).State

# The matcher is a DOWNGRADE, so the strings it must not match are the ones an
# attacker could put into gh's error text about a certificate. gh quotes the
# certificate's own subject and the repository back in its messages, so a short
# generic token here would hand an attacker the override. "SSL" and "EOF" were in
# an earlier draft of the list and a control below is the reason they are not in
# this one.
foreach ($attackerText in @(
    'the certificate is not from a trusted CA (SSL is fine)',
    'unexpected EOF while reading the statement',
    'proxy: the workflow identity could not be established',
    'the repository wcatz/ghost refused',
    'signer workflow wcatz/ghost/.github/workflows/release.yml mismatch',
    # A repository, organisation or workflow name may contain any of these, and gh
    # quotes identity text back in its errors. So an attacker who controls the
    # identity in a bundle they publish can put a downgrade token in the very
    # message that refuses them. The bare 'proxyconnect' was in the allowlist
    # until a control proved 'proxy-connect' is a legal repository name.
    'unknown proxyconnector in wcatz/ghost-proxyconnect',
    'workflow proxyconnect.yml in org wcatz-proxyconnect did not match'
)) {
    $v = Get-VerdictWith (New-Stub -VerifyExit 1 -VerifyOutput $attackerText)
    Check-Equal "attacker text is a refusal, not a machine fault: $($attackerText.Substring(0, [Math]::Min(34, $attackerText.Length)))" `
        'Unverifiable' $v.State
}

# An unauthenticated gh must not be asked to verify at all: it would answer 1, and
# treating that as "the attestation did not verify" would blame the archive for the
# machine's missing login and hide it behind a flag that claims the archive is fine.
$unauth = New-Stub -AuthExit 1
[void](Get-VerdictWith $unauth)
Check-Equal 'an unauthenticated gh is never asked to verify' $false `
    (@($unauth.Calls | Where-Object { $_.Args[0] -eq 'attestation' }).Count -gt 0)

# A pre-cutover release must not cost a network call either.
$old = New-Stub
[void](Get-VerdictWith $old -Version '0.42.9')
Check-Equal 'a pre-cutover release runs no gh command at all' 0 $old.Calls.Count

# The reason is a sentence a user can act on, and it must not leak an unbounded
# blob of gh output into it.
$noisy = New-Stub -VerifyExit 1 -VerifyOutput ('x' * 5000)
Check-Equal 'a long gh failure is truncated' $true ((Get-VerdictWith $noisy).Reason.Length -lt 700)

# --- the decision, over every verdict ----------------------------------------

function Decide {
    param([string]$State, [string]$Reason = 'because', [bool]$Skip = $false)
    return Resolve-AttestationDecision -Verdict @{ State = $State; Reason = $Reason } -Skip $Skip
}

$verified = Decide 'Verified'
Check-Equal 'Verified allows' $true $verified.Allow
Check-Equal 'Verified says so' 'info' $verified.Severity

$skipped = Decide 'Skipped'
Check-Equal 'Skipped allows' $true $skipped.Allow
# A pre-cutover release is allowed because nothing COULD vouch for it, which is a
# different fact from "something vouched for it". Silent success would let the
# second reading stand.
Check-Equal 'Skipped is not silent' 'info' $skipped.Severity
Check-Equal 'Skipped still carries its reason' $true $skipped.Message.Contains('because')

$unverifiable = Decide 'Unverifiable'
Check-Equal 'Unverifiable refuses' $false $unverifiable.Allow
Check-Equal 'Unverifiable names no flag' $false ($unverifiable.Message -match 'SkipAttestation|GHOST_SKIP_ATTESTATION')
# ...and no override reaches it, which is the asymmetry with Unchecked.
Check-Equal 'Unverifiable is not overridable' $false (Decide 'Unverifiable' -Skip $true).Allow

$unchecked = Decide 'Unchecked'
Check-Equal 'Unchecked refuses by default' $false $unchecked.Allow
Check-Equal 'Unchecked names the way out' $true ($unchecked.Message -match 'SkipAttestation')
Check-Equal 'Unchecked names the env var too' $true ($unchecked.Message -match 'GHOST_SKIP_ATTESTATION')

$uncheckedSkip = Decide 'Unchecked' -Skip $true
Check-Equal 'Unchecked with the flag allows' $true $uncheckedSkip.Allow
Check-Equal 'Unchecked with the flag warns' 'warning' $uncheckedSkip.Severity
# The warning has to say what is missing, or "allowed" reads as "verified".
Check-Equal 'the override warning names what was not established' $true `
    ($uncheckedSkip.Message -match 'WITHOUT a verified build attestation')

# An unrecognised state is refused rather than guessed at. A `default` arm that
# returned Allow would be shorter and would install anything.
Check-Equal 'an unrecognised state refuses' $false (Decide 'Nonsense').Allow
Check-Equal 'an unrecognised state is not overridable' $false (Decide 'Nonsense' -Skip $true).Allow
Check-Equal 'an empty state refuses' $false (Decide '').Allow

# --- the flag plumbing -------------------------------------------------------

# -SkipAttestation reaches Main, and the env var exists because the documented way
# to run this script is `iwr ... | iex`, where a parameter cannot be passed.
$src = Get-Content -Raw -LiteralPath $Script
Check-Equal 'Main takes the flag' $true ($src -match '(?s)function Main\s*\{[^}]*\[switch\]\$SkipAttestation')
Check-Equal 'the script declares the flag' $true ($src -match '\[switch\]\$SkipAttestation')
Check-Equal 'Main is called with the flag' $true ($src -match 'Main -SkipAttestation:\$SkipAttestation')
Check-Equal 'the env var is read' $true ($src -match 'GHOST_SKIP_ATTESTATION')

# --- the override, both ways in ---------------------------------------------
#
# Behavioural, because the earlier version of this was a substring test on the
# script text: `GHOST_SKIP_ATTESTATION` appears in the source whether the code
# reads it, ignores it, or mentions it in a comment, so that check passed against
# a script that dropped the variable from the decision entirely.
Check-Equal 'no flag and no env var does not override' $false (Test-AttestationOverridden)
Check-Equal 'the switch overrides' $true (Test-AttestationOverridden -Skip)
foreach ($truthy in @('1', 'true', 'TRUE', 'Yes', 'on', ' 1 ')) {
    $env:GHOST_SKIP_ATTESTATION = $truthy
    Check-Equal "the env var '$truthy' overrides" $true (Test-AttestationOverridden)
}
foreach ($falsy in @('0', 'false', 'no', 'off', '', 'maybe')) {
    $env:GHOST_SKIP_ATTESTATION = $falsy
    Check-Equal "the env var '$falsy' does not override" $false (Test-AttestationOverridden)
}
$env:GHOST_SKIP_ATTESTATION = $null
Check-Equal 'an unset env var does not override' $false (Test-AttestationOverridden)
# An env var from somewhere else in the user's environment must not be able to
# install ghost without a check, and 'maybe' is what a typo produces.
$env:GHOST_SKIP_ATTESTATION = 'maybe'
Check-Equal 'an unrecognised env var does not override' $false (Test-AttestationOverridden)
$env:GHOST_SKIP_ATTESTATION = $null

# And the whole path, flag included, so the two are wired together rather than
# each tested alone.
Check-Equal 'the flag lets an Unchecked release install' $true `
    (Resolve-AttestationDecision -Verdict @{ State = 'Unchecked'; Reason = 'no gh' } -Skip (Test-AttestationOverridden -Skip)).Allow
Check-Equal 'without it the same release is refused' $false `
    (Resolve-AttestationDecision -Verdict @{ State = 'Unchecked'; Reason = 'no gh' } -Skip (Test-AttestationOverridden)).Allow
# The check must run after the download, because gh keys the lookup on the digest
# of the bytes that arrived, and before the unpack, so nothing nobody has vouched
# for is handed to a decompressor.
$iCheck = $src.IndexOf('Test-BuildAttestation -FilePath')
$iUnpack = $src.IndexOf('Install-Ghost -ZipPath')
Check-Equal 'the attestation is checked' $true ($iCheck -ge 0)
# EXACTLY once, and before the unpack. A single IndexOf was not enough: it finds
# the FIRST occurrence, so a second call added after the unpack left it pointing
# at the earlier one and the ordering read as correct while the script had both.
$callCount = ([regex]::Matches($src, 'Test-BuildAttestation -FilePath')).Count
Check-Equal 'the attestation is checked exactly once' 1 $callCount
if ($iCheck -ge 0 -and $iUnpack -ge 0) {
    Check-Equal 'the check runs before the archive is unpacked' $true ($iCheck -lt $iUnpack)
    $iDownload = $src.IndexOf('Get-GhostRelease -ReleaseInfo')
    Check-Equal 'the check runs after the download' $true ($iDownload -lt $iCheck)
    # ...and the refusal has to be able to stop the install at all, which means
    # the decision is consulted between the two rather than merely printed.
    $iDecision = $src.IndexOf('if (-not $decision.Allow)')
    Check-Equal 'the decision is consulted' $true ($iDecision -ge 0)
    Check-Equal 'the refusal is between the check and the unpack' $true `
        ($iDecision -gt $iCheck -and $iDecision -lt $iUnpack)
}

# --- the 5.1 target ----------------------------------------------------------
#
# This script's header says `#Requires -Version 5.1`, `powershell.exe` on Windows
# IS 5.1, and the documented way to run it is `irm … | iex`. A PowerShell 7-only
# construct is a PARSE error, so it fails the whole file rather than one function
# and nothing installs at all — and nothing here would catch it, because the cases
# run under pwsh 7 and no CI job runs Windows PowerShell.
#
# So the operators that are 7.0+ and have no 5.1 spelling are looked for in the
# CODE. Comments are stripped first, and the stripper has to be real: install.ps1
# documents itself in `<# … #>` blocks whose interior lines do NOT begin with `#`,
# so anything that only understands `//` and trailing `#` leaves the prose
# behind — and the prose NAMES both operators while explaining that they must not
# be used. A matcher that read comments would fail on the script's own
# documentation, and one that half-worked would only be caught by that.
function Get-CodeOnly {
    <#
    .SYNOPSIS
        The CODE of a PowerShell script: comments and string literals removed.
    .DESCRIPTION
        A scanner rather than a regex, and the order it makes decisions in is the
        whole point. A `#` inside a string is not a comment and a `//` inside a
        string is not a comment, so strings have to be recognised BEFORE comments —
        a script that strips comments first silently deletes the rest of any line
        whose string contains a hash, and install.ps1 has here-strings.

        What it removes:
          - block comments, which NEST in PowerShell, so the closing pair inside
            one does not necessarily end it. install.ps1 documents itself entirely
            in them, and their interior lines do not begin with a hash, so anything
            that only understands `//` and a trailing `#` leaves the prose behind
            — and the prose names the very constructs being forbidden.
          - `//` and `#` line comments.
          - single-quoted, double-quoted, and here-string literals, with PowerShell's
            doubled-quote escape (`''` inside `'`, `""` inside `"`).

        What it does NOT remove: nothing else. It is not a parser, and
        Test-The5.1ConstructsAreNamed says plainly what that costs.
    #>
    param([string]$Text)

    $out = [System.Text.StringBuilder]::new()
    $i = 0
    $blockDepth = 0
    $hereTerminator = $null
    while ($i -lt $Text.Length) {
        $two = if ($i + 1 -lt $Text.Length) { $Text.Substring($i, 2) } else { '' }

        if ($null -ne $hereTerminator) {
            if ($two -eq $hereTerminator) { $hereTerminator = $null; $i += 2; continue }
            $i++
            continue
        }
        if ($blockDepth -gt 0) {
            if ($two -eq '<#') { $blockDepth++; $i += 2; continue }
            if ($two -eq '#>') { $blockDepth--; $i += 2; continue }
            $i++
            continue
        }
        if ($two -eq '<#') { $blockDepth = 1; $i += 2; continue }
        if ($two -eq "@'") { $hereTerminator = "'@"; $i += 2; continue }
        if ($two -eq '@"') { $hereTerminator = '"@'; $i += 2; continue }
        if ($two -eq '//' -or $Text[$i] -eq '#') {
            while ($i -lt $Text.Length -and $Text[$i] -ne "`n") { $i++ }
            continue
        }
        if ($Text[$i] -eq "'" -or $Text[$i] -eq '"') {
            $q = $Text[$i]
            $i++
            while ($i -lt $Text.Length) {
                if ($Text[$i] -eq $q) {
                    # A doubled quote is an escaped quote, not the end.
                    if ($i + 1 -lt $Text.Length -and $Text[$i + 1] -eq $q) { $i += 2; continue }
                    $i++
                    break
                }
                $i++
            }
            continue
        }
        [void]$out.Append($Text[$i])
        $i++
    }
    return $out.ToString()
}

$raw = Get-Content -Raw -LiteralPath $Script
$psCode = Get-CodeOnly $raw

# Each entry is a construct that does not PARSE under Windows PowerShell 5.1, with
# the pattern that finds it in code. This is a list, not a heuristic: the previous
# version of this check looked for `??` and `?.` only, and the honest description of
# that was "it catches the two operators this author happened to type". So the
# coverage is enumerated, and every pattern has a control proving it fires on the
# construct and does not fire on a 5.1 spelling of the same idea.
#
# WHAT THIS IS NOT: a parser. Get-CodeOnly removes comments AND string literals,
# so a pattern can never fire from inside a quoted string — that is what the
# stripper is for, and it is why a bare `-AsHashtable` token is safe here where it
# would not be in the script itself. (An earlier version of this comment said the
# opposite: it claimed literals were NOT stripped and that the patterns were shaped
# to survive them. The code was rewritten to strip literals in the same change and
# the prose was not, so the comment told the next person to write patterns for a
# scanner that no longer existed.)
#
# The residual is a construct spelled in a shape no pattern here anticipates: it
# would pass this check and still fail to parse on 5.1. That is stated rather than
# implied away. The patterns are still shaped rather than loose, because a stripper
# is not a parser either — a first version of the -Parallel pattern was the bare
# token `-Parallel` and a control caught a legal `$parallel` variable on the next
# run.
$ps7Only = [ordered]@{
    'null-coalescing operator'   = '\?\?'
    'null-conditional operator'  = '\?\.'
    'null-coalescing assignment' = '\?\?='
    'ternary conditional'        = '\?[^\s|]*\s+[^\r\n|]*:'
    'pipeline chain operator &&' = '(?m)(?<![|0-9])&&'
    'pipeline chain operator ||' = '(?m)(?<![|0-9])\|\|'
    'ForEach-Object -Parallel'   = 'ForEach-Object[\s,]+-Parallel\b'
    'ConvertFrom-Json -AsHashtable' = '-AsHashtable\b'
}
foreach ($entry in $ps7Only.GetEnumerator()) {
    Check-Equal "install.ps1 uses no PowerShell 7 $($entry.Key)" $false ($psCode -match $entry.Value)
}
# Join-Path gained a third positional parameter in 7.0; with two it is 5.1-legal,
# so this is a shape check rather than a token one.
$joinPath = [regex]::Matches($psCode, '(?i)\bJoin-Path\b')
foreach ($m in $joinPath) {
    $tail = $psCode.Substring($m.Index + $m.Length)
    # Only the arguments before the end of the statement can be positional.
    $stmt = ($tail -split '[\r\n]')[0]
    $args = ([regex]::Matches(($stmt -split '\|')[0], ',')).Count
    Check-Equal 'Join-Path is given at most two arguments (5.1 has no AdditionalChildPath)' $true ($args -le 1)
}
Check-Equal 'install.ps1 declares its 5.1 target' $true ($raw -match '#Requires -Version 5\.1')

# ...and the fallback the `??` would have provided is still there, so removing the
# operator cannot have removed the behaviour with it.
Check-Equal 'the version core still falls back when unorderable' 'vdev' (Get-AttestationIdentity 'dev').ReleaseTag
Check-Equal 'the version core still falls back for an empty version' 'v' (Get-AttestationIdentity '').ReleaseTag
Check-Equal 'an orderable version uses its core' 'v0.43.0' (Get-AttestationIdentity '0.43.0').ReleaseTag
Check-Equal 'an unorderable version still requires a check' $true (Test-AttestationRequired 'dev')

# Controls for the stripper itself, in both directions. Without these, "no `??`
# found" could be a stripper that removes the whole file.
Check-Equal 'the stripper keeps code' $true ((Get-CodeOnly '$a = 1') -match '\$a = 1')
Check-Equal 'the stripper drops a line comment' 'x = 1' ((Get-CodeOnly 'x = 1 # ?? here').Trim())
Check-Equal 'the stripper drops a block comment' 'x = 1' ((Get-CodeOnly "<# ?? and ?. #> x = 1").Trim())
Check-Equal 'the stripper drops a multi-line block comment' 'x = 1' `
    ((Get-CodeOnly "<#`n ?? on its own line`n and ?. too`n#> x = 1").Trim())
Check-Equal 'the stripper handles a nested block comment' 'x = 1' `
    ((Get-CodeOnly "<# outer <# inner ?? #> still outer #> x = 1").Trim())
# ...and the matcher catches the operators when they ARE in code. One sample per
# pattern, so a pattern that stopped matching anything would fail here rather than
# quietly making its check above vacuous.
$mustCatch = @(
    '?? $x', '"a" ?? $b', '$y = $z?.Name', "??`n", 'if ($a ?? $b) {}',
    '$x ??= $y',
    '$ok = $a ? $b : $c',
    'Get-Content f | Where-Object { $_ } && Write-Host done',
    'Get-Content f || Write-Host missing',
    '1..8 | ForEach-Object -Parallel { $_ }',
    '$h = ConvertFrom-Json $j -AsHashtable'
)
foreach ($sample in $mustCatch) {
    $hit = $false
    foreach ($e in $ps7Only.GetEnumerator()) {
        if ((Get-CodeOnly $sample) -match $e.Value) { $hit = $true }
    }
    Check-Equal "the matcher would catch '$($sample.Trim())'" $true $hit
}
# ...and does not fire on the 5.1 spellings, which is the other half: a pattern
# loose enough to match a legal line would make the checks above unusable.
foreach ($legal in @(
    '$a = 1 ?? 2'.Replace(' ?? ', ' + '),
    'Get-Content f | Where-Object { $_ }',
    'if ($a) { b } else { c }',
    'Join-Path $a "b"',
    'Join-Path -Path $a -ChildPath "b"',
    '$parallel = 1; $r = $a -eq $parallel',
    # Passes because the stripper DELETES the literal, not because the pattern is
    # shaped to survive it. Which is the whole point of the stripper.
    '$msg = "use -AsHashtable on PowerShell 7 only"'
)) {
    $hit = $false
    foreach ($e in $ps7Only.GetEnumerator()) {
        if ((Get-CodeOnly $legal) -match $e.Value) { $hit = $true }
    }
    Check-Equal "a 5.1-legal line is not flagged: '$($legal.Trim())'" $false $hit
}
# ...and leaves a lone `?` alone, so a wildcard or a string is not a false alarm.
Check-Equal 'a lone question mark is not an operator' $false ((Get-CodeOnly 'Get-ChildItem -Filter *.ps1?') -match '\?\?|\?\.')
Check-Equal 'a question mark in a string is not an operator' $false ((Get-CodeOnly 'Write-Host "what? really?"') -match '\?\?|\?\.')

# --- report ------------------------------------------------------------------

if ($script:Failures.Count -gt 0) {
    foreach ($f in $script:Failures) { Write-Host "FAIL: $f" }
    Write-Host "$($script:Failures.Count) of $($script:Checks) checks failed"
    exit 1
}
Write-Host "all $($script:Checks) checks passed"
exit 0

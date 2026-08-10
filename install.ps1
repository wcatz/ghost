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

[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$script:Repo = 'wcatz/ghost'
$script:InstallDir = Join-Path $env:LOCALAPPDATA 'ghost\bin'

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

function Get-GhostRelease {
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

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
    Main
}

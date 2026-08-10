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
    $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
    switch ($arch) {
        'X64' { return 'amd64' }
        'Arm64' { return 'arm64' }
        default {
            throw "Unsupported architecture: $arch. ghost only ships windows/amd64 and windows/arm64 builds."
        }
    }
}

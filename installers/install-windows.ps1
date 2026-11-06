[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ($env:OS -ne 'Windows_NT') { throw 'This installer requires Windows.' }

# Download only the project's latest published release, not an arbitrary URL.
$api = 'https://api.github.com/repos/Aruneshw/gpu_toolkit/releases/latest'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$release = Invoke-RestMethod -Uri $api -Headers @{ 'User-Agent' = 'MeshCompute-installer' }
$exe = @($release.assets | Where-Object { $_.name -eq 'meshcompute-windows-amd64.exe' })
$sums = @($release.assets | Where-Object { $_.name -eq 'SHA256SUMS' })
if ($exe.Count -ne 1 -or $sums.Count -ne 1) { throw 'Release is missing the Windows binary or checksums.' }
$expectedPrefix = 'https://github.com/Aruneshw/gpu_toolkit/releases/download/'
if (-not $exe[0].browser_download_url.StartsWith($expectedPrefix, [StringComparison]::Ordinal) -or
    -not $sums[0].browser_download_url.StartsWith($expectedPrefix, [StringComparison]::Ordinal)) {
    throw 'Unexpected release asset URL.'
}
$checksumText = (Invoke-WebRequest -Uri $sums[0].browser_download_url -UseBasicParsing).Content
$lines = @($checksumText -split "`n" | Where-Object { $_ -match '^([0-9a-fA-F]{64})\s+\*?meshcompute-windows-amd64\.exe\s*$' })
if ($lines.Count -ne 1 -or $lines[0] -notmatch '^([0-9a-fA-F]{64})\s+') { throw 'No unique Windows checksum found.' }
$hash = $Matches[1]
& (Join-Path $PSScriptRoot 'install-prebuilt.ps1') -Url $exe[0].browser_download_url -Sha256 $hash

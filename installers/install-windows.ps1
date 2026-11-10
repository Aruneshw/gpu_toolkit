[CmdletBinding()]
param([switch]$Join, [switch]$AllowElevated, [Parameter(Position = 0)][string]$Code)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ($env:OS -ne 'Windows_NT') { throw 'This installer requires Windows.' }

$installed = Join-Path $env:LOCALAPPDATA 'meshcompute\bin\meshcompute.exe'
$root = Split-Path $PSScriptRoot -Parent
$bundle = Join-Path $root 'meshcompute-windows-amd64.exe'
$bundled = Test-Path -LiteralPath $bundle
function Invoke-Join {
    if ($Code) { & $installed join $Code.Trim() } else { & $installed join }
    exit $LASTEXITCODE
}
# join.cmd is also the restart path for already-enrolled workers. With the
# release ZIP, the bundled (newer) executable is installed first; the installer
# returns immediately when it is already the installed version. With the
# source ZIP and an existing installation, no network access is needed.
if ($Join -and (Test-Path -LiteralPath $installed) -and -not $bundled) {
    Invoke-Join
}

# Download only the project's latest published release, not an arbitrary URL.
if ($bundled) {
    $checksumText = Get-Content -LiteralPath (Join-Path $root 'SHA256SUMS') -Raw
} else {
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
if ($checksumText -is [byte[]]) { $checksumText = [Text.Encoding]::UTF8.GetString($checksumText) }
}
$lines = @($checksumText -split "`n" | Where-Object { $_ -match '^([0-9a-fA-F]{64})\s+\*?meshcompute-windows-amd64\.exe\s*$' })
if ($lines.Count -ne 1 -or $lines[0] -notmatch '^([0-9a-fA-F]{64})\s+') { throw 'No unique Windows checksum found.' }
$hash = $Matches[1]
if ($bundled) {
    & (Join-Path $PSScriptRoot 'install-prebuilt.ps1') -BinaryPath $bundle -Sha256 $hash -AllowElevated:$AllowElevated
} else {
    & (Join-Path $PSScriptRoot 'install-prebuilt.ps1') -Url $exe[0].browser_download_url -Sha256 $hash -AllowElevated:$AllowElevated
}
$binDir = Split-Path $installed -Parent
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (@($userPath -split ';' | Where-Object { $_.TrimEnd('\') -eq $binDir.TrimEnd('\') }).Count -eq 0) {
    [Environment]::SetEnvironmentVariable('Path', (($userPath, $binDir | Where-Object { $_ }) -join ';'), 'User')
}
Write-Output 'Installed. Use .\join.cmd now, or meshcompute join in a new terminal.'
if ($Join) { Invoke-Join }

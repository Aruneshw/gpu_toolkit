[CmdletBinding()]
param(
    [switch]$Force
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ($env:OS -ne 'Windows_NT') { throw 'This uninstaller requires Windows.' }
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if ($principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run without administrator privileges.' }
if (-not $env:LOCALAPPDATA) { throw 'LOCALAPPDATA must be set.' }

$binDir = Join-Path $env:LOCALAPPDATA 'meshcompute\bin'
$binary = Join-Path $binDir 'meshcompute.exe'
$marker = Join-Path $binDir 'install.json'
$existing = Get-Item -LiteralPath $binary -Force -ErrorAction SilentlyContinue
if ($null -eq $existing) { Write-Output 'No installed binary found; state preserved.'; return }
if ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Refusing non-regular file: $binary" }

$lock = Join-Path $binDir '.meshcompute-install.lock'
$lockStream = [IO.File]::Open($lock, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
$oldToolchain = $env:GOTOOLCHAIN
try {
    $verified = $false
    $source = $null
    $markerItem = Get-Item -LiteralPath $marker -Force -ErrorAction SilentlyContinue
    if ($null -ne $markerItem) {
        if ($markerItem.PSIsContainer -or ($markerItem.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Refusing non-regular file: $marker" }
        try {
            $info = Get-Content -LiteralPath $marker -Raw | ConvertFrom-Json
        } catch {
            throw "Install marker is unreadable: $marker"
        }
        $shaProperty = $info.PSObject.Properties['sha256']
        $markerHash = if ($null -ne $shaProperty) { [string]$shaProperty.Value } else { '' }
        if ($markerHash -notmatch '^[0-9A-Fa-f]{64}$') {
            throw "Install marker has no valid checksum: $marker"
        }
        $expected = $markerHash.ToUpperInvariant()
        $actual = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash.ToUpperInvariant()
        $source = 'install.json'
        if ($actual -ne $expected) {
            if (-not $Force) { throw "Refusing to remove a modified binary. Review it, or rerun with -Force: $binary" }
            Write-Warning "Checksum differs from install.json; removing anyway because -Force was supplied."
            $source = 'forced removal after checksum mismatch'
        }
        $verified = $true
    }

    if (-not $verified) {
        if ($null -ne (Get-Command go -ErrorAction SilentlyContinue)) {
            $env:GOTOOLCHAIN = 'local'
            $info = & go version -m $binary
            if ($LASTEXITCODE -eq 0 -and ($info -match '^\s+path\s+meshcompute/cmd/meshcompute$')) {
                $verified = $true
                $source = 'go build info'
            }
        }
        if (-not $verified) {
            if (-not $Force) {
                throw "Cannot verify this binary (no install.json and no usable Go build info). Review it, or rerun with -Force: $binary"
            }
            Write-Warning 'Removing an unverified binary because -Force was supplied.'
            $source = 'forced removal'
        }
    }

    [IO.File]::Delete($binary)
    if ($null -ne $markerItem) { [IO.File]::Delete($marker) }
    Write-Output "Removed meshcompute (verified by $source) only; configuration, state, and backup preserved."
} finally {
    $env:GOTOOLCHAIN = $oldToolchain
    $lockStream.Dispose()
    [IO.File]::Delete($lock)
}

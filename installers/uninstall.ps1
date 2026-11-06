[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ($env:OS -ne 'Windows_NT') { throw 'This uninstaller requires Windows.' }
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if ($principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run without administrator privileges.' }
if (-not $env:LOCALAPPDATA) { throw 'LOCALAPPDATA must be set.' }
$binary = Join-Path $env:LOCALAPPDATA 'meshcompute\bin\meshcompute.exe'
$existing = Get-Item -LiteralPath $binary -Force -ErrorAction SilentlyContinue
if ($null -eq $existing) { Write-Output 'No installed binary found; state preserved.'; return }
if ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Refusing non-regular file: $binary" }
$null = Get-Command go -ErrorAction Stop
$lock = Join-Path (Split-Path $binary -Parent) '.meshcompute-install.lock'
$lockStream = [IO.File]::Open($lock, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
$oldToolchain = $env:GOTOOLCHAIN
try {
    $env:GOTOOLCHAIN = 'local'
    $info = & go version -m $binary
    if ($LASTEXITCODE -ne 0 -or -not ($info -match '^\s+path\s+meshcompute/cmd/meshcompute$')) { throw "Refusing unrelated binary: $binary" }
    [IO.File]::Delete($binary)
    Write-Output 'Removed meshcompute only; configuration, state, and backup preserved.'
} finally {
    $env:GOTOOLCHAIN = $oldToolchain
    $lockStream.Dispose()
    [IO.File]::Delete($lock)
}

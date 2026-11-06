[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ($env:OS -ne 'Windows_NT') { throw 'This installer requires Windows.' }
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if ($principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run without administrator privileges.' }
if (-not $env:LOCALAPPDATA) { throw 'LOCALAPPDATA must be set.' }
$null = Get-Command go -ErrorAction Stop
$binDir = Join-Path $env:LOCALAPPDATA 'meshcompute\bin'
$binary = Join-Path $binDir 'meshcompute.exe'
$backup = "$binary.previous"
$lock = Join-Path $binDir '.meshcompute-install.lock'
$sourceDir = Split-Path $PSScriptRoot -Parent
$null = [IO.Directory]::CreateDirectory($binDir)
$lockStream = [IO.File]::Open($lock, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
$stage = $null
$backedUp = $false
$committed = $false
$oldEnv = @{}
foreach ($key in @('GOTOOLCHAIN','GOPROXY','GOSUMDB','GOWORK','GOFLAGS','CGO_ENABLED','GOOS','GOARCH')) { $oldEnv[$key] = [Environment]::GetEnvironmentVariable($key, 'Process') }
try {
    $env:GOTOOLCHAIN = 'local'; $env:GOPROXY = 'off'; $env:GOSUMDB = 'off'; $env:GOWORK = 'off'
    $env:GOFLAGS = ''; $env:CGO_ENABLED = '0'; $env:GOOS = 'windows'
    $env:GOARCH = (& go env GOHOSTARCH)
    if ($LASTEXITCODE -ne 0) { throw 'Cannot determine host architecture.' }
    $existing = Get-Item -LiteralPath $binary -Force -ErrorAction SilentlyContinue
    if ($null -ne $existing) {
        if ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Refusing non-regular file: $binary" }
        $info = & go version -m $binary
        if ($LASTEXITCODE -ne 0 -or -not ($info -match '^\s+path\s+meshcompute/cmd/meshcompute$')) { throw "Refusing unrelated binary: $binary" }
        if ($null -ne (Get-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue)) { throw "Preserve or remove the reviewed backup first: $backup" }
    }
    $stage = Join-Path $binDir ('.meshcompute-stage.' + [Guid]::NewGuid().ToString('N'))
    $null = New-Item -ItemType Directory -Path $stage
    $stagedBinary = Join-Path $stage 'meshcompute.exe'
    Push-Location $sourceDir
    try {
        & go build -trimpath -buildvcs=false '-ldflags=-s -w -X main.version=0.1.0-dev' -o $stagedBinary ./cmd/meshcompute
        if ($LASTEXITCODE -ne 0) { throw 'Offline build failed.' }
    } finally { Pop-Location }
    & $stagedBinary install
    if ($LASTEXITCODE -ne 0) { throw 'Configuration initialization failed.' }
    if ($null -ne $existing) { [IO.File]::Move($binary, $backup); $backedUp = $true }
    [IO.File]::Move($stagedBinary, $binary)
    $committed = $true
    Write-Output "Installed $binary. Add $binDir to PATH if needed."
} finally {
    try {
        if ($backedUp -and -not $committed) { [IO.File]::Move($backup, $binary) }
        if ($stage -and [IO.Directory]::Exists($stage)) {
            $file = Join-Path $stage 'meshcompute.exe'
            if ([IO.File]::Exists($file)) { [IO.File]::Delete($file) }
            [IO.Directory]::Delete($stage, $false)
        }
    } finally {
        foreach ($key in $oldEnv.Keys) { [Environment]::SetEnvironmentVariable($key, $oldEnv[$key], 'Process') }
        $lockStream.Dispose()
        [IO.File]::Delete($lock)
    }
}

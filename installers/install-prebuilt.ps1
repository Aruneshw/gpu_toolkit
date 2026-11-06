[CmdletBinding(DefaultParameterSetName = 'Path')]
param(
    [Parameter(Mandatory = $true, ParameterSetName = 'Path')]
    [string]$BinaryPath,

    [Parameter(Mandatory = $true, ParameterSetName = 'Url')]
    [string]$Url,

    [string]$Sha256,

    # Explicit override for automated test harnesses that run elevated.
    [switch]$AllowElevated
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ($env:OS -ne 'Windows_NT') { throw 'This installer requires Windows.' }
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if ($principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator) -and -not $AllowElevated) { throw 'Run without administrator privileges.' }
if (-not $env:LOCALAPPDATA) { throw 'LOCALAPPDATA must be set.' }

$binDir = Join-Path $env:LOCALAPPDATA 'meshcompute\bin'
$binary = Join-Path $binDir 'meshcompute.exe'
$backup = "$binary.previous"
$marker = Join-Path $binDir 'install.json'
$lock = Join-Path $binDir '.meshcompute-install.lock'
$download = $null
$stage = $null
$backedUp = $false
$committed = $false
$source = $null

$null = [IO.Directory]::CreateDirectory($binDir)
$lockStream = [IO.File]::Open($lock, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
try {
    if ($PSCmdlet.ParameterSetName -eq 'Url') {
        if (-not $Sha256) { throw 'A SHA-256 checksum is required when downloading over the network.' }
        $uri = [Uri]::new($Url)
        if ($uri.Scheme -ne 'https') { throw 'Only HTTPS download URLs are accepted.' }
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        $download = Join-Path ([IO.Path]::GetTempPath()) ('meshcompute-' + [Guid]::NewGuid().ToString('N') + '.exe')
        Invoke-WebRequest -Uri $uri.AbsoluteUri -OutFile $download -UseBasicParsing
        $source = $download
    } else {
        $item = Get-Item -LiteralPath $BinaryPath -Force
        if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Refusing non-regular file: $BinaryPath" }
        $source = $item.FullName
    }
    if ($Sha256 -and $Sha256 -notmatch '^[0-9A-Fa-f]{64}$') { throw 'Sha256 must be 64 hexadecimal characters.' }

    $stage = Join-Path $binDir ('.meshcompute-stage.' + [Guid]::NewGuid().ToString('N') + '.exe')
    [IO.File]::Copy($source, $stage, $true)

    $header = New-Object byte[] 2
    $stream = [IO.File]::OpenRead($stage)
    try { $read = $stream.Read($header, 0, 2) } finally { $stream.Dispose() }
    if ($read -ne 2 -or $header[0] -ne 0x4D -or $header[1] -ne 0x5A) { throw 'The file is not a Windows executable.' }

    $actual = (Get-FileHash -LiteralPath $stage -Algorithm SHA256).Hash.ToUpperInvariant()
    if ($Sha256) {
        if ($actual -ne $Sha256.ToUpperInvariant()) { throw "SHA-256 mismatch: expected $($Sha256.ToUpperInvariant()), got $actual" }
    } else {
        Write-Output "SHA256 $actual (no checksum was supplied; compare this with the release file)"
    }

    $existing = Get-Item -LiteralPath $binary -Force -ErrorAction SilentlyContinue
    if ($null -ne $existing) {
        if ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Refusing non-regular file: $binary" }
        if ($null -ne (Get-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue)) { throw "Preserve or remove the reviewed backup first: $backup" }
    }

    & $stage install
    if ($LASTEXITCODE -ne 0) { throw 'Configuration initialization failed.' }
    $versionOutput = @(& $stage version)
    if ($LASTEXITCODE -ne 0 -or $versionOutput.Count -eq 0 -or -not $versionOutput[0]) { throw 'The executable did not report a version.' }
    $version = [string]$versionOutput[0]

    if ($null -ne $existing) { [IO.File]::Move($binary, $backup); $backedUp = $true }
    [IO.File]::Move($stage, $binary)
    $committed = $true

    $info = [ordered]@{ sha256 = $actual; version = [string]$version; installed_at = (Get-Date).ToUniversalTime().ToString('o') }
    [IO.File]::WriteAllText($marker, ($info | ConvertTo-Json -Compress), (New-Object System.Text.UTF8Encoding($false)))
    Write-Output "Installed $binary (version $version). Add $binDir to PATH if needed."
} finally {
    try {
        if ($backedUp -and -not $committed) { [IO.File]::Move($backup, $binary) }
        if ($stage -and [IO.File]::Exists($stage)) { [IO.File]::Delete($stage) }
        if ($download -and [IO.File]::Exists($download)) { [IO.File]::Delete($download) }
    } finally {
        $lockStream.Dispose()
        [IO.File]::Delete($lock)
    }
}

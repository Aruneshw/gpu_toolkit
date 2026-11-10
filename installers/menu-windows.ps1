# MeshCompute one-step launcher for Windows (started by windows.cmd).
#   windows.cmd               menu
#   windows.cmd join CODE     join a friend's group
#   windows.cmd host          host a group: shows an invitation, opens the dashboard
#   windows.cmd invite        new invitation for the group you are hosting
#   windows.cmd reconnect     reconnect this computer to the group it joined
[CmdletBinding()]
param(
    [Parameter(Position = 0)][string]$Action,
    [Parameter(Position = 1)][string]$Code,
    [switch]$AllowElevated
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ($env:OS -ne 'Windows_NT') { throw 'This launcher requires Windows.' }

$root = Split-Path $PSScriptRoot -Parent
$exe = Join-Path $env:LOCALAPPDATA 'meshcompute\bin\meshcompute.exe'
$bundle = Join-Path $root 'meshcompute-windows-amd64.exe'
# Workers keep the default state directory so join.cmd and this menu agree.
$hostHome = Join-Path $env:APPDATA 'meshcompute-group'
$ttl = '1h'
$releases = 'https://github.com/Aruneshw/gpu_toolkit/releases/latest'

function Write-Step([string]$text) { Write-Host "==> $text" -ForegroundColor Green }
function Write-Note([string]$text) { Write-Host "!!  $text" -ForegroundColor Yellow }
function Get-ExitCode {
    $v = Get-Variable -Name LASTEXITCODE -Scope Global -ErrorAction SilentlyContinue
    if ($null -eq $v -or $null -eq $v.Value) { return 0 }
    return [int]$v.Value
}

# Install or update. The release ZIP's bundled executable is preferred (no
# network); otherwise the latest release is downloaded once and verified.
function Install-MeshCompute {
    if ((Test-Path -LiteralPath $bundle) -or -not (Test-Path -LiteralPath $exe)) {
        Write-Step 'Installing MeshCompute (checksum verified)...'
        & (Join-Path $PSScriptRoot 'install-windows.ps1') -AllowElevated:$AllowElevated | Out-Host
    }
    if (-not (Test-Path -LiteralPath $exe)) { throw "MeshCompute is not installed at $exe" }
}


function Show-FriendCommands([string]$invite) {
    Write-Host ''
    Write-Host "Send ONE of these to your friend privately (works once, expires in $ttl):" -ForegroundColor Cyan
    Write-Host ''
    Write-Host 'Windows (extract meshcompute-windows-amd64.zip, open PowerShell in that folder):'
    Write-Host "  .\join.cmd $invite"
    Write-Host ''
    Write-Host 'Linux (extract meshcompute-linux-amd64.tar.gz, open a terminal in that folder):'
    Write-Host "  ./linux.sh join $invite"
    Write-Host ''
    Write-Host "Download for your friend: $releases"
    try { Set-Clipboard -Value ".\join.cmd $invite"; Write-Note 'The Windows command was copied to your clipboard.' } catch { }
}

function Start-Join([string]$value) {
    Write-Step 'Joining the group (a new key pair is generated on this computer automatically).'
    if ($value) {
        & $exe join $value.Trim()
    } else {
        # meshcompute reads the pasted invitation itself (no Read-Host length
        # limit) and offers "Enter = reconnect" if this PC already joined.
        & $exe join
    }
    exit (Get-ExitCode)
}

function Start-Reconnect {
    Write-Step 'Reconnecting to your group. Keep this window open; Ctrl-C stops.'
    & $exe start
    exit (Get-ExitCode)
}

function Start-Host {
    Write-Step "Starting your group host (state: $hostHome)"
    Write-Note 'If Windows Firewall asks, click "Allow access" so friends can connect.'
    Write-Host 'The dashboard opens in your browser: live GPUs/VRAM of every laptop and an Invite a friend button.'
    Write-Host 'Keep this window open. Press Ctrl-C to stop the group.'
    Write-Host ''
    & $exe --home $hostHome host --auto --ttl $ttl --open
    exit (Get-ExitCode)
}

function New-Invitation {
    if (-not (Test-Path -LiteralPath (Join-Path $hostHome 'identity.json'))) { throw 'You are not hosting a group yet. Choose "Host a group" first.' }
    $invite = (& $exe --home $hostHome pair --ttl $ttl | Select-Object -Last 1)
    if ((Get-ExitCode) -ne 0 -or -not $invite) { throw 'Could not create an invitation.' }
    Show-FriendCommands ([string]$invite).Trim()
    Write-Note 'Your group host window must be running for your friend to connect.'
}

Install-MeshCompute
$version = (& $exe version | Select-Object -First 1)

if (-not $Action) {
    Write-Host ''
    Write-Host "MeshCompute - share GPUs between laptops   ($version)" -ForegroundColor White
    Write-Host ''
    Write-Host '  1) Join a group     - paste the invitation key a friend sent you'
    Write-Host '  2) Reconnect        - this computer already joined; connect again'
    Write-Host '  3) Host a group     - generate an invitation key and open the dashboard'
    Write-Host '  4) New invitation   - another key for the group you are hosting'
    Write-Host '  5) Quit'
    Write-Host ''
    $choice = Read-Host 'Choose 1-5'
    $Action = @{ '1' = 'join'; '2' = 'reconnect'; '3' = 'host'; '4' = 'invite' }[$choice.Trim()]
    if (-not $Action) { exit 0 }
}

switch ($Action.ToLowerInvariant()) {
    'join' { Start-Join $Code }
    'reconnect' { Start-Reconnect }
    'start' { Start-Reconnect }
    'host' { Start-Host }
    'invite' { New-Invitation }
    'key' { New-Invitation }
    default { throw "Unknown option '$Action'. Use: join CODE, reconnect, host, invite." }
}

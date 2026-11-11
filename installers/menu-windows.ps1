# MeshCompute one-step launcher for Windows. Start it with win.bat.
#   win.bat               menu
#   win.bat host          generate a key: host a group and open the dashboard
#   win.bat join KEY      join a friend's group with their key
#   win.bat reconnect     reconnect this PC to the group it joined
#   win.bat invite        another key for the group you are hosting
#   win.bat mesh          host on your Tailscale / WireGuard address instead of the relay
#   win.bat gpu           install llama.cpp so this PC can lend or pool GPUs
#   win.bat install       only install or update MeshCompute
# Written for Windows PowerShell 5.1 (built into Windows 10/11).
[CmdletBinding()]
param(
    [Parameter(Position = 0)][string]$Action,
    [Parameter(Position = 1)][string]$Code,
    [switch]$AllowElevated
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = Split-Path $PSScriptRoot -Parent
$exe = Join-Path $env:LOCALAPPDATA 'meshcompute\bin\meshcompute.exe'
$bundle = Join-Path $root 'meshcompute-windows-amd64.exe'
# Workers keep the default state directory so join.cmd and this menu agree.
$hostHome = Join-Path $env:APPDATA 'meshcompute-group'
$ttl = '1h'
# Public TCP relay so friends on any network can connect. Set the environment
# variable MESHCOMPUTE_RELAY=none to host directly on this PC's IP instead.
$relay = if ($env:MESHCOMPUTE_RELAY) { $env:MESHCOMPUTE_RELAY } else { 'bore.pub' }
if (@('none', 'off', 'direct') -contains $relay.ToLowerInvariant()) { $relay = '' }
$releases = 'https://github.com/Aruneshw/gpu_toolkit/releases/latest'

function Write-Step([string]$text) { Write-Host "==> $text" -ForegroundColor Green }
function Write-Note([string]$text) { Write-Host "!!  $text" -ForegroundColor Yellow }
function Get-ExitCode {
    $v = Get-Variable -Name LASTEXITCODE -Scope Global -ErrorAction SilentlyContinue
    if ($null -eq $v -or $null -eq $v.Value) { return 0 }
    return [int]$v.Value
}

# Install or update. The ZIP's bundled executable is preferred (no network);
# otherwise the latest release is downloaded and its SHA-256 verified. A failed
# update never blocks you: the already-installed version keeps working.
function Install-MeshCompute {
    $needed = (Test-Path -LiteralPath $bundle) -or -not (Test-Path -LiteralPath $exe)
    if (-not $needed) { return }
    Write-Step 'Installing / updating MeshCompute (checksum verified)...'
    try {
        & (Join-Path $PSScriptRoot 'install-windows.ps1') -AllowElevated:$AllowElevated | Out-Host
    } catch {
        if (Test-Path -LiteralPath $exe) {
            Write-Note "Update skipped: $($_.Exception.Message)"
            Write-Note 'Continuing with the version that is already installed.'
        } else {
            throw "Installation failed: $($_.Exception.Message)"
        }
    }
    if (-not (Test-Path -LiteralPath $exe)) { throw "MeshCompute is not installed at $exe" }
}

function Show-FriendCommands([string]$invite) {
    Write-Host ''
    Write-Host "Send this to your friend privately (works once, expires in $ttl):" -ForegroundColor Cyan
    Write-Host ''
    Write-Host 'Windows: extract meshcompute-windows-amd64.zip, double-click win.bat, choose 2, paste:'
    Write-Host "  $invite"
    Write-Host ''
    Write-Host 'Linux: extract meshcompute-linux-amd64.tar.gz and run in that folder:'
    Write-Host "  ./linux.sh join $invite"
    Write-Host ''
    Write-Host "Download for your friend: $releases"
    try { Set-Clipboard -Value $invite; Write-Note 'The key was copied to your clipboard.' } catch { }
}

# Whether the group host may open a shell on this PC is this PC's owner's
# decision. The menu asks (default: no); the command line never prompts and
# uses the environment variable MESHCOMPUTE_ALLOW_TERMINAL=1 or 0 instead.
function Get-TerminalChoice([bool]$ask) {
    $preset = [string]$env:MESHCOMPUTE_ALLOW_TERMINAL
    if (@('1', 'yes', 'true') -contains $preset.ToLowerInvariant()) { return @('--allow-terminal') }
    if (@('0', 'no', 'false') -contains $preset.ToLowerInvariant()) { return @('--no-terminal') }
    if (-not $ask) { return @() }
    Write-Host 'The group host can be allowed to open a terminal (PowerShell as your user) on this PC.'
    Write-Host 'Say yes only for your own laptops or a host you fully trust.'
    $answer = [string](Read-Host 'Allow a remote terminal on this PC? [y/N]')
    if (@('y', 'yes') -contains $answer.Trim().ToLowerInvariant()) { return @('--allow-terminal') }
    return @('--no-terminal')
}

function Start-Join([string]$value, [bool]$ask) {
    Write-Step 'Joining the group (this PC creates its own key pair automatically).'
    $options = @(Get-TerminalChoice $ask)
    if ($value) {
        & $exe join @options $value.Trim()
    } else {
        # meshcompute reads the pasted key itself (no Read-Host length limit)
        # and offers "Enter = reconnect" if this PC already joined.
        & $exe join @options
    }
}

function Start-Reconnect {
    Write-Step 'Reconnecting to your group. Keep this window open; Ctrl-C stops.'
    & $exe start
}

function Test-MeshGroup {
    $config = Join-Path $hostHome 'config.json'
    return (Test-Path -LiteralPath $config) -and [bool](Select-String -LiteralPath $config -Pattern '"mesh"' -Quiet)
}

function Start-Host([string]$mode) {
    Write-Step "Generating a key and hosting your group (state: $hostHome)"
    Write-Host 'Your browser opens the dashboard: every laptop''s GPUs/VRAM, Invite a friend, Pooled GPUs and a terminal.'
    Write-Host 'Keep this window open. Press Ctrl-C to stop the group.'
    Write-Host ''
    if ($mode -eq 'mesh' -or $relay -eq 'mesh') {
        $target = if ($env:MESHCOMPUTE_MESH) { $env:MESHCOMPUTE_MESH } else { 'auto' }
        Write-Host 'Hosting on your Tailscale / WireGuard address: every computer on the same VPN can join directly.'
        Write-Note 'If Windows Firewall asks, click "Allow access". For WireGuard on Windows, pass the VPN address: set MESHCOMPUTE_MESH=10.x.y.z'
        & $exe --home $hostHome host --mesh $target --ttl $ttl --open
    } elseif ($mode -ne 'relay' -and (Test-MeshGroup)) {
        Write-Host 'This group is hosted on your mesh VPN. (Move it to the public relay with: win.bat host relay)'
        & $exe --home $hostHome host --ttl $ttl --open
    } elseif ($relay) {
        Write-Host "Friends connect through the public relay $relay - works from any network."
        & $exe --home $hostHome host --relay $relay --ttl $ttl --open
    } else {
        Write-Note 'If Windows Firewall asks, click "Allow access" so friends can connect.'
        & $exe --home $hostHome host --direct --auto --ttl $ttl --open
    }
}

function New-Invitation {
    if (-not (Test-Path -LiteralPath (Join-Path $hostHome 'identity.json'))) { throw 'You are not hosting a group yet. Choose 1 (Generate a key) first.' }
    $invite = (& $exe --home $hostHome pair --ttl $ttl | Select-Object -Last 1)
    if ((Get-ExitCode) -ne 0 -or -not $invite) { throw 'Could not create a key.' }
    Show-FriendCommands ([string]$invite).Trim()
    Write-Note 'Your host window (option 1) must be running for your friend to connect.'
}

# The GPU backend runs in its own window so the worker window stays usable.
function Start-GpuSetup {
    $script = Join-Path $root 'scripts\windows-gpu-setup.ps1'
    if (-not (Test-Path -LiteralPath $script)) { throw "GPU setup script missing: $script" }
    if (-not (Get-Command nvidia-smi.exe -ErrorAction SilentlyContinue)) {
        Write-Note 'nvidia-smi was not found. Install the latest NVIDIA driver first; without an NVIDIA GPU the model cannot run on the GPU.'
    }
    Write-Step 'Opening the GPU setup in a new window (downloads llama.cpp, about 680 MB once).'
    Start-Process -FilePath 'powershell.exe' -ArgumentList @('-NoLogo', '-NoProfile', '-NoExit', '-ExecutionPolicy', 'Bypass', '-File', "`"$script`"")
    Write-Host 'When it finishes, use option 2 or 3 here: this PC then lends its GPU to the group automatically.'
}

$fromMenu = $false
try {
    Install-MeshCompute
    $version = (& $exe version | Select-Object -First 1)

    if (-not $Action) {
        Write-Host ''
        Write-Host "MeshCompute - share GPUs between laptops   (version $version)" -ForegroundColor White
        Write-Host ''
        Write-Host '  1) Generate a key   - host a group, get a key for friends, open the dashboard'
        Write-Host '  2) Join with a key  - paste the key a friend sent you'
        Write-Host '  3) Reconnect        - this PC already joined; connect again'
        Write-Host '  4) New key          - another key for the group you are hosting'
        Write-Host '  5) Set up GPU AI    - install llama.cpp so this PC can lend its GPU (or pool GPUs as host)'
        Write-Host '  6) Host on a VPN    - use your Tailscale / WireGuard address instead of the relay'
        Write-Host '  7) Quit'
        Write-Host ''
        $choice = [string](Read-Host 'Choose 1-7')
        $Action = @{ '1' = 'host'; '2' = 'join'; '3' = 'reconnect'; '4' = 'invite'; '5' = 'gpu'; '6' = 'mesh' }[$choice.Trim()]
        if (-not $Action) { exit 0 }
        $fromMenu = $true
    }

    # Functions are called as statements so meshcompute's output streams
    # straight to this window; its exit code is read afterwards.
    $global:LASTEXITCODE = 0
    switch ($Action.ToLowerInvariant()) {
        'host' { Start-Host $Code }
        'mesh' { Start-Host 'mesh' }
        'join' { Start-Join $Code $fromMenu }
        'reconnect' { Start-Reconnect }
        'start' { Start-Reconnect }
        'invite' { New-Invitation }
        'key' { New-Invitation }
        'gpu' { Start-GpuSetup }
        'install' { Write-Host "MeshCompute $version is installed at $exe" }
        default { throw "Unknown option '$Action'. Use: host, mesh, join KEY, reconnect, invite, gpu, install." }
    }
    exit (Get-ExitCode)
} catch {
    Write-Host ''
    Write-Host "Error: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}

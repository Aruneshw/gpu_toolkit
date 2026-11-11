#!/usr/bin/env bash
# MeshCompute one-step launcher for Linux.
#
#   ./linux.sh            menu: host a group / join with an invitation / ...
#   ./linux.sh host       host a group: prints an invitation, opens the dashboard
#   ./linux.sh mesh       host on your Tailscale / WireGuard address instead of the relay
#   ./linux.sh gpu        install llama.cpp so this computer can lend or pool GPUs
#   ./linux.sh invite     new invitation for the group you are hosting
#   ./linux.sh join CODE  join a friend's group with their invitation
#   ./linux.sh reconnect  reconnect this computer to the group it joined
#
# Group state lives in your home directory (never on exFAT/USB drives):
#   host:   $MESHCOMPUTE_HOST_HOME   (default ~/.meshcompute-auto)
#   worker: $MESHCOMPUTE_WORKER_HOME (default ~/.meshcompute-worker)
set -euo pipefail

REPO="Aruneshw/gpu_toolkit"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOST_HOME="${MESHCOMPUTE_HOST_HOME:-$HOME/.meshcompute-auto}"
WORKER_HOME="${MESHCOMPUTE_WORKER_HOME:-$HOME/.meshcompute-worker}"
PORT="${MESHCOMPUTE_PORT:-7443}"
TTL="${MESHCOMPUTE_TTL:-1h}"
# Public TCP relay so friends on any network can connect; set
# MESHCOMPUTE_RELAY=none to host directly on this computer's IP instead.
RELAY="${MESHCOMPUTE_RELAY-bore.pub}"
case "$RELAY" in none|off|direct) RELAY="" ;; esac

if [ -t 1 ]; then
  B=$'\e[1m'; G=$'\e[32m'; Y=$'\e[33m'; R=$'\e[31m'; C=$'\e[36m'; N=$'\e[0m'
else
  B=""; G=""; Y=""; R=""; C=""; N=""
fi
say()  { printf '%s\n' "$*"; }
info() { printf '%s==>%s %s\n' "$G" "$N" "$*"; }
warn() { printf '%s!!%s  %s\n' "$Y" "$N" "$*"; }
die()  { printf '%sError:%s %s\n' "$R" "$N" "$*" >&2; exit 1; }

arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "unsupported CPU $(uname -m); build from source with: make build" ;;
  esac
}

# Download the latest published release for this CPU and verify its SHA-256.
download() {
  local a dir tmp url
  a="$(arch)"
  dir="$HOME/.local/share/meshcompute"
  tmp="$(mktemp -d)"
  url="https://github.com/$REPO/releases/latest/download"
  info "Downloading MeshCompute (linux-$a) from GitHub releases..." >&2
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$tmp/m.tgz" "$url/meshcompute-linux-$a.tar.gz" && curl -fsSL -o "$tmp/SHA256SUMS" "$url/SHA256SUMS"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$tmp/m.tgz" "$url/meshcompute-linux-$a.tar.gz" && wget -q -O "$tmp/SHA256SUMS" "$url/SHA256SUMS"
  else
    die "install curl or wget, or download the release tarball manually"
  fi || die "download failed; check your internet connection"
  local want got
  want="$(awk -v f="meshcompute-linux-$a.tar.gz" '$2==f {print $1}' "$tmp/SHA256SUMS")"
  got="$(sha256sum "$tmp/m.tgz" | awk '{print $1}')"
  [ -n "$want" ] && [ "$want" = "$got" ] || die "checksum mismatch for the downloaded release; not installing"
  mkdir -p "$dir"
  tar -xzf "$tmp/m.tgz" -C "$tmp" meshcompute
  install -m 0755 "$tmp/meshcompute" "$dir/meshcompute"
  rm -rf "$tmp"
  info "Installed $dir/meshcompute (checksum verified)" >&2
  echo "$dir/meshcompute"
}

# In a source checkout with Go installed, always rebuild first (Go's build
# cache makes this take about a second), so a stale bin/meshcompute from an
# older version can never be used.
build_from_source() {
  local v
  v="$(git -C "$HERE" describe --tags --always --dirty 2>/dev/null || echo dev)"
  info "Building MeshCompute from source ($v)..." >&2
  # Build beside the old file and swap it in, whatever bin/meshcompute holds.
  (cd "$HERE" && mkdir -p bin && go build -trimpath -ldflags "-X main.version=$v" -o bin/.meshcompute.new ./cmd/meshcompute &&
    mv -f bin/.meshcompute.new bin/meshcompute) >&2 ||
    die "build failed; run 'go build ./...' in $HERE to see why"
  echo "$HERE/bin/meshcompute"
}

# A binary is current when it knows the newest launcher flag (host --mesh).
current() { "$1" help 2>/dev/null | grep -q -- '--mesh'; }

find_binary() {
  local a c
  a="$(arch)"
  if [ -f "$HERE/go.mod" ] && [ -d "$HERE/cmd/meshcompute" ] && command -v go >/dev/null 2>&1; then
    build_from_source; return
  fi
  for c in "$HERE/meshcompute" "$HERE/bin/meshcompute" "$HERE/dist/meshcompute-linux-$a" \
           "$HOME/.local/share/meshcompute/meshcompute" "$HOME/.local/bin/meshcompute"; do
    if [ -x "$c" ] && [ ! -d "$c" ] && current "$c"; then echo "$c"; return; fi
  done
  if command -v meshcompute >/dev/null 2>&1 && current "$(command -v meshcompute)"; then
    command -v meshcompute; return
  fi
  # Nothing current on this computer: fetch the latest verified release.
  download
}

BIN="$(find_binary)"
mc() { "$BIN" "$@"; }

friend_commands() {
  local code="$1"
  say ""
  say "${B}Send ONE of these to your friend privately (works once, expires in $TTL):${N}"
  say ""
  say "${C}Windows${N}: extract meshcompute-windows-amd64.zip, double-click win.bat, choose 2, paste:"
  say "  $code"
  say ""
  say "${C}Linux${N} (extract meshcompute-linux-amd64.tar.gz, open a terminal in that folder):"
  say "  ./linux.sh join $code"
  say ""
  say "Download for your friend: https://github.com/$REPO/releases/latest"
}

firewall_hint() {
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
    warn "ufw firewall is active. If your friend cannot connect, allow the port: sudo ufw allow $PORT/tcp"
  fi
}

# True when the existing group was created on a mesh VPN address.
on_mesh() { [ -f "$HOST_HOME/config.json" ] && grep -q '"mesh"' "$HOST_HOME/config.json"; }

do_host() {
  local mode="${1:-}"
  local reach=(--relay "$RELAY")
  if [ "$mode" = mesh ] || [ "$RELAY" = mesh ]; then
    reach=(--mesh "${MESHCOMPUTE_MESH:-auto}" --port "$PORT")
  elif [ "$mode" != relay ] && on_mesh; then
    reach=()   # the group lives on a VPN address: stay there
    say "   This group is hosted on your mesh VPN. (Move it to the public relay with: ./linux.sh host relay)"
  elif [ -z "$RELAY" ]; then
    reach=(--direct --auto --port "$PORT")
    firewall_hint
  fi
  info "Starting your group host (state: $HOST_HOME)"
  if [ "${reach[0]:-}" = --mesh ]; then
    say "   Hosting on your Tailscale / WireGuard address: every computer on the same VPN can join directly."
  elif [ "${reach[0]:-}" = --relay ]; then
    say "   Friends connect through the public relay $RELAY: works from any network."
  fi
  say "   The dashboard opens in your browser: live GPUs/VRAM of every laptop,"
  say "   ${B}Invite a friend${N}, ${B}Pooled GPUs${N} (one model across all GPUs), and a terminal"
  say "   for this computer and for friends' laptops that allowed it."
  say "   Keep this window open. Press Ctrl-C to stop the group."
  say ""
  exec "$BIN" --home "$HOST_HOME" host "${reach[@]}" --ttl "$TTL" --open
}

do_invite() {
  [ -f "$HOST_HOME/identity.json" ] || die "you are not hosting a group yet; run: ./linux.sh host"
  local code
  code="$(mc --home "$HOST_HOME" pair --ttl "$TTL")"
  friend_commands "$code"
  say ""
  warn "The group host must be running (./linux.sh host) for your friend to connect."
}

# Whether the group host may open a shell on this computer is this computer's
# owner's decision. MESHCOMPUTE_ALLOW_TERMINAL=1/0 answers without a prompt;
# otherwise a person at the keyboard is asked, and the default is no.
terminal_choice() {
  case "${MESHCOMPUTE_ALLOW_TERMINAL:-}" in
    1|yes|true) echo --allow-terminal; return ;;
    0|no|false) echo --no-terminal; return ;;
  esac
  [ -t 0 ] && [ -t 1 ] || return 0
  local answer
  printf '%s\n' "The group host can be allowed to open a terminal (a shell as your user) on this computer." >&2
  printf '%s\n' "Say yes only for your own laptops or a host you fully trust." >&2
  read -r -p "Allow a remote terminal on this computer? [y/N]: " answer || answer=""
  case "$answer" in y|Y|yes|YES) echo --allow-terminal ;; *) echo --no-terminal ;; esac
}

do_join() {
  local code="${1:-}" term
  info "Joining the group (a new key pair is generated on this computer automatically)."
  term="$(terminal_choice)"
  if [ -z "$code" ]; then
    # meshcompute prompts for the invitation itself, and offers
    # "Enter = reconnect" when this computer already joined a group.
    exec "$BIN" --home "$WORKER_HOME" join $term
  fi
  exec "$BIN" --home "$WORKER_HOME" join $term "$code"
}

do_gpu() {
  local script="$HERE/scripts/linux-gpu-setup.sh"
  [ -f "$script" ] || die "GPU setup script missing: $script"
  MESHCOMPUTE_BIN="$BIN" MESHCOMPUTE_HOST_HOME="$HOST_HOME" exec bash "$script"
}

do_reconnect() {
  [ -f "$WORKER_HOME/identity.json" ] || die "this computer has not joined a group yet; choose 'Join a group'"
  info "Reconnecting to your group. Keep this window open; Ctrl-C stops."
  exec "$BIN" --home "$WORKER_HOME" start
}

menu() {
  say ""
  say "${B}MeshCompute${N} — share GPUs between laptops   ($("$BIN" version 2>/dev/null || echo dev))"
  say ""
  say "  ${B}1${N}) Host a group     — generate an invitation key and open the dashboard"
  say "  ${B}2${N}) Join a group     — paste the invitation key a friend sent you"
  say "  ${B}3${N}) Reconnect        — this computer already joined; connect again"
  say "  ${B}4${N}) New invitation   — another key for the group you are hosting"
  say "  ${B}5${N}) Host on a VPN    — use your Tailscale / WireGuard address instead of the relay"
  say "  ${B}6${N}) Set up GPU AI    — install llama.cpp so this computer can lend or pool GPUs"
  say "  ${B}7${N}) Quit"
  say ""
  local choice
  read -r -p "Choose 1-7: " choice || exit 0
  case "$choice" in
    1) do_host ;;
    2) do_join ;;
    3) do_reconnect ;;
    4) do_invite ;;
    5) do_host mesh ;;
    6) do_gpu ;;
    *) exit 0 ;;
  esac
}

case "${1:-}" in
  host) do_host "${2:-}" ;;
  mesh|vpn|tailscale|wireguard) do_host mesh ;;
  gpu) do_gpu ;;
  invite|key|pair) do_invite ;;
  join) do_join "${2:-}" ;;
  reconnect|start) do_reconnect ;;
  "") menu ;;
  -h|--help|help) sed -n '2,15p' "$0" ;;
  *) die "unknown option '$1' (use: host, mesh, invite, join CODE, reconnect, gpu)" ;;
esac

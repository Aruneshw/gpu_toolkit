#!/usr/bin/env bash
# MeshCompute one-step launcher for Linux.
#
#   ./linux.sh            menu: host a group / join with an invitation / ...
#   ./linux.sh host       host a group: prints an invitation, opens the dashboard
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
  trap 'rm -rf "$tmp"' RETURN
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
  info "Installed $dir/meshcompute (checksum verified)" >&2
  echo "$dir/meshcompute"
}

find_binary() {
  local a c
  a="$(arch)"
  for c in "$HERE/meshcompute" "$HERE/bin/meshcompute" "$HERE/dist/meshcompute-linux-$a" \
           "$HOME/.local/share/meshcompute/meshcompute" "$HOME/.local/bin/meshcompute"; do
    if [ -x "$c" ] && [ ! -d "$c" ]; then echo "$c"; return; fi
  done
  if command -v meshcompute >/dev/null 2>&1; then command -v meshcompute; return; fi
  if [ -f "$HERE/go.mod" ] && command -v go >/dev/null 2>&1; then
    info "Building MeshCompute from source..." >&2
    (cd "$HERE" && go build -trimpath -o bin/meshcompute ./cmd/meshcompute) >&2
    echo "$HERE/bin/meshcompute"; return
  fi
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

do_host() {
  info "Starting your group host on port $PORT (state: $HOST_HOME)"
  firewall_hint
  say "   The dashboard opens in your browser: live GPUs/VRAM of every laptop,"
  say "   ${B}Invite a friend${N} button, and a built-in terminal."
  say "   Keep this window open. Press Ctrl-C to stop the group."
  say ""
  exec "$BIN" --home "$HOST_HOME" host --auto --port "$PORT" --ttl "$TTL" --open
}

do_invite() {
  [ -f "$HOST_HOME/identity.json" ] || die "you are not hosting a group yet; run: ./linux.sh host"
  local code
  code="$(mc --home "$HOST_HOME" pair --ttl "$TTL")"
  friend_commands "$code"
  say ""
  warn "The group host must be running (./linux.sh host) for your friend to connect."
}

do_join() {
  local code="${1:-}"
  info "Joining the group (a new key pair is generated on this computer automatically)."
  if [ -z "$code" ]; then
    # meshcompute prompts for the invitation itself, and offers
    # "Enter = reconnect" when this computer already joined a group.
    exec "$BIN" --home "$WORKER_HOME" join
  fi
  exec "$BIN" --home "$WORKER_HOME" join "$code"
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
  say "  ${B}5${N}) Quit"
  say ""
  local choice
  read -r -p "Choose 1-5: " choice || exit 0
  case "$choice" in
    1) do_host ;;
    2) do_join ;;
    3) do_reconnect ;;
    4) do_invite ;;
    *) exit 0 ;;
  esac
}

case "${1:-}" in
  host) do_host ;;
  invite|key|pair) do_invite ;;
  join) do_join "${2:-}" ;;
  reconnect|start) do_reconnect ;;
  "") menu ;;
  -h|--help|help) sed -n '2,13p' "$0" ;;
  *) die "unknown option '$1' (use: host, invite, join CODE, reconnect)" ;;
esac

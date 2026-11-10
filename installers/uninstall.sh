#!/bin/sh
set -eu
fail() { printf '%s\n' "$*" >&2; exit 1; }
[ "$(uname -s)" = Linux ] || fail 'This uninstaller requires Linux.'
[ "$(id -u)" -ne 0 ] || fail 'Run as a regular user, not root.'
: "${HOME:?HOME must be set}"
binary="$HOME/.local/bin/meshcompute"
[ ! -L "$binary" ] || fail "Refusing symlink: $binary"
[ -e "$binary" ] || { printf '%s\n' 'No installed binary found; state preserved.'; exit 0; }
command -v go >/dev/null || fail 'Go is needed to identify the installed binary safely.'
export GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off
lock="$HOME/.local/bin/.meshcompute-install.lock"
mkdir -- "$lock" 2>/dev/null || fail "Installer lock exists: $lock"
trap 'rmdir -- "$lock"' EXIT
trap 'exit 130' HUP INT TERM
[ -f "$binary" ] || fail "Not a regular file: $binary"
info=$(go version -m "$binary") || fail "Cannot identify $binary"
printf '%s\n' "$info" | grep -Eq '^[[:space:]]+path[[:space:]]+meshcompute/cmd/meshcompute$' || fail "Refusing unrelated binary: $binary"
rm -- "$binary"
printf '%s\n' 'Removed meshcompute only; configuration, state, and backup preserved.'

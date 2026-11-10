#!/bin/sh
set -eu
umask 077
fail() { printf '%s\n' "$*" >&2; exit 1; }
[ "$(uname -s)" = Linux ] || fail 'This installer requires Linux.'
[ "$(id -u)" -ne 0 ] || fail 'Run as a regular user, not root.'
: "${HOME:?HOME must be set}"
command -v go >/dev/null || fail 'Install Go 1.24 or newer first.'
export GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOWORK=off GOFLAGS= CGO_ENABLED=0 GOOS=linux
GOARCH=$(go env GOHOSTARCH); export GOARCH
source_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
bin_dir="$HOME/.local/bin"
mkdir -p -- "$bin_dir"
binary="$bin_dir/meshcompute"
backup="$binary.previous"
lock="$bin_dir/.meshcompute-install.lock"
mkdir -- "$lock" 2>/dev/null || fail "Installer lock exists: $lock"
stage=
backed_up=0
committed=0
cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$backed_up" -eq 1 ] && [ "$committed" -eq 0 ]; then
        mv -- "$backup" "$binary" || printf '%s\n' "Restore $backup to $binary manually." >&2
    fi
    if [ -n "$stage" ]; then
        rm -f -- "$stage/meshcompute"
        rmdir -- "$stage"
    fi
    rmdir -- "$lock"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM
[ ! -L "$binary" ] || fail "Refusing symlink: $binary"
if [ -e "$binary" ]; then
    [ -f "$binary" ] || fail "Not a regular file: $binary"
    info=$(go version -m "$binary") || fail "Cannot identify $binary"
    printf '%s\n' "$info" | grep -Eq '^[[:space:]]+path[[:space:]]+meshcompute/cmd/meshcompute$' || fail "Refusing unrelated binary: $binary"
    [ ! -e "$backup" ] && [ ! -L "$backup" ] || fail "Preserve or remove the reviewed backup first: $backup"
fi
stage=$(mktemp -d "$bin_dir/.meshcompute-stage.XXXXXXXX")
(cd "$source_dir" && go build -trimpath -buildvcs=false -ldflags='-s -w -X main.version=0.1.0-dev' -o "$stage/meshcompute" ./cmd/meshcompute)
"$stage/meshcompute" install
if [ -e "$binary" ]; then
    mv -- "$binary" "$backup"
    backed_up=1
fi
mv -- "$stage/meshcompute" "$binary"
committed=1
printf '%s\n' "Installed $binary. Add $bin_dir to PATH if needed."

#!/usr/bin/env bash
# Browser test of the dashboards: builds MeshCompute, starts a group host and
# two workers on loopback (one asks before running tasks, one runs them
# automatically; both allow terminals), then drives headless Chrome through
# the fleet view, the terminal grid, tasks and the worker's approval page.
# Needs Go, Node 22+ and Chrome (CHROME=/path/to/chrome to override).
#   scripts/ui-test.sh
set -euo pipefail
cd "$(dirname "$0")/.."
command -v node >/dev/null || { echo "node 22+ is required" >&2; exit 2; }
work="$(mktemp -d)"
bin="$work/meshcompute"
pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  # Chrome may still be closing its profile folder.
  for _ in 1 2 3 4 5; do rm -rf "$work" 2>/dev/null && break; sleep 1; done
}
trap cleanup EXIT
go build -o "$bin" ./cmd/meshcompute
mkdir -m 700 "$work/H" "$work/W" "$work/W2"
export MESHCOMPUTE_RELAY=none MESHCOMPUTE_NO_BROWSER=1 SHELL=/bin/bash
port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
hp=$(port); ui=$(port)
"$bin" --home "$work/H" host --listen "127.0.0.1:$hp" --ui "127.0.0.1:$ui" >"$work/host.log" 2>&1 & pids+=($!)
for _ in $(seq 50); do grep -q '^eyJ' "$work/host.log" 2>/dev/null && break; sleep 0.2; done
join() { # dir policy
  local code; code=$("$bin" --home "$work/H" pair --ttl 5m 2>/dev/null | grep '^eyJ' | head -1)
  "$bin" --home "$1" install >/dev/null
  "$bin" --home "$1" config set listen "127.0.0.1:$(port)"
  "$bin" --home "$1" config set share_gpu off
  echo "$code" | MESHCOMPUTE_WORKER_UI="127.0.0.1:$(port)" "$bin" --home "$1" join --stdin --allow-terminal --tasks "$2" >"$1.log" 2>&1 &
  pids+=($!)
}
join "$work/W" ask
join "$work/W2" auto
for _ in $(seq 100); do grep -q 'Connected to the group host' "$work/W.log" 2>/dev/null && grep -q 'Connected to the group host' "$work/W2.log" 2>/dev/null && break; sleep 0.2; done
host_url=$(grep -o 'http://127.0.0.1:[0-9]*/?token=[0-9a-f]*' "$work/host.log" | head -1)
worker_url=$(grep -o 'http://127.0.0.1:[0-9]*/?token=[0-9a-f]*' "$work/W.log" | head -1)
[ -n "$host_url" ] && [ -n "$worker_url" ] || { echo "setup failed"; tail -5 "$work"/*.log; exit 1; }
HAS_NVIDIA=0; command -v nvidia-smi >/dev/null && nvidia-smi >/dev/null 2>&1 && HAS_NVIDIA=1
HAS_NVIDIA=$HAS_NVIDIA node scripts/ui-test.mjs "$host_url" "$worker_url" "$work"

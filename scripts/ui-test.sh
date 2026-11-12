#!/usr/bin/env bash
# Browser test of the dashboards: builds MeshCompute, starts a group host and
# WORKERS workers on loopback (default 5; the first asks before running
# tasks, the others run them automatically; all allow terminals), then drives
# headless Chrome through the fleet view, the terminal grid, tasks and the
# worker's approval page.
# Needs Go, Node 22+ and Chrome (CHROME=/path/to/chrome to override).
# With LLAMA_DIR (a llama.cpp folder with llama-server and ggml-rpc-server)
# and POOL_MODEL (a .gguf file), workers lend their GPU and the test also
# registers the model and runs a pool through the dashboard.
#   scripts/ui-test.sh            WORKERS=8 scripts/ui-test.sh
#   LLAMA_DIR=~/llama POOL_MODEL=~/models/small.gguf scripts/ui-test.sh
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
WORKERS="${WORKERS:-5}"
mkdir -m 700 "$work/H"
export MESHCOMPUTE_RELAY=none MESHCOMPUTE_NO_BROWSER=1 SHELL=/bin/bash
port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
hp=$(port); ui=$(port)
LLAMA_DIR="${LLAMA_DIR:-}"; POOL_MODEL="${POOL_MODEL:-}"
if [ -n "$LLAMA_DIR" ]; then
  "$bin" --home "$work/H" install >/dev/null
  "$bin" --home "$work/H" config set llama_server "$LLAMA_DIR/llama-server"
fi
"$bin" --home "$work/H" host --listen "127.0.0.1:$hp" --ui "127.0.0.1:$ui" >"$work/host.log" 2>&1 & pids+=($!)
for _ in $(seq 50); do grep -q '^eyJ' "$work/host.log" 2>/dev/null && break; sleep 0.2; done
join() { # dir policy [extra join flags]
  mkdir -m 700 "$1"
  local code; code=$("$bin" --home "$work/H" pair --ttl 5m 2>/dev/null | grep '^eyJ' | head -1)
  "$bin" --home "$1" install >/dev/null
  "$bin" --home "$1" config set listen "127.0.0.1:$(port)"
  if [ -n "$LLAMA_DIR" ]; then
    "$bin" --home "$1" config set rpc_server "$LLAMA_DIR/ggml-rpc-server"
  else
    "$bin" --home "$1" config set share_gpu off
  fi
  echo "$code" | MESHCOMPUTE_WORKER_UI="127.0.0.1:$(port)" "$bin" --home "$1" join --stdin --allow-terminal --tasks "$2" ${3:-} >"$1.log" 2>&1 &
  pids+=($!)
}
join "$work/W" ask
# Workers 2 and 3 let others hop through them: they listen on this
# computer's LAN address and appear on the host's network map.
for i in $(seq 2 "$WORKERS"); do
  if [ "$i" -le 3 ]; then join "$work/W$i" auto --allow-hop; else join "$work/W$i" auto; fi
done
connected() { local n=0 f; for f in "$work"/W*.log; do grep -q 'Connected to the group host' "$f" && n=$((n+1)); done; [ "$n" -eq "$WORKERS" ]; }
for _ in $(seq 150); do connected && break; sleep 0.2; done
connected || { echo "not every worker connected"; tail -3 "$work"/W*.log; exit 1; }
host_url=$(grep -o 'http://127.0.0.1:[0-9]*/?token=[0-9a-f]*' "$work/host.log" | head -1)
worker_url=$(grep -o 'http://127.0.0.1:[0-9]*/?token=[0-9a-f]*' "$work/W.log" | head -1)
[ -n "$host_url" ] && [ -n "$worker_url" ] || { echo "setup failed"; tail -5 "$work"/*.log; exit 1; }
HAS_NVIDIA=0; command -v nvidia-smi >/dev/null && nvidia-smi >/dev/null 2>&1 && HAS_NVIDIA=1
POOL_MODEL="$POOL_MODEL" EXPECT_WORKERS=$WORKERS HAS_NVIDIA=$HAS_NVIDIA HOPS=$(( WORKERS >= 3 ? 2 : WORKERS - 1 )) node scripts/ui-test.mjs "$host_url" "$worker_url" "${SHOTS_DIR:-$work}"
# The same map from the command line.
topo=$("$bin" --home "$work/H" topology)
hops=$(grep -c "forwards for others: yes" <<<"$topo" || true)
if [ "$hops" -ge 2 ] && grep -q -- "->.* ms" <<<"$topo"; then echo "PASS topology command lists both forwarding workers and their times"; else echo "FAIL topology command"; echo "$topo"; exit 1; fi

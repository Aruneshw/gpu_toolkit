# Plan: host-controlled worker fleet

Goal: one host dashboard drives every friend's laptop. The host runs open-weight
models from its own disk (for example an external HDD), spreads them over the
workers' GPUs, sends tasks (shell or PowerShell commands) to chosen workers,
which run them after the owner approves, and sees every worker live: a
"digital twin" per laptop and one terminal per worker. Every worker also gets
its own dashboard showing what the host is doing on it.

## What the engine can and cannot do

Pooling uses llama.cpp RPC. Its topology is a star: the host's `llama-server`
exchanges activations with every worker's `rpc-server` for every token.
Workers never pass activations directly to each other. Consequences:

- Layer placement, KV-cache placement and type, and request parallelism are
  all controllable (llama-server flags), so the host can choose them.
- "Multi-hop" can only help a worker reach the host over a better path (for
  example a worker behind the public relay routing through a nearby worker
  that has a direct mesh link). It cannot make worker 2 send layer output
  straight to worker 3; that needs a different engine and is out of scope.
- Per-token latency to each worker dominates speed, not bandwidth, once the
  weights are cached on the worker (`rpc-server -c`).

## Phases

### Phase 1: tasks with worker approval, worker dashboard (done 2026-11-12)
- New tunnel stream `/v1/stream/task`: the host sends a command, shell
  (auto, bash, sh, powershell, cmd) and working directory; the worker streams
  output, state and exit code back; closing the stream kills the process tree.
- Worker policy `task_approval`: `ask` (default), `auto` (trust this group's
  host), `off`. With `ask`, the task waits up to 10 minutes for the owner to
  approve or deny it on the worker dashboard; "always allow" switches to auto.
- Worker dashboard (`join` starts it on 127.0.0.1:8082, `--open` opens it):
  connection state, this laptop's GPU, approval prompts showing the exact
  command, running and finished tasks with live output and a kill button,
  switches for terminal, tasks and GPU sharing.
- Host: Tasks panel. Pick one or many workers, type a command, send. Each
  worker gets its own task with live state (waiting for approval, running,
  succeeded, failed, denied, cancelled) and output. Cancel from the host.
- Audit: every task is logged on both sides.

### Phase 2: live fleet view (digital twin) and terminal grid (done 2026-11-12)
- Telemetry adds GPU temperature, power and utilization history.
- Host "Fleet" view: one live card per worker (GPU utilization, VRAM, temp,
  power, RAM, CPU, link RTT and throughput, pool role, running tasks), updated
  every 2 s over server-sent events, with short history graphs.
- Terminal grid: one terminal per worker (5 workers, 5 terminals). Typing in
  worker 3's terminal runs on worker 3. Optional "type into all selected".
- Session limit raised from 8 to 24.

### Phase 3: pool controls
- Model library: point at a folder (for example the external HDD), list every
  `.gguf` with its size, register with one click.
- Layer split: automatic by free VRAM, or manual weights per device
  (`--tensor-split`); the dashboard shows which layers and how much memory
  landed on each computer (parsed from llama-server's load log).
- KV cache: type (f16, q8_0, q4_0), keep on host RAM (`--no-kv-offload`),
  flash attention, context size.
- Parallelism: request slots (`--parallel`) and several named pools at once,
  each with its own model and its own chosen workers; requests go to a chosen
  pool or round-robin over replicas of the same model.

### Phase 4: faster links
- TCP_NODELAY and larger socket buffers on every tunnel stream (RPC sends many
  small messages per token).
- Continuous RTT and throughput measurement per worker, shown on the fleet
  cards; warnings when a worker would slow the whole pool.
- Prefer a direct mesh (Tailscale/WireGuard) path over the public relay when
  both computers are on the mesh.

### Phase 5: topology and multi-hop routing
- Workers report their LAN and mesh addresses; the host asks pairs of workers
  to measure each other (authenticated ping) and shows a latency map.
- A worker whose path to the host is slow can connect through a nearby worker
  that has a fast path. The relaying worker only forwards encrypted bytes;
  TLS still ends at the host.

## Safety rules kept throughout
- Tasks and terminals run as the worker's user and are off or ask-first by
  default; only the group host's pinned certificate can send them.
- Approval prompts show the exact command; nothing runs silently unless the
  owner chose auto.
- Every dashboard is loopback-only and token-protected.

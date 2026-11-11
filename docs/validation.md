# Validation evidence

Date: 2026-11-06. This file records what was actually executed for this
checkout. It is not a production-readiness or hardware-certification claim.

## Environment

- Linux amd64 host, Go 1.26.8 (module declares Go 1.24).
- NVIDIA RTX 3050 6 GB visible to `nvidia-smi` for resource discovery.
- A prebuilt llama.cpp `b12119` CPU runtime and a licensed LFM3 1.7B
  `Q4_K_M` GGUF were used for real inference. The GPU was not used
  (`--gpu-layers 0`); no CUDA toolkit was installed.
- No second physical host or VPN link was available.
- GitHub Actions also ran the test suite on Ubuntu and Windows runners with Go
  1.24.x and stable (see the CI section below).

## Commands and outcomes

All commands were run from the repository root.

| Command | Outcome |
| --- | --- |
| `gofmt -l .` | no output |
| `go vet ./...` | pass |
| `go test ./... -count=1` | pass (105 test cases including subtests) |
| `go test -race ./... -count=1` | pass |
| `GOPROXY=off GOTOOLCHAIN=local go build ./...` | pass |
| `go test ./... -count=1` with `GOPROXY=off` | pass |
| `GOOS=linux GOARCH=amd64/arm64 go build ./cmd/meshcompute` | pass |
| `GOOS=windows GOARCH=amd64 go build ./cmd/meshcompute` | pass |
| `GOOS=windows GOARCH=amd64 go vet ./...` | pass |
| `GOOS=windows GOARCH=amd64 go test -c ./internal/resource` | pass (Windows test binary compiled) |
| `make dist` | pass; Linux amd64/arm64 and Windows amd64 plus `SHA256SUMS` |
| `sh -n installers/install.sh installers/uninstall.sh` | pass |
| Linux installer: install, `version`, uninstall | pass; binary removed, state preserved |
| Windows binary under Wine: `version`, `install`, `doctor`, `config set`, `cluster create`, `start`, `status` | pass; CPU-only, no PowerShell in Wine |

The repeated TLS dispatch test was also run 20 times to check the oversized-body
path:

```sh
go test ./internal/agent -run TestTwoAgentTLSDispatchFixture -count=20
```

Result: pass.

The streaming and dashboard paths were also stress-run:

```sh
go test -race ./internal/backend ./internal/agent ./internal/dashboard ./internal/sse ./internal/cli -count=3
go test -race ./internal/agent -run TestTwoAgentTLSStreamFixture -count=10
```

Result: pass.

## Continuous integration

GitHub Actions run
[42258567595](https://github.com/Aruneshw/gpu_toolkit/actions/runs/42258567595)
completed successfully on the Windows-compatibility commit. All nine jobs
passed:

- test: Ubuntu and Windows, each on Go 1.24.x and stable; race detector on Linux
- offline build and tests with `GOPROXY=off` and `GOTOOLCHAIN=local`
- cross-compile: Linux amd64/arm64 and Windows amd64
- Linux installer smoke: install, run, uninstall
- Windows: PowerShell installer syntax parse for source and prebuilt installers
- Windows prebuilt installer: install, `version`, `install.json` checksum,
  tamper refusal, forced removal, reinstall, and clean uninstall verified
  through the marker rather than Go build info
- Windows Go tests now exercise the real PowerShell/CIM discovery path for CPU,
  RAM, and disk on the runner

The earlier run of the dashboard series exposed a Windows-Go-1.24-only timing
failure; finding 5 below records the fix. The Windows-compatibility series
exposed the elevation and WMI cold-start issues recorded as findings 6 and 7.

## What the automated tests exercise

- **Security and transport:** coordinator initialization, real HTTPS enrollment,
  single-use invitations under concurrent processes, replay rejection, expired
  and malformed invitations, CA and leaf pinning, foreign-cluster rejection,
  same-CA non-coordinator rejection, worker-to-worker rejection, revocation
  reload on each handshake, fail-closed on corrupt/unsafe state, symlink
  rejection, and local self-access for both roles.
- **Backend and registry:** GGUF magic and full-file SHA-256 registration,
  duplicate/mutation rejection, KV arithmetic and overflow, loopback endpoint
  restrictions, health/model/context checks, tokenizer budget enforcement,
  cancellation, response limits, streaming chunk parsing, missing/invalid final
  chunks, emit-failure propagation, and exact launch arguments with ambient
  `LLAMA_*`/`GGML_RPC*` removal. The automated fixture is an HTTP server and a
  helper subprocess, not a language model.
- **Agent:** real TLS enrollment and dispatch between two state directories on
  loopback for both JSON and SSE, participant attribution, busy rejection,
  missing-model rejection, prompt non-leakage into audit output,
  malformed/trailing/oversized request rejection, stream error events, slot
  release after success/failure, and revocation.
- **Dashboard and SSE:** token bootstrap and cookie, constant-time token checks,
  non-loopback Host and cross-origin POST rejection, security headers, aggregate
  RAM/VRAM/model arithmetic, node status failures, run dispatch fields, strict
  request rejection, streamed token/done/error relay, CRLF/comments, truncated
  streams, and size limits.
- **Resources, scheduler, network, CLI, config:** meminfo/GPU CSV parsing with
  unknown values left unknown, cancelled discovery, disk probing, synthetic-only
  scheduling simulations, stale-worker/unstable-network rejection, explicit
  non-pooling of RAM and VRAM, probe measurement and failure handling, CLI
  parsing, offline install, and strict configuration loading.

## Manual integration smoke tests

1. CLI lifecycle in a temporary home: `install`, `version`, `simulate`,
   `doctor`, `model add/list` with a fixture GGUF, `cluster create`, `pair`,
   and `status`. Expected JSON and file modes (0700 state, 0600 config/identity)
   were observed.
2. Two independent state directories and two agent processes on loopback:
   coordinator `cluster create`, `pair`, worker `join`, both agents started,
   `peer add`, coordinator `status` (both nodes healthy), worker self `status`,
   `network probe local`, then `revoke`. After revocation the worker row became
   unhealthy and authenticated access failed.
3. Local coordinator `start` followed by self `status`: healthy, proving the
   self-identity path. The Linux installer built offline, ran, and uninstalled
   cleanly.
4. Real CPU inference on the Linux host: llama.cpp `b12119`, LFM3 1.7B
   `Q4_K_M`, context 2048, `--gpu-layers 0`. `meshcompute run` returned
   `"Paris, and the population of France is about 67 million..."` at
   6.78 tok/s.
5. Real dashboard streaming on the same stack: `meshcompute ui` printed a
   64-hex-character token URL; requests without a token, with a non-loopback
   Host, or with a bad origin returned 403. `GET /api/status` reported one
   healthy node, one GPU, 6,442,450,944 bytes aggregate VRAM, and the explicit
   “not pooled” scope text. `POST /api/run` streamed 16 `event: token` frames
   followed by `event: done` at 9.80 tok/s.

These use one host and loopback, not a physical two-machine VPN. The GPU was
visible but not used for inference.

## Integration review findings fixed during completion

1. **Local CLI could not reach its own agent.** The client authorized only the
   opposite cluster role, so `status`/`run --node local` failed on both
   coordinator and worker. `Client`, `ServerTLS`, and `Authorize` now also accept
   the node's own leaf certificate, still with CA-chain verification. Added
   `TestLocalSelfAccess`.
2. **Oversized streaming request produced a connection reset** instead of the
   documented 400 response. The agent now reads the bounded body before
   decoding and drains a bounded remainder before replying. The dispatch test
   passes repeatedly.
3. **Windows portability.** Unconditional Unix mode-bit checks and directory
   `fsync` made identity creation fail on Windows. These are now platform
   helpers: Unix keeps the 0700/0600 and fsync requirements; Windows relies on
   inherited user-profile ACLs and a file flush before rename. The full test
   suite now runs on Windows CI, and the prebuilt installer is executed there;
   the source-build `install.ps1` is still syntax-checked only.
4. **Go 1.26 test temporaries.** `testing.T.TempDir()` now creates per-test
   directories as 0775 under the default umask, which the security package
   correctly rejects. Agent tests now create a private child directory, matching
   the security tests.
5. **Coarse probe clock on Windows Go 1.24.** A single 256 KiB loopback probe
   measured as zero, so throughput stayed zero despite a successful transfer.
   The probe now repeats a bounded number of times and divides total bytes by
   aggregate elapsed time. Windows Go 1.24 CI passes after the change.
6. **Tailscale/CGNAT identity mismatch.** `config` accepted RFC 6598
   `100.64.0.0/10` listen addresses, but the TLS identity layer's private-IP
   check used `net.IP.IsPrivate` and rejected them. `privateIP` now accepts the
   CGNAT range explicitly, with `TestPrivateIP` covering accepted and rejected
   boundaries.
7. **PowerShell discovery encoding.** Redirected Windows PowerShell output can
   be UTF-8 with a BOM, UTF-16 LE/BE with a BOM, or unmarked UTF-16LE. JSON
   decoding now normalizes those forms, and the discovery tests cover them.
8. **CI elevation and WMI cold start.** GitHub's Windows runners are elevated,
   so the prebuilt installer's deliberate admin refusal blocked the smoke test;
   an explicit `-AllowElevated` switch is passed only by CI, and the default
   still refuses. A 5-second discovery timeout also killed the first
   `Get-CimInstance` call on a cold runner; hardware probing now allows 20
   seconds and reports timeouts/stderr instead of a bare exit status.

## Security review notes retained as limitations

- There is no rate limiting, audit-log shipping, certificate rotation/renewal,
  CRL/OCSP, HA consensus, or remote revocation API. Those are deployment
  responsibilities.
- A crashed writer can leave `.lock`; locks are never stolen automatically.
- Revocation does not cancel an already-authorized in-flight request or stream.
- Exact coordinator pinning means replacing the coordinator certificate requires
  explicit worker reprovisioning.
- The scheduler is a planning/admission check only. It does not reserve memory,
  pool RAM and VRAM, or execute RPC. KV estimates are architecture-specific and
  must be supplied from verified metadata.
- A peer named `local` is currently accepted by configuration but shadows the
  built-in local endpoint in `peer` lookups. Use a different peer name.
- The loopback network probe can report throughput below the default 50 MB/s
  threshold on a busy host; this only annotates a recommendation and does not
  enable RPC or change placement mid-generation.

## Not validated

- CUDA execution, GPU layer offload, or measured VRAM behavior. CPU-only
  inference was executed; the RTX 3050 was detected but unused.
- Real inference over a remote node or WAN link.
- A physical Windows machine, Windows GPU/CUDA execution, or Windows firewall
  behavior. CI runs the Windows Go tests and the prebuilt installer; Wine runs
  the Windows binary for CLI lifecycle checks. Neither is GPU or physical-host
  validation, and the source-build `install.ps1` is not executed in CI.
- A physical two-machine VPN deployment, firewall behavior, or WAN reliability.
- macOS, AMD, or Intel GPU targets.
- Pooled inference with real llama.cpp binaries (see 2026-11-11 below for what
  was and was not run).

## 2026-11-10: address moves, re-join, dashboard invitations and terminal

Executed on the Linux development laptop (Parrot, RTX 3050 6 GB):

- `go test ./...`, `go test -race ./...`, `make verify` (gofmt, vet, Linux
  amd64/arm64 and Windows cross-builds, offline build): all pass.
- New tests: `TestHostAddressChangeAndRejoin` (host moves to a new IP, old pin
  and old invitations rejected, worker re-joins with backup and reconnects),
  `TestShellRoundTrip`/`TestCloseKillsShellAndLimits`/`TestScrollbackOffsets`
  (real PTY), `TestInviteAPI`, `TestStaticAndInfo`, `TestTerminalOverHTTP`.
- Live run with real processes: group created on `127.0.0.1`, worker joined,
  host restarted with `--auto` and moved itself to the laptop's current global
  IPv6 (NOTICE printed instead of the old "use a new --home" failure). The old
  worker printed "cannot reach the group host". An invitation generated via
  `POST /api/invite` was used with `join <CODE>`: the old identity was backed
  up, the worker connected, and the dashboard showed 2/2 nodes, 2 GPUs,
  12.0 GiB VRAM. Reusing the invitation failed with an explicit
  "ask for a NEW invitation" message. Restarting the host kept the address and
  the worker reconnected by itself.
- Headless Chrome against the live dashboard: invite panel and countdown,
  copy-ready commands, xterm terminal with the user's bash prompt,
  `TERM=xterm-256color`, colour output, Ctrl-C (exit 130), the *Status* quick
  button, session restored with scrollback after a page reload, and no console
  errors.

Not verified here: the PowerShell installer changes (`join.cmd <CODE>`,
bundled-binary upgrade, replacing an older `.previous` backup) were not run on
real Windows, and a second physical computer on another network was not used.

### Launcher scripts (2026-11-10)

- `linux.sh` was run from an extracted `dist` tarball: the menu's *Host a group*
  resumed the group on the live IPv6, printed the invitation and asked the
  desktop to open the dashboard (`--open`, via a recording `xdg-open`). *New
  invitation* printed copy-ready Windows/Linux commands. *Join a group* with a
  pasted whole `.\join.cmd <KEY>` line enrolled and connected; *Reconnect*
  reconnected; unknown options and a missing identity gave clear errors.
- The first Windows menu (`windows.cmd`) failed on a friend's PC during the
  upgrade step: `File.Replace(..., $null)` under Windows PowerShell 5.1 raised
  "The path is not of a legal form" (PowerShell passes `$null` as `""`).
  Fixed in `install-prebuilt.ps1`; `windows.cmd` was replaced by `win.bat`,
  and CI now runs install, two upgrades, host, invite and join through
  `win.bat` with Windows PowerShell 5.1 on a real Windows runner.

### Public relay (2026-11-10)

Cause of the friend's `dial tcp [2409:…]:7443: i/o timeout`: the host was bound
to an IPv6 address the kernel had already marked deprecated. The Jio hotspot
rotates the /64 prefix every few minutes (8 prefixes in about two hours; 7 with
`preferred_lft 0`), so a directly advertised IPv6 address stops being routed
within minutes. `host --auto` now skips deprecated addresses, and hosting uses
the bore relay by default.

- `internal/relay` implements the bore v0.5 client protocol in Go; tests run a
  protocol-faithful fake server (grant, heartbeat, visitor forwarding, port in
  use, reconnect reclaiming the same port).
- Live through bore.pub from this laptop: host registered `bore.pub:57745`,
  a separate worker identity joined via `https://159.223.110.159:57745`
  (IPv4), connected, and the dashboard showed 2/2 nodes and 2 GPUs. Restarting
  the host reclaimed the same port with no certificate change, and the worker
  reconnected by itself. A busy local port fell back to the previous one.
- A worker whose local port is busy now runs outbound-only
  (`TestWorkerConnectsWhenLocalPortBusy`).
- Not verified: the friend's Windows PC joining through the relay (pending
  their next attempt with a new key).

## 2026-11-11: mesh VPN hosting, remote terminal, pooled GPUs

Executed on the Linux development laptop (Parrot, kernel 7.2.11, RTX 3050 6 GB):

- `go test ./... -count=1` and `go test -race` on the changed packages pass.
  New tests: `TestPoolAndRemoteTerminalEndToEnd` (a worker lends its "GPU",
  the host starts one server whose `--rpc` endpoints are loopback forwarders
  into the worker's TLS tunnel, inference runs through it, the dashboard path
  opens a shell on the worker, the pool recovers alone when the worker leaves,
  a worker certificate cannot control the pool);
  `TestRemoteTerminalNeedsWorkerConsent`; `TestRemoteShellOverStream`.
  The two llama.cpp programs are stand-ins built into the test binary (an echo
  server and an HTTP fixture that verifies every `--rpc` endpoint), so these
  tests prove the plumbing, not llama.cpp.
- **WireGuard, real tunnel.** Two network namespaces joined by a veth pair,
  kernel WireGuard between them (`wg` from Debian's wireguard-tools). `host
  --mesh auto` detected `wg0` and hosted on `10.66.0.1:7443`; the worker in
  the other namespace joined through the tunnel; the dashboard opened a
  terminal on it; the host port was not reachable on the underlay address;
  a restart without flags stayed on the mesh address; with the interface down
  the host refused to move the group.
- **Remote terminal, live.** Host and worker as separate processes on
  loopback: `/api/term/open` with the worker's ID, input, streamed output and
  exit code 4 returned through the tunnel; the worker window printed the
  session notice.
- **Found and fixed:** `nvidia-smi` takes about 5.5 s per call on this
  machine. Every status request ran it, so host, worker and pool polls timed
  each other out. The agent now caches discovery (one run at a time, refreshed
  in the background).
- Static check of the pinned llama.cpp release b12182 (not executed): the
  Linux archive contains `llama-server` and `ggml-rpc-server`, whose option
  strings include `--host`, `--port`, `-c/--cache` and `--rpc`.

- **Real llama.cpp pool (CPU build), 2026-11-11 evening.** `scripts/linux-gpu-setup.sh`
  installed b12182 (checksum verified; CPU variant, as this laptop has no
  Vulkan loader). Host and worker as separate processes with their own state
  directories; the worker joined with `--allow-terminal` and started
  `ggml-rpc-server` on loopback. `pool start smol` (SmolLM2-135M Q8_0) reached
  `ready`: `llama-server ... --rpc 127.0.0.1:<forwarder>`, with sockets
  showing llama-server -> host forwarder -> TLS tunnel -> worker ->
  `ggml-rpc-server`, and the RPC server holding 29 MB of the model's tensors.
  `meshcompute run smol` and a direct `/completion` both generated text at
  about 25 tokens/s. Through `/api/term/open` with the worker's ID, the shell's
  parent process was the worker agent; output, resize (30x100 to 40x120) and
  exit code 3 came back through the tunnel.
- `make verify` (gofmt, vet, tests, cross-builds, offline build) and
  `go test -race ./...` pass; the agent package passed three repeated runs.
- Mesh detection in a user network namespace: `wg0` (10.8.0.1) was reported
  as WireGuard and `tailscale0` (100.101.102.103) as Tailscale; `host --mesh
  tailscale` and `host --mesh wireguard` each hosted on the matching address.

Not run:

- **The pool on two physical GPUs.** The run above used the CPU build on one
  laptop. A GPU run needs the Vulkan (or CUDA) build on both computers:
  `./linux.sh gpu` / `win.bat` option 5 on each, then *Start pool*.
- **The Tailscale client itself.** Detection is by interface name, device type
  and the 100.64.0.0/10 / fd7a:115c:a1e0::/48 ranges; it was exercised with
  real WireGuard and a stand-in `tailscale0` interface, not a running
  `tailscaled`.
- **Windows remote terminal.** The ConPTY backend cross-compiles and has a
  Windows-only test (`TestWindowsShellRoundTrip`) for CI. Under Wine the
  pseudo-console starts but Wine does not bind the child's output to it, so
  it could not be confirmed locally.
- `scripts/linux-gpu-setup.sh` and the Windows script changes: syntax-checked
  (`bash -n`) only.

## 2026-11-12: tasks with worker approval, worker dashboard

- New package `internal/task`; tests (race detector): auto-run with output,
  non-zero exit, approve and deny, cancel kills the process tree, withdraw
  while waiting, output resumed without loss or duplication after a dropped
  connection, a restarted worker never re-runs a task it forgot.
- `TestTasksEndToEnd` (real agents, mutual TLS, both dashboards over HTTP):
  host sends two tasks; the worker dashboard shows both waiting; the host
  cannot approve; the owner approves one and denies the other; host and worker
  output match; "Always allow" saves `task_approval=auto`; the host cancels a
  running task; with tasks off the host is refused; a worker certificate
  cannot reach the task route.
- Live, real binaries on one laptop (host and worker as separate processes):
  the worker printed and served its dashboard on 127.0.0.1:8082; two tasks
  sent through the host API showed as waiting with their exact commands
  (headless Chrome screenshot); one approved (output `499999500000`, exit 0),
  one denied; the host dashboard showed both results.
- `make verify` and `GOOS=windows go vet ./...` pass.

Not run: tasks on a real Windows worker (PowerShell and `taskkill` process
tree handling compile but are untested), and clicking the buttons in a real
browser (the buttons call the same endpoints the test uses).

## 2026-11-12: live fleet view and terminal grid

- Telemetry: `nvidia-smi` now also reports temperature, power draw and power
  limit (old 6-column output still parsed); CPU load from `/proc/stat` on
  Linux and `LoadPercentage` on Windows. Unit tests for both parsers.
- Dashboard tests (race detector, three runs): fleet sampling (host never
  pinged, offline worker keeps its history and last-seen time, no resample
  within 1.5 s, history capped at 90 samples); three local shells over one
  multiplexed stream with broadcast input, per-session output and exit codes,
  unknown session reported. One real bug found and fixed: sampling before the
  first page request crashed on an uninitialized map.
- `TestFleetAndTerminalGridEndToEnd`: a host and two real workers over mutual
  TLS. Fleet shows both with measured round trips and the running task on the
  right card; one shell per worker; input typed for worker A never reaches
  worker B; "type into all" reaches both; exit codes 5 and 6 arrive over one
  stream.
- Browser (`scripts/ui-test.sh`, headless Chrome driven over the DevTools
  protocol, real binaries, host plus two workers): 36 checks pass, including
  real keystrokes in the grid panes, the two panes running on two different
  workers (each shell reports its parent process's state folder), reload
  re-attaching both panes with their output, *Send task* from a card, and
  clicking *Approve* on the worker's own page. No JavaScript errors. Added to
  CI as the "dashboards in headless Chrome" job.

Notes: on loopback the measured round trip through the agent and tunnel is
about 14 to 19 ms; that per-request overhead is the target of phase 4.
Not run: the grid with a Windows worker (ConPTY), and a group larger than two
workers (the session limit is now 24).
